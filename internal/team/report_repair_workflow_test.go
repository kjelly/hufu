package team

import (
	"context"
	"errors"
	"testing"
)

func TestVerificationRepairReturnsToProducerOnce(t *testing.T) {
	declared := loadHufuCodeReviewTeam(t)
	const (
		derive = iota
		synthesize
		audit
	)
	bound, _, err := CompileTaskGoalContracts(declared, []TaskDef{
		{Agent: "reviewer", ContractID: "derive-review-report"},
		{Agent: "synthesizer", ContractID: "synthesize-review"},
		{Agent: "reviewer", ContractID: "verify-review-report"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fact references are resolved before scheduler admission. This fixture
	// starts with an already executed report and only exercises failure routing.
	bound[synthesize].FactRefs = nil
	c := newTestCoordinator(t)
	ids := []string{"1", "2", "3"}
	items := make([]*TodoItem, len(bound))
	for i, task := range bound {
		items[i] = testTodoItemFromTask(task, ids[i], ids)
		items[i].Status = TaskDone
	}
	items[audit].Status = TaskError
	items[audit].FailureEvent = &FailureEventPayload{FailureClass: FailureVerify, RetryDisposition: RetryNone}
	c.taskTracker.TodoList().Restore(items)
	s := mustNewDAGScheduler(t, c, bound, items, nil)
	s.states[derive], s.states[synthesize], s.states[audit], s.inProgress = TaskDone, TaskDone, TaskInProgress, 1
	// The production team has a still-failed report acceptance milestone.
	// Without these links the fixture silently skips criterion routing and
	// cannot catch it preempting the declared failure edge.
	c.acceptanceSpec = declared.Config.AcceptanceSpec
	c.sessionData = NewSession()
	c.sessionData.CriterionResults = []CriterionResult{{ID: "review-report-audited", State: CriterionFailed}}
	c.phaseWorkflow = workflowAt(t, PhaseVerify)
	c.phaseWorkflow.policies.FailFast = false
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.handleEvent(ctx, agentTaskResult{idx: audit, agentName: items[audit].Agent, todoID: ids[audit], err: errors.New("verification rejected report ownership")})
	// The relaunched worker can replace the live TodoItem while persisting
	// its canceled attempt. Drain its result before inspecting that alias.
	drainRelaunchedTask(t, s)
	remediation := todoByID(t, c, ids[synthesize]).RemediationContext
	if s.criterionRetryUsed != 0 || s.retries[audit] != 1 || s.states[derive] != TaskDone || s.states[synthesize] != TaskInProgress || remediation == nil {
		t.Fatalf("synthesis not repaired with diagnostic: retries=%v states=%v remediation=%#v", s.retries, s.states, remediation)
	}
	// Simulate completion of the repaired producer followed by another rejected
	// audit. The exhausted edge must retain failure rather than restart again.
	c.taskTracker.TodoList().UpdateStatus(ids[synthesize], TaskDone, "repaired report")
	c.taskTracker.TodoList().UpdateStatus(ids[audit], TaskError, "audit still rejected")
	items[audit].FailureEvent = &FailureEventPayload{FailureClass: FailureVerify, RetryDisposition: RetryNone}
	s.states[synthesize], s.states[audit], s.inProgress = TaskDone, TaskInProgress, 1
	s.handleEvent(ctx, agentTaskResult{idx: audit, agentName: items[audit].Agent, todoID: ids[audit], err: errors.New("verification rejected again")})
	if s.criterionRetryUsed != 0 || s.retries[audit] != 1 || s.states[derive] != TaskDone || s.states[synthesize] != TaskDone || s.states[audit] != TaskError {
		t.Fatalf("repair budget bypassed: retries=%v states=%v", s.retries, s.states)
	}
}
