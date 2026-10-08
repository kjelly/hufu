package team

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestAcceptanceRecoveryRespectsWorkerDisposition(t *testing.T) {
	for _, disposition := range []RetryDisposition{ReconcileOnly, NeedsHuman, ReplanRequired, RetryWorker} {
		t.Run(string(disposition), func(t *testing.T) {
			c := newDoneWorkflowCoordinator(t)
			c.phaseWorkflow = nil
			c.acceptanceCmd = ""
			if err := c.SetAcceptanceSpec(AcceptanceSpec{Mode: "blocking", RequireNoUnresolvedTasks: true, RequiredWorkers: []string{"worker"}}); err != nil {
				t.Fatal(err)
			}
			// Match a blocking outcome gate without a strict manifest gate,
			// which would refuse finish before acceptance recovery is reached.
			profile, _ := GetBuiltinProfile(string(ProfileDefault))
			profile.AcceptanceMode = AcceptanceBlocking
			c.SetExecutionProfile(profile)
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "original failed task"}})[0]
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskBlocked, "result repair failed")
			item.FailureEvent = &FailureEventPayload{FailureClass: FailureProtocol, RetryDisposition: disposition}
			c.SetWrapUp()
			response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{
				Input: `{"response":"incomplete evidence report","acknowledge_failed_tasks":true}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			terminal := disposition == ReconcileOnly || disposition == NeedsHuman
			if !terminal {
				if !response.IsError || !c.acceptanceRecovery.Load() || c.selfHealingAttempts != 1 || c.finishCalled.Load() {
					t.Fatalf("recoverable task lost bounded self-healing: response=%#v attempts=%d", response, c.selfHealingAttempts)
				}
				return
			}
			if response.IsError || c.acceptanceRecovery.Load() || c.selfHealingAttempts != 0 || !c.finishCalled.Load() {
				t.Fatalf("terminal disposition reopened worker dispatch: response=%#v attempts=%d", response, c.selfHealingAttempts)
			}
			if !strings.Contains(response.Content, string(disposition)) || strings.Contains(response.Content, "re-run tasks") {
				t.Fatalf("terminal finish gave incompatible recovery advice: %s", response.Content)
			}
			result := c.LastRunResult()
			if result == nil || IsRunOutcomeSuccess(result.Outcome) || result.ExitCode == 0 || len(result.UnresolvedTasks) != 1 || result.Acceptance == nil || result.Acceptance.EffectiveState() != AcceptanceFailed {
				t.Fatalf("terminal acceptance concealed the original failure: %#v", result)
			}
			_, dispatchErr := c.ExecuteTasks(t.Context(), []TaskDef{{Agent: "worker", Goal: "same task with rewritten prose"}})
			if dispatchErr == nil || !strings.Contains(dispatchErr.Error(), "wrap-up") || len(c.taskTracker.TodoList().Items()) != 1 {
				t.Fatalf("worker replay allowed after terminal finish: err=%v", dispatchErr)
			}
		})
	}
}

func TestAcceptanceRecoveryIgnoresVerifiedResolution(t *testing.T) {
	c, original, replacement := verifiedResolutionFixture(t)
	original.FailureEvent.RetryDisposition = ReconcileOnly
	if err := c.CommitTaskResolution(t.Context(), original.ID, &TaskResolution{
		Status: "superseded", ResolvedBy: replacement.ID, Reason: "same frozen contract verified",
	}); err != nil {
		t.Fatal(err)
	}
	if reason := c.acceptanceRepairUnavailableReason(); reason != "" {
		t.Fatalf("verified resolution still blocks acceptance recovery: %s", reason)
	}
}
