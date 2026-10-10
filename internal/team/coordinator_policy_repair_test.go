package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestCoordinatorPolicyRepairIsDeterministicAndBounded(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	violation := &delegationPolicyViolation{message: "worker already has a terminal result"}

	first, exhausted := c.coordinatorPolicyRepairPrompt(violation)
	if exhausted || !strings.Contains(first, "Attempt 1/2") || !strings.Contains(first, "call finish directly") {
		t.Fatalf("first repair prompt=%q exhausted=%v", first, exhausted)
	}
	second, exhausted := c.coordinatorPolicyRepairPrompt(violation)
	if exhausted || !strings.Contains(second, "Attempt 2/2") {
		t.Fatalf("second repair prompt=%q exhausted=%v", second, exhausted)
	}
	third, exhausted := c.coordinatorPolicyRepairPrompt(violation)
	if !exhausted || !strings.HasPrefix(third, coordinatorPolicyRepairExhaustedPrefix) {
		t.Fatalf("third repair prompt=%q exhausted=%v", third, exhausted)
	}
	if !c.IsWrapUp() {
		t.Fatal("repair exhaustion must enter wrap-up")
	}
	fourth, exhausted := c.coordinatorPolicyRepairPrompt(violation)
	if !exhausted || !strings.Contains(fourth, "3/2") {
		t.Fatalf("repeated repair prompt=%q exhausted=%v, want latched 3/2 exhaustion", fourth, exhausted)
	}
	if got := c.coordinatorPolicyRepairsAttempt.Load(); got != maxCoordinatorPolicyRepairs+1 {
		t.Fatalf("repair attempts after exhaustion=%d, want %d", got, maxCoordinatorPolicyRepairs+1)
	}
}

func TestCoordinatorPolicyRepairRecognizesProviderWrappedError(t *testing.T) {
	if !isCoordinatorPolicyRepairResult("Error: " + coordinatorPolicyRepairPrefix + "\nAttempt 1/2") {
		t.Fatal("provider-wrapped repair marker was not recognized")
	}
}

func TestCoordinatorPolicyRepairTerminalizesFailedWorkflowImmediately(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		phaseWorkflow: &runtimeWorkflow{
			enabled: true,
			state:   PhaseFailed,
		},
	}

	prompt, exhausted := c.coordinatorPolicyRepairPrompt(&delegationPolicyViolation{message: "invalid delegation"})
	if !exhausted || !strings.HasPrefix(prompt, coordinatorPolicyRepairExhaustedPrefix) {
		t.Fatalf("failed-workflow repair prompt=%q exhausted=%v", prompt, exhausted)
	}
	if strings.Contains(prompt, "call finish directly") {
		t.Fatalf("failed-workflow repair offered an impossible finish action: %q", prompt)
	}
	if got := c.coordinatorPolicyRepairsAttempt.Load(); got != 1 {
		t.Fatalf("failed-workflow repair attempts=%d, want 1", got)
	}
	if !c.coordinatorPolicyRepairExhausted.Load() || c.coordinatorPolicyRepairPending.Load() || !c.IsWrapUp() {
		t.Fatal("failed-workflow repair must latch exhaustion, clear pending repair, and enter wrap-up")
	}
}

func TestCoordinatorPolicyRepairResponseSetsPendingState(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	response := c.coordinatorPolicyRepairResponse(&delegationPolicyViolation{message: "invalid delegation"})
	if !c.coordinatorPolicyRepairPending.Load() {
		t.Fatal("policy repair response did not set pending state")
	}
	if !isCoordinatorPolicyRepairResult(response.Content) {
		t.Fatalf("repair response = %q, want recognizable marker", response.Content)
	}
}

func TestCoordinatorPolicyRepairNeverRedispatchesCompletedWorker(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "completed work"}})[0]
	if err := tracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "done", "authoritative result"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{taskTracker: tracker, session: &TeamSession{Config: agent.TeamConfig{}}}
	c.coordinatorPolicyRepairsAttempt.Store(1)
	c.coordinatorPolicyRepairPending.Store(true)
	err := c.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: "repeat completed work"}})
	if err == nil || !strings.Contains(err.Error(), "completed workers may not be redispatched") {
		t.Fatalf("validateDelegationPolicy error=%v", err)
	}
	if got := len(tracker.TodoList().Items()); got != 1 {
		t.Fatalf("rejected repair dispatch created %d tasks", got)
	}
}

