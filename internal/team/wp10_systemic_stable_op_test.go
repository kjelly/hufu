package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

// TestWP10_SystemicCount_BlocksFutureUnFingerprintedTask is the reviewer
// P1 regression: after a systemic scope is escalated, a FUTURE candidate
// task that has NOT yet failed (no failure fingerprint) but whose
// deterministic (component, operation) prefix matches the escalated scope
// must be blocked at dispatch. §6.2: "停止對該 scope 派工".
//
// This test covers the STABLE operation identity: a failed task that ran
// and recorded a mutable LastOperation (e.g. "bash") must fingerprint with
// the STABLE operation (verify/reconcile/kind-derived, ignoring
// LastOperation), so a fresh candidate with no LastOperation derives the
// same operation and is blocked. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, §11, WP-10
func TestWP10_SystemicCount_BlocksFutureUnFingerprintedTask(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	// Three distinct tasks fail. They each RAN (LastOperation set to
	// "bash" by the tool-call path), but the systemic fingerprint uses
	// stableOperation (ignoring LastOperation), so the scope operation is
	// "task:outcome" (Kind-derived, no verify/reconcile).
	for i := 0; i < 3; i++ {
		item := &TodoItem{ID: string(rune('a' + i)), Kind: TaskKindOutcome, Advances: []string{"build"}, LastOperation: "bash"}
		fp := NewFailureFingerprint("build", "worker", stableOperation(item), FailureVerify, "exit code 1")
		state.record(item, fp, "", limits)
	}
	if !state.HardBlocked {
		t.Fatal("precondition: systemic escalation did not fire")
	}
	// A future candidate task: same agent, same Kind, NO LastOperation
	// (it has never run). stableOperation derives "task:outcome" — the
	// SAME operation the failed tasks fingerprinted with — so the prefix
	// matches and the candidate is blocked. This is the §6.2
	// "停止對該 scope 派工" behavior for ordinary post-escalation tasks.
	futureItem := &TodoItem{
		ID:       "future-task",
		Agent:    "worker",
		Kind:     TaskKindOutcome,
		Advances: []string{"build"},
	}
	blocked := state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, futureItem)
	if !blocked {
		t.Fatal("future un-fingerprinted task with matching stable (component, operation) prefix was NOT blocked after systemic escalation (§6.2: 停止對該 scope 派工)")
	}

	// A future task with a DIFFERENT component must NOT be blocked by
	// this scope's escalation.
	differentItem := &TodoItem{
		ID:       "other-task",
		Agent:    "reviewer",
		Kind:     TaskKindOutcome,
		Advances: []string{"build"},
	}
	if state.blocksTask(TaskDef{Agent: "reviewer", Kind: TaskKindOutcome, Advances: []string{"build"}}, differentItem) {
		t.Fatal("unrelated task with a different component was incorrectly blocked by the systemic scope")
	}

	// A future task with a DIFFERENT Kind (different stable operation)
	// must NOT be blocked by this scope's escalation.
	differentKindItem := &TodoItem{
		ID:       "other-kind-task",
		Agent:    "worker",
		Kind:     TaskKindRepair,
		Advances: []string{"build"},
	}
	if state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindRepair, Advances: []string{"build"}}, differentKindItem) {
		t.Fatal("task with a different stable operation (Kind) was incorrectly blocked by the systemic scope")
	}
}

// TestWP10_SystemicCount_BlocksFutureTaskViaKindDerivedOperation verifies
// the prefix-block path works when the operation is derived from the task
// Kind (the common case for outcome tasks with no verify/last-op). The
// escalated scope's operation must match "task:outcome" for this to block.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func TestWP10_SystemicCount_BlocksFutureTaskViaKindDerivedOperation(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	// Failures where stableOperation(item) returns "task:outcome" (no
	// verify, no last-op). The fingerprint's Operation field is set to
	// "task:outcome" by stableOperation, so the scope prefix is
	// (worker, task:outcome).
	for i := 0; i < 3; i++ {
		item := &TodoItem{ID: string(rune('a' + i)), Kind: TaskKindOutcome, Advances: []string{"build"}}
		fp := NewFailureFingerprint("build", "worker", stableOperation(item), FailureVerify, "exit code 1")
		state.record(item, fp, "", limits)
	}
	if !state.HardBlocked {
		t.Fatal("precondition: escalation did not fire")
	}
	// A future outcome task with the same agent and no verify/last-op
	// derives operation "task:outcome" → prefix matches → blocked.
	future := &TodoItem{ID: "future", Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}
	if !state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, future) {
		t.Fatal("future task with Kind-derived operation matching the escalated scope prefix was NOT blocked")
	}
}

