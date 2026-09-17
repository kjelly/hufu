package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestPrimaryDecisionServiceProducesOneBoundRuntimeOccurrence(t *testing.T) {
	fixture := newDecisionE2E(t)
	fixture.coordinator.eventJournal = eventStoreJournal{store: fixture.coordinator.eventStore}
	preparer, err := NewPrimaryDecisionPreparer(fixture.coordinator, "Should the bridge change be performed?", agent.DecisionProfileBuiltinLightV2)
	if err != nil {
		t.Fatal(err)
	}
	logicalID := "ldr_0123456789abcdef0123456789abcdef"
	result, err := preparer.PrepareDecisionForTerminal(context.Background(), DecisionTerminalPreparationRequest{
		Intent:       TerminalIntent{EntryPoint: TerminalEntryFinishTool, WantsSuccess: true},
		Candidate:    &RunResult{RunID: "run-decision-v1", Outcome: RunOutcomeCompleted, GoalSatisfied: true},
		LogicalRunID: logicalID, ExecutionRunID: "run-decision-v1", BranchID: "main", Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Action != TerminalPreparationCommitTerminal || result.PrimaryBinding == nil || result.PrimaryBindingEventID == nil {
		t.Fatalf("preparation = %#v", result)
	}
	items := fixture.coordinator.TaskTracker().TodoList().Items()
	primaryCount := 0
	for _, item := range items {
		if IsPrimaryOccurrence(item) {
			primaryCount++
			if item.Status != TaskDone || item.PrimaryManifestProof == nil {
				t.Fatalf("primary occurrence = %#v", item)
			}
		}
	}
	if primaryCount != 1 {
		t.Fatalf("primary occurrence count = %d, want 1", primaryCount)
	}
	events, err := fixture.coordinator.eventJournal.ReadEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReplayLogicalDecisionRun(events, logicalID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if projection.ActivePrimary == nil || projection.Phase != LogicalDecisionBound {
		t.Fatalf("projection = %#v", projection)
	}
	resume, err := InspectDecisionResume(context.Background(), fixture.session.Workspace, logicalID)
	if err != nil {
		t.Fatal(err)
	}
	if resume.Question == "" || resume.ProfileRef != agent.DecisionProfileBuiltinLightV2 || resume.ExistingBinding == nil || resume.ExistingBindingEvent == nil {
		t.Fatalf("resume info = %#v", resume)
	}
}
