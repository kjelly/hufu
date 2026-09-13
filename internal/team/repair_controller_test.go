package team

import (
	"context"
	"errors"
	"testing"
)

func TestRepairControllerFailClosedAndCheckpointed(t *testing.T) {
	rc := NewRepairController()
	checkpointed, retried := false, false
	result := rc.Execute(context.Background(), RepairRequest{
		Task:       TaskDef{Agent: "worker", Goal: "retry"},
		Checkpoint: func(context.Context) error { checkpointed = true; return nil },
		Retry:      func(context.Context) error { retried = true; return nil },
	})
	if result.Err != nil || result.State != "executing" || !checkpointed || !retried {
		t.Fatalf("retry result = %#v, checkpointed=%v retried=%v", result, checkpointed, retried)
	}

	blocked := rc.Execute(context.Background(), RepairRequest{
		Task: TaskDef{SideEffect: SideEffectExternalWrite},
		Checkpoint: func(context.Context) error {
			t.Fatal("blocked repair must not checkpoint or execute")
			return nil
		},
		Retry: func(context.Context) error {
			t.Fatal("blocked repair must not retry")
			return nil
		},
	})
	if blocked.State != "blocked" || blocked.Err == nil {
		t.Fatalf("external mutation without reconcile = %#v", blocked)
	}
}

func TestRepairControllerReconcileAndRollbackGuards(t *testing.T) {
	rc := NewRepairController()
	result := rc.Execute(context.Background(), RepairRequest{
		Task:       TaskDef{Recovery: RecoveryReconcile},
		Checkpoint: func(context.Context) error { return nil },
		Reconcile:  func(context.Context) (string, error) { return RecoveryStateComplete, nil },
	})
	if result.Err != nil || result.State != RecoveryStateComplete {
		t.Fatalf("complete reconcile = %#v", result)
	}
	unknown := rc.Execute(context.Background(), RepairRequest{
		Task:       TaskDef{Recovery: RecoveryReconcile},
		Checkpoint: func(context.Context) error { return nil },
		Reconcile:  func(context.Context) (string, error) { return RecoveryStateUnknown, nil },
	})
	if unknown.State != "blocked" || unknown.Err == nil {
		t.Fatalf("unknown reconcile = %#v", unknown)
	}
	rollback := rc.Execute(context.Background(), RepairRequest{
		Task:              TaskDef{},
		AllowRollback:     true,
		RollbackRequested: true,
		Checkpoint:        func(context.Context) error { return nil },
		Rollback:          func(context.Context) error { return errors.New("rollback failed") },
	})
	if rollback.Decision.Action != RepairRollback || rollback.State != "blocked" || rollback.Err == nil {
		t.Fatalf("rollback result = %#v", rollback)
	}
}

func TestRepairControllerAllowsRetryAfterReconcileProvesNotStarted(t *testing.T) {
	rc := NewRepairController()
	checkpointed, retried := false, false
	result := rc.Execute(context.Background(), RepairRequest{
		Task:          TaskDef{SideEffect: SideEffectExternalWrite, Recovery: RecoveryReconcile},
		RecoveryState: RecoveryStateNotStarted,
		Checkpoint:    func(context.Context) error { checkpointed = true; return nil },
		Retry:         func(context.Context) error { retried = true; return nil },
	})
	if result.Err != nil || result.Decision.Action != RepairRetry || !checkpointed || !retried {
		t.Fatalf("retry after not-started reconcile = %#v, checkpointed=%v retried=%v", result, checkpointed, retried)
	}
}

func TestRepairControllerDecisionMatrix(t *testing.T) {
	replay := true
	noReplay := false
	tests := []struct {
		name string
		req  RepairRequest
		want RepairAction
	}{
		{name: "replayable retry", req: RepairRequest{Task: TaskDef{Execution: ExecutionContract{AllowsReplay: &replay}}}, want: RepairRetry},
		{name: "requested escalation", req: RepairRequest{Task: TaskDef{Escalate: true}}, want: RepairEscalate},
		{name: "manual blocks", req: RepairRequest{Task: TaskDef{Recovery: RecoveryManual}}, want: RepairBlock},
		{name: "never blocks", req: RepairRequest{Task: TaskDef{Recovery: RecoveryNever}}, want: RepairBlock},
		{name: "non replayable reconciles", req: RepairRequest{Task: TaskDef{Execution: ExecutionContract{AllowsReplay: &noReplay}, ReconcileTool: "probe"}}, want: RepairReconcile},
		{name: "external without reconcile blocks", req: RepairRequest{Task: TaskDef{SideEffect: SideEffectExternalWrite}}, want: RepairBlock},
		{name: "reconciled not started retries", req: RepairRequest{Task: TaskDef{SideEffect: SideEffectExternalWrite, Recovery: RecoveryReconcile}, RecoveryState: RecoveryStateNotStarted}, want: RepairRetry},
		{name: "reconciled not started escalates", req: RepairRequest{Task: TaskDef{SideEffect: SideEffectExternalWrite, Recovery: RecoveryReconcile, Escalate: true}, RecoveryState: RecoveryStateNotStarted}, want: RepairEscalate},
		{name: "attempt budget replans", req: RepairRequest{Task: TaskDef{}, Attempt: 2, MaxAttempts: 2}, want: RepairReplan},
		{name: "reconciled attempt budget replans", req: RepairRequest{Task: TaskDef{SideEffect: SideEffectExternalWrite, Recovery: RecoveryReconcile}, RecoveryState: RecoveryStateNotStarted, Attempt: 2, MaxAttempts: 2}, want: RepairReplan},
		{name: "unauthorized rollback blocks", req: RepairRequest{RollbackRequested: true}, want: RepairBlock},
	}

	controller := NewRepairController()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controller.Decide(tt.req).Action; got != tt.want {
				t.Fatalf("Decide action = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepairControllerCheckpointFailureRunsNoActionCallback(t *testing.T) {
	callbackCalls := 0
	result := NewRepairController().Execute(t.Context(), RepairRequest{
		Task:       TaskDef{},
		Checkpoint: func(context.Context) error { return errors.New("checkpoint failed") },
		Retry:      func(context.Context) error { callbackCalls++; return nil },
		Escalate:   func(context.Context) error { callbackCalls++; return nil },
		Reconcile:  func(context.Context) (string, error) { callbackCalls++; return RecoveryStateComplete, nil },
		Replan:     func(context.Context) error { callbackCalls++; return nil },
		Rollback:   func(context.Context) error { callbackCalls++; return nil },
	})
	if result.Err == nil || result.State != "blocked" {
		t.Fatalf("checkpoint failure result = %#v", result)
	}
	if callbackCalls != 0 {
		t.Fatalf("checkpoint failure ran %d action callbacks, want 0", callbackCalls)
	}
}
