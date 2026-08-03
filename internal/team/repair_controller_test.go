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