// TestWP10_SystemicCount_FailedTaskWithLastOperationBlocksFreshCandidate is
// the exact reviewer P1 scenario: real executed tasks record their final
// tool call via SetLastOperation (e.g. "bash"), so PersistFailure
// fingerprints them with LastOperation set. A fresh post-escalation
// candidate that has NOT run has no LastOperation and derives
// "task:outcome". The fix uses stableOperation (ignoring LastOperation)
// for the systemic fingerprint so both sides derive the same operation
// and the fresh candidate is blocked. This test reproduces the minimal
// failing scenario the reviewer described.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, §11, WP-10
func TestWP10_SystemicCount_FailedTaskWithLastOperationBlocksFreshCandidate(t *testing.T) {
	var state AntiThrashingState
	limits := agent.ReliabilityConfig{MaxSystemicFailureTasks: 3, HardEnforcement: true}
	// Three tasks failed AFTER execution, so each has LastOperation="bash"
	// (set by the tool-call path). The systemic fingerprint must use
	// stableOperation → "task:outcome", NOT "bash".
	for i := 0; i < 3; i++ {
		item := &TodoItem{ID: string(rune('a' + i)), Kind: TaskKindOutcome, Advances: []string{"build"}, LastOperation: "bash"}
		// Confirm the failed task's stableOperation differs from its
		// failureOperation (LastOperation path).
		if stableOperation(item) != "task:outcome" {
			t.Fatalf("stableOperation = %q, want task:outcome", stableOperation(item))
		}
		if failureOperation(item) != "bash" {
			t.Fatalf("failureOperation = %q, want bash (LastOperation path)", failureOperation(item))
		}
		fp := NewFailureFingerprint("build", "worker", stableOperation(item), FailureVerify, "exit code 1")
		state.record(item, fp, "", limits)
	}
	if !state.HardBlocked {
		t.Fatal("precondition: systemic escalation did not fire")
	}
	// Fresh candidate: same agent, same Kind, NO LastOperation (never ran).
	// Its stableOperation is "task:outcome" (matches the failed tasks'
	// fingerprint operation), so it MUST be blocked.
	fresh := &TodoItem{ID: "fresh", Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}
	if failureOperation(fresh) != "task:outcome" {
		t.Fatalf("fresh candidate failureOperation = %q, want task:outcome", failureOperation(fresh))
	}
	if !state.blocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, fresh) {
		t.Fatal("fresh candidate (no LastOperation) was NOT blocked after systemic escalation of tasks that ran with LastOperation=bash; stableOperation mismatch (§6.2: 停止對該 scope 派工)")
	}
}

// TestWP10_SystemicCount_FailedTaskWithLastOperationBlocksFreshCandidatePersistFailure
// is the end-to-end PersistFailure + scheduler-path version of the
// reviewer P1 scenario: three PersistFailure calls for tasks whose
// LastOperation was set to "bash" during execution, then a fresh
// candidate (no LastOperation) must be blocked by antiThrashingBlocksTask.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, §11, WP-10
func TestWP10_SystemicCount_FailedTaskWithLastOperationBlocksFreshCandidatePersistFailure(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp10-lastop"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	failed := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "a", Advances: []string{"build"}, Kind: TaskKindOutcome},
		{Agent: "worker", Desc: "b", Advances: []string{"build"}, Kind: TaskKindOutcome},
		{Agent: "worker", Desc: "c", Advances: []string{"build"}, Kind: TaskKindOutcome},
	})
	// Simulate execution: each task's last tool call was "bash".
	for _, item := range failed {
		c.taskTracker.TodoList().SetLastOperation(item.ID, "bash")
	}
	detail := "verification failed: exit code 1"
	for _, item := range failed {
		c.PersistFailure("worker", item.Desc, item.ID, detail)
	}
	if !c.antiThrashingHardBlocked() {
		t.Fatal("precondition: systemic escalation did not fire")
	}
	// Fresh candidate: same agent, same Kind, NO LastOperation.
	fresh := &TodoItem{ID: "fresh", Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}
	if !c.antiThrashingBlocksTask(TaskDef{Agent: "worker", Kind: TaskKindOutcome, Advances: []string{"build"}}, fresh) {
		t.Fatal("fresh candidate (no LastOperation) was NOT blocked via antiThrashingBlocksTask after systemic escalation of executed tasks (LastOperation=bash); stableOperation must be used on both sides (§6.2: 停止對該 scope 派工)")
	}
}