func TestCoordinatorPolicyRepairAllowsRedispatchWhenAgentHasUnfinishedWork(t *testing.T) {
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "go-reviewer", Desc: "batch 1 (done)"},
		{Agent: "go-reviewer", Desc: "batch 2 (failed)"},
	})
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskDone, "done", "batch 1 result"); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[1].ID, TaskError, "failed", "worker omitted submit_result"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{taskTracker: tracker, session: &TeamSession{Config: agent.TeamConfig{}}}
	c.coordinatorPolicyRepairsAttempt.Store(1)
	c.coordinatorPolicyRepairPending.Store(true)
	err := c.validateDelegationPolicy([]TaskDef{{Agent: "go-reviewer", Goal: "review batch 2 retry"}})
	if err != nil {
		t.Fatalf("validateDelegationPolicy failed unexpectedly for agent with unfinished work: %v", err)
	}
}

func TestCompletedPolicyRepairRestoresNormalDelegation(t *testing.T) {
	for _, protected := range []bool{false, true} {
		name := "reusable_role"
		if protected {
			name = "one_shot_role"
		}
		t.Run(name, func(t *testing.T) {
			tracker := NewTaskTracker()
			item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "analyst", Desc: "completed first stage"}})[0]
			if err := tracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "done", "accepted result"); err != nil {
				t.Fatal(err)
			}
			c := &Coordinator{taskTracker: tracker, session: &TeamSession{Config: agent.TeamConfig{}}}
			if protected {
				c.session.Config.Delegation.NoRedispatchAfterSuccess = []string{"analyst"}
			}
			c.coordinatorPolicyRepairPrompt(&delegationPolicyViolation{message: "wrong role in an earlier stage"})
			// Successful agent delegation clears the pending correction while
			// preserving cumulative attempts for the bounded repair budget.
			c.coordinatorPolicyRepairPending.Store(false)
			c.coordinatorPolicyRepairsSuccess.Add(1)
			err := c.validateDelegationPolicy([]TaskDef{{Agent: "analyst", Goal: "perform required next stage"}})
			if protected {
				if err == nil || !strings.Contains(err.Error(), "successful terminal results may not be redispatched in this team") {
					t.Fatalf("explicit team one-shot policy was bypassed: %v", err)
				}
			} else if err != nil {
				t.Fatalf("historical repair attempt prevented normal next-stage work: %v", err)
			}
			if c.coordinatorPolicyRepairsAttempt.Load() != 1 || c.coordinatorPolicyRepairsSuccess.Load() != 1 || c.coordinatorPolicyRepairPending.Load() {
				t.Fatal("normal delegation changed repair accounting")
			}
			// A later violation still uses the remaining budget; completing a
			// correction does not grant unbounded fresh repair attempts.
			if _, exhausted := c.coordinatorPolicyRepairPrompt(&delegationPolicyViolation{message: "later violation"}); exhausted {
				t.Fatal("remaining repair attempt was lost")
			}
			if _, exhausted := c.coordinatorPolicyRepairPrompt(&delegationPolicyViolation{message: "repeated later violation"}); !exhausted {
				t.Fatal("completed correction reset the bounded repair budget")
			}
		})
	}
}

func TestCoordinatorPolicyRepairFinalSummaryUsesTodoEvidence(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "completed work"}})[0]
	tracker.TodoList().UpdateStatusAndOutput(item.ID, TaskDone, "done", "authoritative result")
	c := &Coordinator{taskTracker: tracker}
	for i := 0; i < maxCoordinatorPolicyRepairs+1; i++ {
		c.coordinatorPolicyRepairPrompt(&delegationPolicyViolation{message: "invalid delegation"})
	}
	summary := c.finalizeCoordinatorPolicyRepairRun()
	if !strings.Contains(summary, "authoritative result") || strings.Contains(summary, "LLM") {
		t.Fatalf("summary=%q, want deterministic Todo evidence without LLM wording", summary)
	}
}
