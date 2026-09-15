package inspect

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestRecoveryEligibilityReusesRepairPolicyWithoutExecuting(t *testing.T) {
	item := &team.TodoItem{
		ID: "task-7", Status: team.TaskBlocked, SideEffect: team.SideEffectExternalWrite,
		Recovery: team.RecoveryReconcile, ReconcileTool: "read-only-probe", RecoveryState: team.RecoveryStateUnknown,
		MaxRetries: 3, Retries: 1,
		FailureEvent:      &team.FailureEventPayload{FailureClass: team.FailureExecution, ReceiptID: "receipt-9", Fingerprint: "fingerprint-9"},
		ExecutionReceipts: []team.ExecutionReceipt{{Attempt: 2, ActionInvocationID: "action-2"}},
	}
	events := []IndexedEvent{{Ordinal: 4, Event: team.RunEvent{ID: "event-4", Hash: "hash-4", RunID: "run-42", TaskID: item.ID}}}
	got := RecoveryEligibilityForTasks([]*team.TodoItem{item}, events, "run-42", true)
	if got == nil || !got.ReconcileEligible || got.RetryEligible || got.ExternalEffectState != team.RecoveryStateUnknown {
		t.Fatalf("eligibility = %#v", got)
	}
	if got.ExpectedRevision == "" || got.PolicyRevision == "" || got.Attempt != 2 || !got.ResumeEligible {
		t.Fatalf("eligibility lacks target revision facts: %#v", got)
	}
	if len(got.SourceRefs) < 3 {
		t.Fatalf("eligibility source refs = %#v", got.SourceRefs)
	}
}

func TestRecoveryEligibilityPolicyDenyAndMissingCapability(t *testing.T) {
	item := &team.TodoItem{
		ID: "task", Status: team.TaskBlocked, SideEffect: team.SideEffectExternalWrite,
		Recovery: team.RecoveryManual, RecoveryState: team.RecoveryStateUnknown,
		FailureEvent: &team.FailureEventPayload{FailureClass: team.FailurePolicy},
	}
	got := RecoveryEligibilityForTasks([]*team.TodoItem{item}, nil, "run", false)
	if got == nil || !got.PolicyDenied || got.ReconcileEligible || got.RetryEligible || got.CapabilityKnown {
		t.Fatalf("manual/missing-capability eligibility = %#v", got)
	}
}

func TestRecoveryEligibilityAllowsRetryOnlyAfterNotStartedEvidence(t *testing.T) {
	item := &team.TodoItem{
		ID: "task", Status: team.TaskInProgress, SideEffect: team.SideEffectExternalWrite,
		Recovery: team.RecoveryReconcile, ReconcileTool: "probe", RecoveryState: team.RecoveryStateNotStarted,
		MaxRetries:   3,
		FailureEvent: &team.FailureEventPayload{RetryDisposition: team.ReconcileOnly},
	}
	got := RecoveryEligibilityForTasks([]*team.TodoItem{item}, nil, "run", true)
	if got == nil || !got.RetryEligible || got.ReconcileEligible {
		t.Fatalf("not-started eligibility = %#v", got)
	}
}

func TestRecoveryEligibilityHonorsTerminalDisposition(t *testing.T) {
	replan := &team.TodoItem{
		ID: "budget", Status: team.TaskError, Recovery: team.RecoveryRetry,
		FailureEvent: &team.FailureEventPayload{RetryDisposition: team.ReplanRequired},
	}
	got := RecoveryEligibilityForTask(replan, nil, "run", false)
	if got == nil || !got.ResumeEligible || got.RetryEligible || got.ReconcileEligible || got.ReasonCode != "recovery_replan" {
		t.Fatalf("replan eligibility = %#v", got)
	}

	reconcile := &team.TodoItem{
		ID: "protocol", Status: team.TaskBlocked, Recovery: team.RecoveryRetry,
		VerifySpec:   &team.VerificationSpec{Type: team.VerifyFileExists, Path: "result.json"},
		FailureEvent: &team.FailureEventPayload{RetryDisposition: team.ReconcileOnly},
	}
	got = RecoveryEligibilityForTask(reconcile, nil, "run", false)
	if got == nil || got.ResumeEligible || got.RetryEligible || !got.ReconcileEligible || got.ReasonCode != "recovery_reconcile" {
		t.Fatalf("reconcile eligibility = %#v", got)
	}
}

func TestRecoveryEligibilityPrioritizesUnknownExternalEffectAcrossTasks(t *testing.T) {
	tasks := []*team.TodoItem{
		{ID: "external", Status: team.TaskBlocked, SideEffect: team.SideEffectExternalWrite, Recovery: team.RecoveryReconcile},
		{ID: "later-local", Status: team.TaskError, SideEffect: team.SideEffectWorkspaceWrite, Recovery: team.RecoveryRetry},
	}
	got := RecoveryEligibilityForTasks(tasks, nil, "run", false)
	if got == nil || got.TaskID != "external" || got.ExternalEffectState != team.RecoveryStateUnknown {
		t.Fatalf("highest safety-priority recovery candidate = %#v", got)
	}
}

func TestSessionRecoveryRevisionBindsBranchAndCheckpointProjection(t *testing.T) {
	session := &team.SessionData{RecoveryRequired: true, WorkflowState: team.PhaseExecute, Tasks: []*team.TodoItem{{ID: "task", Status: team.TaskPaused, OccurrenceRevision: 2}}}
	first, refs := SessionRecoveryRevision(session, "main")
	again, _ := SessionRecoveryRevision(session, "main")
	if first == "" || first != again || len(refs) != 1 || refs[0] != "main" {
		t.Fatalf("session revision = %q / %q refs=%#v", first, again, refs)
	}
	branchChanged, _ := SessionRecoveryRevision(session, "other")
	if branchChanged == first {
		t.Fatal("branch change did not change session recovery revision")
	}
	session.Tasks[0].OccurrenceRevision++
	checkpointChanged, _ := SessionRecoveryRevision(session, "main")
	if checkpointChanged == first {
		t.Fatal("checkpoint projection change did not change session recovery revision")
	}
}
