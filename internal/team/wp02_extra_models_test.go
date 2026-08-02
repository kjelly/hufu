package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

// TestCloneCoordinator_SharesContractWarningDedup verifies that
// cloneCoordinator shares the contractWarningDedup set by pointer, so an
// isolated coordinator (extra-models) does not re-emit contract_warning
// events for the same todoID that the parent already emitted (reviewer P2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCloneCoordinator_SharesContractWarningDedup(t *testing.T) {
	var warnCount int
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			warnCount++
		}
	})
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	findings := ValidateExecutionContractFull(task, agent.VerifierLintWarn).Findings

	// Parent emits the warning.
	c.emitContractWarnings("1", findings)
	if warnCount != 1 {
		t.Fatalf("expected 1 contract_warning from parent, got %d", warnCount)
	}

	// Clone the coordinator (simulating extra-models isolation). The clone
	// must share the parent's dedup set so the same todoID does not re-emit.
	clone := cloneCoordinator(c, c.session)
	if clone.contractWarnings == nil {
		t.Fatal("cloned coordinator has nil contractWarnings — dedup not shared")
	}
	if clone.contractWarnings != c.contractWarnings {
		t.Fatal("cloned coordinator does not share the parent's contractWarnings pointer")
	}

	// The clone attempts to emit the same warning for the same todoID.
	// Because the dedup set is shared, this must NOT emit a second event.
	clone.emitContractWarnings("1", findings)
	if warnCount != 1 {
		t.Errorf("expected dedup to suppress duplicate from clone, got %d contract_warning events", warnCount)
	}

	// A different todoID still emits (different task dispatch).
	clone.emitContractWarnings("2", findings)
	if warnCount != 2 {
		t.Errorf("expected 2 contract_warning events for different todoIDs, got %d", warnCount)
	}
}

// TestCloneCoordinator_NilContractWarningsInitializesShared verifies that when
// the parent has a nil contractWarnings (e.g., never emitted), cloneCoordinator
// initializes a shared set and both parent and clone use it.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCloneCoordinator_NilContractWarningsInitializesShared(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}
	if c.contractWarnings != nil {
		t.Fatal("precondition: parent contractWarnings should be nil before first use")
	}

	// cloneCoordinator must initialize a shared set when the parent is nil.
	clone := cloneCoordinator(c, c.session)
	if clone.contractWarnings == nil {
		t.Fatal("cloned coordinator should have a non-nil contractWarnings after clone")
	}

	// Parent must also now have the shared set (assigned by cloneCoordinator).
	if c.contractWarnings == nil {
		t.Fatal("parent contractWarnings should be initialized by cloneCoordinator when nil")
	}
	if clone.contractWarnings != c.contractWarnings {
		t.Fatal("clone and parent must share the same contractWarnings pointer")
	}
}

// TestExecuteSingleAgentWithModel_NoDuplicateWarning simulates the
// extra-models path: the outer executeTask emits a contract_warning for the
// todoID, then executeSingleAgentWithModel clones the coordinator and calls
// executeTask again. The shared dedup set must suppress the duplicate.
//
// This test drives cloneCoordinator + emitContractWarnings directly to model
// the exact sequence without requiring a real provider/agent.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestExecuteSingleAgentWithModel_NoDuplicateWarning(t *testing.T) {
	var warnCount int
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			warnCount++
		}
	})
	c.session = &TeamSession{
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name: "extra",
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	findings := ValidateExecutionContractFull(task, agent.VerifierLintWarn).Findings

	// Step 1: outer executeTask emits the warning for todoID "1".
	c.emitContractWarnings("1", findings)
	if warnCount != 1 {
		t.Fatalf("expected 1 contract_warning from outer executeTask, got %d", warnCount)
	}

	// Step 2: executeSingleAgentWithModel clones the coordinator. The clone
	// shares the dedup set. Its executeTask call would re-run
	// validateContractStructural → emitContractWarnings, but the shared set
	// suppresses the duplicate.
	clone := cloneCoordinator(c, c.session)
	clone.emitContractWarnings("1", findings)
	if warnCount != 1 {
		t.Errorf("expected clone to suppress duplicate warning (shared dedup), got %d contract_warning events", warnCount)
	}
}

func TestCloneCoordinatorRoutesUsageToParentNoProgressBudget(t *testing.T) {
	parent := &Coordinator{
		taskTracker: NewTaskTracker(),
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Name: "usage-parent",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
	}
	clone := cloneCoordinator(parent, parent.session)
	clone.recordReliabilityUsage("todo-1", 1, 17)
	clone.recordReliabilityUsage("todo-1", 1, 17) // cumulative receipt must deduplicate within this clone
	clone.recordNoProgressTokens(5)               // auxiliary/sidecar usage must use the parent too

	if got := parent.noProgressCounters().Tokens; got != 22 {
		t.Fatalf("parent no-progress tokens = %d, want 22 from clone worker + auxiliary usage", got)
	}
	if got := clone.noProgressCounters().Tokens; got != 0 {
		t.Fatalf("clone-local no-progress tokens = %d, want accounting owned by parent", got)
	}
	if stopped, reason := parent.enforceNoProgressBudget(); !stopped || reason == "" {
		t.Fatalf("parent budget enforcement = (%v, %q), want terminal stop from clone usage", stopped, reason)
	}
	if result := parent.LastRunResult(); result == nil || result.Outcome != RunOutcomePartial || result.Continuation == nil {
		t.Fatalf("parent result = %#v, want partial continuation after clone usage threshold", result)
	}
}
