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
}

func TestCoordinatorPolicyRepairRecognizesProviderWrappedError(t *testing.T) {
	if !isCoordinatorPolicyRepairResult("Error: " + coordinatorPolicyRepairPrefix + "\nAttempt 1/2") {
		t.Fatal("provider-wrapped repair marker was not recognized")
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
	err := c.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: "repeat completed work"}})
	if err == nil || !strings.Contains(err.Error(), "completed workers may not be redispatched") {
		t.Fatalf("validateDelegationPolicy error=%v", err)
	}
	if got := len(tracker.TodoList().Items()); got != 1 {
		t.Fatalf("rejected repair dispatch created %d tasks", got)
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
