package team

import (
	"context"
	"errors"
	"testing"
)

func TestVerificationRepairReturnsToProducerOnce(t *testing.T) {
	s, items := dagSchedulerOnFailureClassesFixture(t, []TaskFailureClass{FailureVerify}, FailureVerify)
	s.tasks[0].SideEffect, s.tasks[0].Recovery = SideEffectNone, RecoveryRetry
	s.tasks[1].SideEffect, s.tasks[1].Recovery = SideEffectNone, RecoveryRetry
	s.tasks[1].MaxRetries = 1
	s.coord.phaseWorkflow = workflowAt(t, PhaseVerify)
	s.coord.phaseWorkflow.policies.FailFast = false
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: items[1].Agent, todoID: items[1].ID, err: errors.New("verification rejected report ownership")})
	if s.retries[1] != 1 || s.states[0] != TaskInProgress || items[0].RemediationContext == nil {
		t.Fatalf("producer not repaired with diagnostic: retries=%v states=%v remediation=%#v", s.retries, s.states, items[0].RemediationContext)
	}
	drainRelaunchedTask(t, s)
	// Simulate completion of the repaired producer followed by another rejected
	// audit. The exhausted edge must retain failure rather than restart again.
	s.coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "repaired report")
	s.coord.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskError, "audit still rejected")
	items[1].FailureEvent = &FailureEventPayload{FailureClass: FailureVerify, RetryDisposition: RetryNone}
	s.states[0], s.states[1], s.inProgress = TaskDone, TaskInProgress, 1
	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: items[1].Agent, todoID: items[1].ID, err: errors.New("verification rejected again")})
	if s.retries[1] != 1 || s.states[0] != TaskDone || s.states[1] != TaskError {
		t.Fatalf("repair budget bypassed: retries=%v states=%v", s.retries, s.states)
	}
}
