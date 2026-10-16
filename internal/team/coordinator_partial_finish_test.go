package team

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestFailedWorkflowPartialFinishWritesCanonicalTerminalEvent(t *testing.T) {
	c := newDoneWorkflowCoordinator(t)
	t.Cleanup(func() { _ = c.Close() })
	c.executionRunID = "run-partial-workflow"
	store, err := NewEventStore(c.session.Workspace, c.executionRunID, "session-partial")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	c.terminalLifecycleRunID = c.executionRunID
	c.terminalLifecycleDone = make(chan struct{})
	c.terminalLifecyclePrepareDone = make(chan struct{})
	c.phaseWorkflow.state = PhaseFailed
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "auditor", Desc: "rejected audit", Phase: PhaseVerify}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "wrong artifact owner")
	response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"partial","acknowledge_failed_tasks":true}`})
	if err != nil || response.IsError {
		t.Fatalf("finish=%#v err=%v", response, err)
	}
	result := c.LastRunResult()
	c.FinalizeRun(t.Context(), result, result.Acceptance)
	c.FinalizeRun(t.Context(), result, result.Acceptance)
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, event := range events {
		if event.Type != "run_finished" {
			continue
		}
		finished++
		var payload struct {
			Outcome       RunOutcome `json:"outcome"`
			GoalSatisfied bool       `json:"goal_satisfied"`
			Acceptance    *struct {
				State string `json:"state"`
			} `json:"acceptance"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Outcome != RunOutcomePartial || payload.GoalSatisfied || payload.Acceptance == nil || payload.Acceptance.State != string(AcceptanceFailed) {
			t.Fatalf("terminal event lost failure: %s", event.Payload)
		}
	}
	if finished != 1 {
		t.Fatalf("run_finished count=%d, want 1", finished)
	}
}

func TestFailedWorkflowPartialFinishPreservesFailure(t *testing.T) {
	c := newDoneWorkflowCoordinator(t)
	t.Cleanup(func() { _ = c.Close() })
	c.phaseWorkflow.state = PhaseFailed
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "auditor", Desc: "audit report", Phase: PhaseVerify}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "wrong evidence owner")
	c.phaseWorkflow.failedTaskID = item.ID
	c.phaseWorkflow.failedFromPhase = PhaseVerify
	tool := &finishTool{coordinator: c}
	response, err := tool.Run(t.Context(), fantasy.ToolCall{Input: `{"response":"everything passed"}`})
	if err != nil || !response.IsError {
		t.Fatalf("unacknowledged failed workflow finished: %#v %v", response, err)
	}
	response, err = tool.Run(t.Context(), fantasy.ToolCall{Input: `{"response":"everything passed","acknowledge_failed_tasks":true}`})
	if err != nil || response.IsError {
		t.Fatalf("partial finish: %#v %v", response, err)
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomePartial || result.GoalSatisfied || result.ExitCode == 0 || len(result.UnresolvedTasks) != 1 || result.Acceptance == nil || result.Acceptance.EffectiveState() != AcceptanceFailed {
		t.Fatalf("failure concealed: %#v", result)
	}
	if c.phaseWorkflow.State() != PhaseFailed || item.Status != TaskError || c.selfHealingAttempts != 0 || !c.finishCalled.Load() {
		t.Fatal("partial termination changed failure or enabled repair")
	}
	if strings.Contains(response.Content, "everything passed") {
		t.Fatal("unaudited model claim used as final report")
	}
}

func TestFailedWorkflowPartialFinishWaitsForWorkers(t *testing.T) {
	c := newDoneWorkflowCoordinator(t)
	t.Cleanup(func() { _ = c.Close() })
	c.phaseWorkflow.state = PhaseFailed
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "still pending"}})
	response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"partial","acknowledge_failed_tasks":true}`})
	if err != nil || !response.IsError || c.finishCalled.Load() {
		t.Fatalf("pending worker bypassed: %#v %v", response, err)
	}
}

func TestFailedWorkflowAcknowledgmentPreservesDisposition(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  TaskStatus
		class   TaskFailureClass
		outcome RunOutcome
		code    int
	}{
		{"cancelled", TaskError, FailureCancelled, RunOutcomeCancelled, 130},
		{"blocked", TaskBlocked, FailurePolicy, RunOutcomeBlocked, 7},
		{"structural", TaskError, "", RunOutcomeFailed, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newDoneWorkflowCoordinator(t)
			t.Cleanup(func() { _ = c.Close() })
			c.phaseWorkflow.state = PhaseFailed
			if test.name != "structural" {
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "original failure"}})[0]
				c.taskTracker.TodoList().UpdateStatus(item.ID, test.status, "failed")
				item.FailureEvent = &FailureEventPayload{FailureClass: test.class}
			}
			response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"partial","acknowledge_failed_tasks":true}`})
			if err != nil || response.IsError {
				t.Fatalf("finish=%#v err=%v", response, err)
			}
			result := c.LastRunResult()
			if result == nil || result.Outcome != test.outcome || result.GoalSatisfied || result.ExitCode != test.code {
				t.Fatalf("disposition changed: %#v", result)
			}
		})
	}
}

func TestWorkflowFailureGuidanceChecksReplacementContract(t *testing.T) {
	c, original, replacement := verifiedResolutionFixture(t)
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhaseFailed, failedTaskID: original.ID}
	err := c.workflowFailureResult(errors.New("workflow failed"))
	if !strings.Contains(err.Error(), "verified replacement from ["+replacement.ID+"]") {
		t.Fatalf("missing valid resolver: %v", err)
	}
	replacement.ContractID = "unrelated"
	err = c.workflowFailureResult(errors.New("workflow failed"))
	if !strings.Contains(err.Error(), "no verified replacement") {
		t.Fatalf("unrelated resolver recommended: %v", err)
	}
}
