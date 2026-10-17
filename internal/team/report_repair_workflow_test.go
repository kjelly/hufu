package team

import (
	"context"
	"errors"
	"testing"
)

func TestVerificationRepairReturnsToProducerOnce(t *testing.T) {
	s, items := dagSchedulerOnFailureClassesFixture(t, []TaskFailureClass{FailureVerify}, FailureVerify)
	declared := loadHufuCodeReviewTeam(t)
	bound, _, err := CompileTaskGoalContracts(declared, []TaskDef{{Agent: "synthesizer", ContractID: "synthesize-review"}, {Agent: "reviewer", ContractID: "verify-review-report"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range s.tasks {
		s.tasks[i].DependsOn, s.tasks[i].OnFailure = bound[i].DependsOn, bound[i].OnFailure
		s.tasks[i].SideEffect, s.tasks[i].Recovery = bound[i].SideEffect, bound[i].Recovery
		s.tasks[i].MaxRetries, s.tasks[i].OnFailureClasses = bound[i].MaxRetries, bound[i].OnFailureClasses
		s.tasks[i].Kind, s.tasks[i].Advances = bound[i].Kind, bound[i].Advances
	}
	// The production team has a still-failed report acceptance milestone.
	// Without these links the fixture silently skips criterion routing and
	// cannot catch it preempting the declared failure edge.
	s.coord.acceptanceSpec = declared.Config.AcceptanceSpec
	s.coord.sessionData = NewSession()
	s.coord.sessionData.CriterionResults = []CriterionResult{{ID: "review-report-audited", State: CriterionFailed}}
	s.coord.phaseWorkflow = workflowAt(t, PhaseVerify)
	s.coord.phaseWorkflow.policies.FailFast = false
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: items[1].Agent, todoID: items[1].ID, err: errors.New("verification rejected report ownership")})
	// The relaunched worker can replace the live TodoItem while persisting
	// its canceled attempt. Drain its result before inspecting that alias.
	drainRelaunchedTask(t, s)
	if s.criterionRetryUsed != 0 || s.retries[1] != 1 || s.states[0] != TaskInProgress || items[0].RemediationContext == nil {
		t.Fatalf("producer not repaired with diagnostic: retries=%v states=%v remediation=%#v", s.retries, s.states, items[0].RemediationContext)
	}
	// Simulate completion of the repaired producer followed by another rejected
	// audit. The exhausted edge must retain failure rather than restart again.
	s.coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "repaired report")
	s.coord.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskError, "audit still rejected")
	items[1].FailureEvent = &FailureEventPayload{FailureClass: FailureVerify, RetryDisposition: RetryNone}
	s.states[0], s.states[1], s.inProgress = TaskDone, TaskInProgress, 1
	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: items[1].Agent, todoID: items[1].ID, err: errors.New("verification rejected again")})
	if s.criterionRetryUsed != 0 || s.retries[1] != 1 || s.states[0] != TaskDone || s.states[1] != TaskError {
		t.Fatalf("repair budget bypassed: retries=%v states=%v", s.retries, s.states)
	}
}
