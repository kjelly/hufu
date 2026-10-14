package team

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestLearningGapClassification(t *testing.T) {
	replayable := &RunEvent{Type: "memory_retrieved"}
	tests := []struct {
		name       string
		gap        LearningGap
		autoRepair bool
		want       MemoryLearningReport
	}{
		{
			name:       "replayable memory event",
			gap:        LearningGap{EventType: "memory_retrieved", IdempotencyKey: "k", PendingRepair: true, RepairEvent: replayable},
			autoRepair: true,
			want:       MemoryLearningReport{PendingRepairGaps: 1},
		},
		{
			name:       "aggregate repair of an appended event",
			gap:        LearningGap{EventType: "memory_aggregate_repair", IdempotencyKey: "k", PendingRepair: true},
			autoRepair: true,
			want:       MemoryLearningReport{PendingRepairGaps: 1},
		},
		{
			name: "non-memory event append failure",
			gap:  LearningGap{EventType: "shared_working_memory_saved", PendingRepair: true},
			want: MemoryLearningReport{UnrepairableGaps: 1},
		},
		{
			name: "memory event without an idempotency key",
			gap:  LearningGap{EventType: "memory_retrieved", PendingRepair: true, RepairEvent: replayable},
			want: MemoryLearningReport{UnrepairableGaps: 1},
		},
		{
			name: "manual credit review",
			gap:  LearningGap{EventType: "memory_outcome_recorded", ManualReviewRequired: true},
			want: MemoryLearningReport{ManualReviewGaps: 1},
		},
		{
			name:       "resolved gap",
			gap:        LearningGap{EventType: "memory_retrieved", IdempotencyKey: "k", RepairEvent: replayable},
			autoRepair: true,
			want:       MemoryLearningReport{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAutoRepairableGap(tt.gap); got != tt.autoRepair {
				t.Fatalf("isAutoRepairableGap = %v, want %v", got, tt.autoRepair)
			}
			c := &Coordinator{
				session:     &TeamSession{},
				sessionData: &SessionData{LearningGaps: []LearningGap{tt.gap}},
			}
			got := c.MemoryLearningReport()
			got.EventCountsUnavailable = false
			if got != tt.want {
				t.Fatalf("report = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNonMemoryAppendFailureIsNotCountedAsAutomaticRepair(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{eventStore: store, session: &TeamSession{Workspace: workspace}, sessionData: NewSession()}
	store.commitHook = func() error { return errors.New("injected sync failure") }
	if err := c.emitEvent("shared_working_memory_saved", "coordinator", "", map[string]string{"run_id": "run", "kind": "fact"}); err == nil {
		t.Fatal("expected injected append failure")
	}

	if len(c.sessionData.LearningGaps) != 1 {
		t.Fatalf("learning gaps = %+v, want one", c.sessionData.LearningGaps)
	}
	report := c.MemoryLearningReport()
	if report.PendingRepairGaps != 0 || report.UnrepairableGaps != 1 {
		t.Fatalf("report = %+v, want 0 pending and 1 unrepairable", report)
	}
}

func TestLearningGapRepairWritesToTheBranchThatFailed(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "safe procedure", Scope: contextstore.Scope{ProjectID: "project"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tree := NewSessionTree()
	failedBranch, err := tree.CreateRootBranch("failed")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID(failedBranch.ID)

	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	c := &Coordinator{eventStore: store, contextRepo: repo, session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{MemoryLearning: policy}}, sessionData: NewSession()}
	payload, err := json.Marshal(memoryEventPayload{SchemaVersion: 1, RetrievalID: "retrieval-1", ContextItemID: "memory-1", PolicyVersion: policy.PolicyVersion})
	if err != nil {
		t.Fatal(err)
	}
	const key = "memory:retrieved:run:task:1:retrieval-1:memory-1"
	syncFile := store.commitHook
	store.commitHook = func() error { return errors.New("injected sync failure") }
	if _, err := c.emitEventOnce(key, RunEvent{Type: "memory_retrieved", Actor: "runtime", TaskID: "task", Attempt: 1, Payload: payload}); err == nil {
		t.Fatal("expected injected append failure")
	}
	store.commitHook = syncFile
	if gaps := c.sessionData.LearningGaps; len(gaps) != 1 || gaps[0].RepairEvent == nil || gaps[0].RepairEvent.BranchID != failedBranch.ID {
		t.Fatalf("learning gaps = %+v, want one repair event pinned to %q", gaps, failedBranch.ID)
	}

	// Check out main before the repair runs, as a later process would.
	if _, err := tree.CheckoutBranch("main", store); err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	c.repairMemoryLearningGaps(tree, "main")

	if gaps := c.sessionData.LearningGaps; len(gaps) != 1 || gaps[0].PendingRepair {
		t.Fatalf("learning gaps = %+v, want the gap resolved", gaps)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var branches []string
	for _, event := range events {
		if event.IdempotencyKey == key {
			branches = append(branches, effectiveEventBranchID(event))
		}
	}
	if len(branches) != 1 || branches[0] != failedBranch.ID {
		t.Fatalf("repaired event branches = %v, want only %q", branches, failedBranch.ID)
	}
	for _, event := range FilterEventsForBranch(events, tree, "main") {
		if event.IdempotencyKey == key {
			t.Fatalf("repair leaked the event into main: %+v", event)
		}
	}
	// The failed branch's startup hydration reduces the event, not main's repair.
	if aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", policy.PolicyVersion); err == nil && aggregate.ExposureCount != 0 {
		t.Fatalf("main reduced another branch's event: exposure count = %d", aggregate.ExposureCount)
	}
}
