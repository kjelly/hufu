package team

import (
	"fmt"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// TestEmitContractWarnings_ConcurrentNilInitNoRace verifies that concurrent
// warn-mode emitContractWarnings calls on a coordinator with a nil
// contractWarnings pointer do not race on pointer initialization. Run with
// -race to refute the static data race flagged in the review (reviewer P1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestEmitContractWarnings_ConcurrentNilInitNoRace(t *testing.T) {
	var warnCount int
	var warnMu sync.Mutex
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			warnMu.Lock()
			warnCount++
			warnMu.Unlock()
		}
	})
	c.session = &TeamSession{
		Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}
	if c.contractWarnings != nil {
		t.Fatal("precondition: contractWarnings must be nil")
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

	// Spawn many goroutines that each emit warnings for DIFFERENT todoIDs
	// concurrently. The race would manifest as the unsynchronized nil check
	// assigning different pointers; with sync.Once the pointer is stable.
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			c.emitContractWarnings(fmt.Sprintf("todo-%d", i), findings)
		}(i)
	}
	wg.Wait()

	// Each todoID emitted exactly once → n warnings.
	if warnCount != n {
		t.Errorf("expected %d contract_warning events, got %d", n, warnCount)
	}
	// The pointer must now be non-nil and stable.
	if c.contractWarnings == nil {
		t.Fatal("contractWarnings should be initialized after concurrent emissions")
	}
}

// TestCloneCoordinator_ConcurrentNilParentNoRace verifies that concurrent
// cloneCoordinator calls on a nil-parent coordinator do not race on
// contractWarnings initialization. Each clone must resolve the same shared
// dedup set. Run with -race (reviewer P1 — extra-models nil-parent race).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestCloneCoordinator_ConcurrentNilParentNoRace(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.session = &TeamSession{
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name: "race",
			Reliability: agent.ReliabilityConfig{
				VerifierLintMode: agent.VerifierLintWarn,
			},
		},
	}
	if c.contractWarnings != nil {
		t.Fatal("precondition: contractWarnings must be nil")
	}

	// Spawn many concurrent cloneCoordinator calls (simulating parallel
	// executeSingleAgentWithModel goroutines on a nil-parent coordinator).
	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)
	clones := make([]*Coordinator, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			clones[i] = cloneCoordinator(c, c.session)
		}(i)
	}
	close(start)
	wg.Wait()

	// Every clone must share the parent's contractWarnings pointer.
	if c.contractWarnings == nil {
		t.Fatal("parent contractWarnings should be initialized by cloneCoordinator")
	}
	for i, clone := range clones {
		if clone == nil {
			t.Errorf("clone %d is nil", i)
			continue
		}
		if clone.contractWarnings == nil {
			t.Errorf("clone %d has nil contractWarnings", i)
			continue
		}
		if clone.contractWarnings != c.contractWarnings {
			t.Errorf("clone %d contractWarnings pointer differs from parent — dedup set not shared", i)
		}
	}
}

// TestEmitContractWarnings_ConcurrentSameTodoIDSingleEmission verifies that
// concurrent emitContractWarnings calls for the SAME todoID produce exactly
// one contract_warning event (the dedup set is shared and thread-safe).
// Run with -race (reviewer P1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestEmitContractWarnings_ConcurrentSameTodoIDSingleEmission(t *testing.T) {
	var warnCount int
	var warnMu sync.Mutex
	c := &Coordinator{taskTracker: NewTaskTracker()}
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			warnMu.Lock()
			warnCount++
			warnMu.Unlock()
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

	// Many goroutines emit for the SAME todoID — only one should win.
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-start
			c.emitContractWarnings("same-todo", findings)
		}()
	}
	close(start)
	wg.Wait()

	if warnCount != 1 {
		t.Errorf("expected exactly 1 contract_warning for same todoID under concurrency, got %d", warnCount)
	}
}
