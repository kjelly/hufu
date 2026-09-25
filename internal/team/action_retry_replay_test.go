package team

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// executePhaseWorkflow returns an enabled runtime workflow that has advanced
// to EXECUTE, where static actions run and in-run retries are recorded.
func executePhaseWorkflow(t *testing.T, session *TeamSession) *runtimeWorkflow {
	t.Helper()
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestPermitActionRetryRespectsReplaySafety(t *testing.T) {
	noReplay := false
	transient := ActionProviderError{Capability: "structured-actions", Cause: fmt.Errorf("temporary unavailable")}
	tests := []struct {
		name  string
		setup func(*TaskDef)
		want  bool
	}{
		{name: "none with retry recovery", setup: func(task *TaskDef) { task.SideEffect, task.Recovery = SideEffectNone, RecoveryRetry }, want: true},
		{name: "external write", setup: func(task *TaskDef) { task.SideEffect = SideEffectExternalWrite }},
		{name: "unknown side effect", setup: func(task *TaskDef) { task.SideEffect = SideEffectUnknown }},
		{name: "manual recovery", setup: func(task *TaskDef) { task.SideEffect, task.Recovery = SideEffectNone, RecoveryManual }},
		{name: "reconcile recovery", setup: func(task *TaskDef) { task.SideEffect, task.Recovery = SideEffectNone, RecoveryReconcile }},
		{name: "never recovery", setup: func(task *TaskDef) { task.SideEffect, task.Recovery = SideEffectNone, RecoveryNever }},
		{name: "replay disallowed by execution contract", setup: func(task *TaskDef) { task.Execution.AllowsReplay = &noReplay }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := executePhaseWorkflow(t, workflowTestSession(t))
			task := TaskDef{Agent: "executor", MaxRetries: 2, Action: &Action{Capability: "structured-actions", Type: "apply"}}
			tt.setup(&task)
			if got := w.permitActionRetry(task, transient); got != tt.want {
				t.Fatalf("permitActionRetry() = %v, want %v", got, tt.want)
			}
			_, _, _, retryState := w.snapshot()
			wantAttempts := 0
			if tt.want {
				wantAttempts = 1
			}
			if got := len(retryState.Attempts); got != wantAttempts {
				t.Fatalf("recorded retry signatures = %d, want %d (%v)", got, wantAttempts, retryState.Attempts)
			}
		})
	}
}

// TestDAGSchedulerTransientActionRetryRequiresReplaySafety covers the
// scheduler branch that re-runs a structured action after a transient
// provider failure: a replay-safe action is reset and relaunched, while a
// non-replayable one stays failed without consuming a retry.
func TestDAGSchedulerTransientActionRetryRequiresReplaySafety(t *testing.T) {
	tests := []struct {
		name        string
		sideEffect  SideEffectClass
		wantRetries int
		wantState   TaskStatus
	}{
		{name: "replay-safe action is retried", sideEffect: SideEffectNone, wantRetries: 1, wantState: TaskInProgress},
		{name: "external write is not retried", sideEffect: SideEffectExternalWrite, wantRetries: 0, wantState: TaskError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := workflowTestSession(t)
			coord := &Coordinator{
				session:       session,
				taskTracker:   NewTaskTracker(),
				reportStatus:  func(StatusEvent) {},
				sessionData:   NewSession(),
				taskCache:     newDefaultTaskCache(taskCacheDependencies{}),
				maxConcurrent: 1,
				phaseWorkflow: executePhaseWorkflow(t, session),
			}
			task := TaskDef{
				Agent: "executor", Goal: "execute apply", Phase: PhaseExecute, MaxRetries: 2,
				SideEffect: tt.sideEffect, Action: &Action{Capability: "structured-actions", Type: "apply"},
			}
			items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: task.Agent, Desc: task.Goal, Phase: task.Phase, Action: task.Action, SideEffect: task.SideEffect}})
			s := mustNewDAGScheduler(t, coord, []TaskDef{task}, items, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.inProgress = 1
			coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskInProgress, "running")
			coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskError, "provider failed")
			providerErr := ActionProviderError{Capability: "structured-actions", Cause: fmt.Errorf("temporary unavailable")}
			s.handleEvent(ctx, agentTaskResult{idx: 0, agentName: task.Agent, todoID: items[0].ID, err: providerErr})
			if s.retries[0] != tt.wantRetries {
				t.Fatalf("retries = %d, want %d", s.retries[0], tt.wantRetries)
			}
			if s.states[0] != tt.wantState {
				t.Fatalf("state = %s, want %s", s.states[0], tt.wantState)
			}
			// A relaunched task runs against the cancelled context; drain its
			// completion so it cannot race the test's cleanup.
			cancel()
			select {
			case <-s.eventCh:
			case <-time.After(2 * time.Second):
			}
		})
	}
}
