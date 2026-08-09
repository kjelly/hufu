package team

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// TestClassifyTaskFailure_ContractClass verifies that contract preflight
// failures are classified as FailureContract (not the default
// FailureExecution), satisfying the WP-02 requirement that error findings be
// recorded with the contract class before dispatch (reviewer P1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, WP-02
func TestClassifyTaskFailure_ContractClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want TaskFailureClass
	}{
		{
			name: "source=contract prefix (FailureDetail output)",
			err:  errors.New("source=contract | error=verifier contract error: verify (verifier_not_asserting): ..."),
			want: FailureContract,
		},
		{
			name: "contract preflight failed message",
			err:  errors.New("contract preflight failed: verify (verifier_invalid): missing path"),
			want: FailureContract,
		},
		{
			name: "execution error unaffected",
			err:  errors.New("agent failed: exit code 1"),
			want: FailureExecution,
		},
		{
			name: "verification error unaffected",
			err:  errors.New("verification failed: deliverable missing"),
			want: FailureVerify,
		},
		{
			name: "timeout error unaffected",
			err:  errors.New("context deadline exceeded"),
			want: FailureTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyTaskFailure(tt.err)
			if got != tt.want {
				t.Errorf("classifyTaskFailure() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRecordContractFailure_PersistsContractClass is an integration test that
// exercises the full recordContractFailure → PersistFailure →
// classifyTaskFailure path and asserts the failure is classified with the
// contract class (reviewer P1). It uses a real EventStore to capture the
// failure_fingerprint event and inspects the recorded class.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §9, WP-02
func TestRecordContractFailure_PersistsContractClass(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp02"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }

	es, err := NewEventStore(workspace, "run-test", "sess-test")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	c.eventStore = es

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f artifact || echo FAIL",
		Execution: ExecutionContract{
			Kind: ExecutionKindInline,
		},
	}
	findings := []ContractFinding{
		{
			Severity: FindingSeverityError,
			Code:     FindingVerifierNotAsserting,
			Field:    "verify",
			Message:  "verifier uses `|| echo ...` as a terminal fallback which always exits 0",
		},
	}
	c.recordContractFailure(task, "", findings)

	// recordContractFailure produces a detail beginning with "source=contract"
	// (via FailureDetail). classifyTaskFailure must classify this as
	// FailureContract. Reconstruct the detail exactly as recordContractFailure
	// builds it and assert the classifier agrees.
	detail := c.FailureDetail(
		errors.New("contract preflight failed: verify (verifier_not_asserting): verifier uses `|| echo ...`"),
		string(FailureContract),
	)
	gotClass := classifyTaskFailure(errors.New(detail))
	if gotClass != FailureContract {
		t.Errorf("classifyTaskFailure(detail) = %q, want %q (detail=%s)", gotClass, FailureContract, detail)
	}

	// Read the event store back and find the failure_fingerprint event with
	// class == contract.
	events, _ := es.ReadEvents()
	foundContractFingerprint := false
	for _, e := range events {
		if e.Type != "failure_fingerprint" {
			continue
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			continue
		}
		if fp, ok := payload["fingerprint"].(map[string]interface{}); ok {
			if class, ok := fp["class"].(string); ok && TaskFailureClass(class) == FailureContract {
				foundContractFingerprint = true
				break
			}
		}
	}
	if !foundContractFingerprint {
		t.Errorf("expected a failure_fingerprint event with class=%q", FailureContract)
	}
}

// TestCoordinatorExecuteTasks_WarnModeEmitsNoWarningAtPreflight verifies that
// the ExecuteTasks preflight (validateAndReportContract) does NOT emit
// contract_warning events — the per-task execution path (validateContractStructural)
// is the single warning emitter. This prevents duplicate warnings when both
// the preflight and executeTask run, and ensures the warn-mode task reaches
// executeTask before any warning is emitted (reviewer P2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCoordinatorExecuteTasks_WarnModeEmitsNoWarningAtPreflight(t *testing.T) {
	var warnCount int
	c := &Coordinator{}
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

	tasks := []TaskDef{
		{
			Agent:  "worker",
			Goal:   "do work",
			Verify: "test -f artifact || echo FAIL",
			Execution: ExecutionContract{
				Kind: ExecutionKindInline,
			},
		},
	}
	// ExecuteTasks preflights (no warning emission). It fails on agent
	// resolution (no agents configured), but the preflight itself should
	// emit zero contract_warning events — warnings come from executeTask.
	_, _ = c.ExecuteTasks(nil, tasks)

	if warnCount != 0 {
		t.Errorf("expected 0 contract_warning events at preflight (executeTask is the single emitter), got %d", warnCount)
	}
}

// TestValidateContractStructural_EmitsSingleWarning verifies the per-task
// execution-path check (validateContractStructural) is the single
// contract_warning emitter: it emits exactly one event for a warn-mode finding
// and deduplicates on repeated calls for the same todoID (reviewer P2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestValidateContractStructural_EmitsSingleWarning(t *testing.T) {
	var warnCount int
	c := &Coordinator{}
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
	// First call emits the warning.
	if err := c.validateContractStructural(task, "1"); err != nil {
		t.Fatalf("expected warn mode to not block, got: %v", err)
	}
	if warnCount != 1 {
		t.Fatalf("expected 1 contract_warning event on first call, got %d", warnCount)
	}
	// Second call for the same todoID is deduplicated (no new event).
	if err := c.validateContractStructural(task, "1"); err != nil {
		t.Fatalf("expected warn mode to not block on second call, got: %v", err)
	}
	if warnCount != 1 {
		t.Errorf("expected deduplication to keep count at 1, got %d", warnCount)
	}
	// A different todoID emits again (different task dispatch).
	if err := c.validateContractStructural(task, "2"); err != nil {
		t.Fatalf("expected warn mode to not block for new todoID, got: %v", err)
	}
	if warnCount != 2 {
		t.Errorf("expected 2 contract_warning events for different todoIDs, got %d", warnCount)
	}
}

// TestValidateContractStructural_BlocksOnError verifies the structural check
// still blocks error-severity findings (defense-in-depth on the execution path).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestValidateContractStructural_BlocksOnError(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintError,
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
	if err := c.validateContractStructural(task, "1"); err == nil {
		t.Fatal("expected error mode to block dispatch via structural check")
	}
}
