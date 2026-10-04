package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestExplicitAppliedMemoryReceivesVerifiedCredit(t *testing.T) {
	c, repo := outcomeTestCoordinator(t, "memory-1")
	item := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, &VerificationResult{ExitCode: 0})
	c.recordMemoryOutcomeForTask(item, "task_completed")
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "memory-policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.PositiveWeight != 1 || aggregate.VerifiedSupportCount != 1 || aggregate.NegativeWeight != 0 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
}

func TestUnknownFailureDoesNotPenalizeMemory(t *testing.T) {
	c, repo := outcomeTestCoordinator(t, "memory-1")
	item := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, nil)
	c.recordMemoryOutcomeForTask(item, "task_failed")
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "memory-policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.NegativeWeight != 0 || aggregate.CausalFailureCount != 0 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
}

func TestOutcomeCreditIsCappedAndNormalized(t *testing.T) {
	c, repo := outcomeTestCoordinator(t, "memory-1", "memory-2")
	uses := []MemoryUseRef{
		{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1},
		{RetrievalID: "retrieval-1", ContextItemID: "memory-2", Disposition: MemoryUseApplied, Confidence: 1},
	}
	c.recordMemoryOutcomeForTask(outcomeTestItem(uses, &VerificationResult{ExitCode: 0}), "task_completed")
	total := 0.0
	for _, id := range []string{"memory-1", "memory-2"} {
		aggregate, err := repo.ExperienceAggregate(context.Background(), id, "memory-policy-v1")
		if err != nil {
			t.Fatal(err)
		}
		total += aggregate.PositiveWeight
	}
	if total != 1 {
		t.Fatalf("total credit = %f, want 1", total)
	}
}

func TestOutcomeCreditIsCappedPerSignal(t *testing.T) {
	c, repo := outcomeTestCoordinator(t, "memory-1", "memory-2")
	uses := []MemoryUseRef{
		{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1},
		{RetrievalID: "retrieval-1", ContextItemID: "memory-2", Disposition: MemoryUseApplied, Confidence: 1},
	}
	item := outcomeTestItem(uses, &VerificationResult{ExitCode: 0})
	// verification_passed spends the full positive cap across the two items.
	c.recordMemoryOutcomeForTask(item, "task_completed")
	// acceptance_passed is an independent outcome signal: it must get its own
	// cap instead of being suppressed by the verification credit already spent.
	c.recordMemoryOutcomeSignal(item, "acceptance_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
	// Duplicate emission of either signal must remain idempotent.
	c.recordMemoryOutcomeForTask(item, "task_completed")
	c.recordMemoryOutcomeSignal(item, "acceptance_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
	for _, id := range []string{"memory-1", "memory-2"} {
		aggregate, err := repo.ExperienceAggregate(context.Background(), id, "memory-policy-v1")
		if err != nil {
			t.Fatal(err)
		}
		if aggregate.PositiveWeight != 1 {
			t.Fatalf("item %s positive weight = %f, want 1 (0.5 verification + 0.5 acceptance)", id, aggregate.PositiveWeight)
		}
	}
}

func TestCausalVerificationFailureDemotesMemory(t *testing.T) {
	c, repo := outcomeTestCoordinator(t, "memory-1")
	item := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, &VerificationResult{ExitCode: 1})
	item.TypedResult.Commands = []CommandResult{{Command: "go test ./...", ExitCode: 1}}
	c.recordMemoryOutcomeForTask(item, "task_failed")
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "memory-policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.NegativeWeight != 1 || aggregate.CausalFailureCount != 1 {
		t.Fatalf("aggregate = %+v", aggregate)
	}
}

func TestResumeDoesNotDoubleCountOutcome(t *testing.T) {
	testRepeatedOutcomeDoesNotDoubleCount(t)
}

func TestMemoryOutcomeWeightForSignalFiltersByManifestAcrossRuns(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-a", "session-memory-credit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	appendEvent := func(runID, taskID, eventType, payload string) {
		t.Helper()
		if err := store.Append(RunEvent{
			RunID: runID, TaskID: taskID, Type: eventType, Actor: "test", Payload: []byte(payload),
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent("run-a", "task-1", "memory_outcome_recorded", `{"retrieval_id":"retrieval-a","signal":"verification_passed","direction":"positive","effective_weight":0.25}`)
	appendEvent("run-b", "task-1", "memory_outcome_recorded", `{"retrieval_id":"retrieval-b","signal":"verification_passed","direction":"positive","effective_weight":0.9}`)
	appendEvent("run-b", "task-1", "memory_outcome_recorded", `{"retrieval_id":"retrieval-a","signal":"verification_passed","direction":"positive","effective_weight":0.5}`)
	appendEvent("run-a", "task-2", "memory_outcome_recorded", `{"retrieval_id":"retrieval-a","signal":"verification_passed","direction":"positive","effective_weight":0.8}`)
	appendEvent("run-a", "task-1", "memory_outcome_recorded", `{"retrieval_id":"retrieval-a","signal":"acceptance_passed","direction":"positive","effective_weight":0.8}`)
	appendEvent("run-a", "task-1", "memory_outcome_recorded", `{"retrieval_id":"retrieval-a","signal":"verification_passed","direction":"negative","effective_weight":0.8}`)
	appendEvent("run-a", "task-1", "task_progress", `{"retrieval_id":"retrieval-a","signal":"verification_passed","direction":"positive","effective_weight":0.8}`)
	appendEvent("run-a", "task-1", "memory_outcome_recorded", `true`)
	for range 20 {
		appendEvent("run-b", "task-1", "task_progress", `{"noise":true}`)
	}

	c := &Coordinator{eventStore: store}
	item := &TodoItem{ID: "task-1", MemoryManifests: []MemoryInjectionManifest{{RetrievalID: "retrieval-a"}}}
	if got, err := c.memoryOutcomeWeightForSignal(item, "verification_passed", "positive"); err != nil || got != 0.75 {
		t.Fatalf("weight = %v, error = %v; want 0.75, nil", got, err)
	}

	store.stateValid = false
	store.stateErr = errors.New("injected read failure")
	if got, err := c.memoryOutcomeWeightForSignal(item, "verification_passed", "positive"); got != 0 || err == nil {
		t.Fatalf("invalid store weight = %v, error = %v; want 0 and an error", got, err)
	}
	c.eventStore = nil
	if got, err := c.memoryOutcomeWeightForSignal(item, "verification_passed", "positive"); got != 0 || err == nil {
		t.Fatalf("missing store weight = %v, error = %v; want 0 and an error", got, err)
	}
}

func TestMemoryOutcomeCreditReadFailureDoesNotAppendBeforeRecovery(t *testing.T) {
	c, _ := outcomeTestCoordinator(t, "memory-1")
	item := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-2", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, nil)
	item.TypedResult.Attempt = 2
	item.MemoryManifests = append(item.MemoryManifests, MemoryInjectionManifest{
		RetrievalID: "retrieval-2", RunID: "run-1", TaskID: item.ID, Attempt: 2,
		Agent: "worker", PolicyVersion: "memory-policy-v1", Items: []MemoryInjectionItem{{ContextItemID: "memory-1"}},
	})
	if err := c.eventStore.Append(RunEvent{
		RunID: "run-1", TaskID: item.ID, Type: "memory_outcome_recorded", Actor: "runtime",
		Payload: []byte(`{"retrieval_id":"retrieval-1","signal":"verification_passed","direction":"positive","effective_weight":0.75}`),
	}); err != nil {
		t.Fatal(err)
	}

	c.eventStore.stateValid = false
	c.eventStore.stateErr = errors.New("injected read failure")
	c.recordMemoryOutcomeSignal(item, "verification_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
	if c.eventStore.stateValid {
		t.Fatal("outcome append recovered an unreadable credit ledger")
	}
	if c.sessionData == nil || len(c.sessionData.LearningGaps) != 1 || !c.sessionData.LearningGaps[0].ManualReviewRequired || c.sessionData.LearningGaps[0].PendingRepair {
		t.Fatalf("credit gap was not classified for manual review: %+v", c.sessionData)
	}
	if saved := LoadSession(c.session.Workspace); saved == nil || len(saved.LearningGaps) != 1 || !saved.LearningGaps[0].ManualReviewRequired || saved.LearningGaps[0].PendingRepair {
		t.Fatalf("manual review gap was not checkpointed: %+v", saved)
	}
	c.repairMemoryLearningGaps()
	if !c.sessionData.LearningGaps[0].ManualReviewRequired || c.sessionData.LearningGaps[0].PendingRepair {
		t.Fatalf("automatic repair changed manual credit gap: %+v", c.sessionData.LearningGaps[0])
	}

	// A subsequent independent append may restore the store. Only then may
	// the new retrieval spend the signal's remaining quarter-credit.
	if err := c.eventStore.Append(RunEvent{RunID: "run-1", TaskID: item.ID, Type: "task_progress", Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	c.recordMemoryOutcomeSignal(item, "verification_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
	events, err := c.eventStore.QueryEvents(EventQuery{Types: []string{"memory_outcome_recorded"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("outcome events = %d, want 2", len(events))
	}
	weight, err := c.memoryOutcomeWeightForSignal(item, "verification_passed", "positive")
	if err != nil || weight != 1 {
		t.Fatalf("credit after recovery = %v, error = %v; want 1, nil", weight, err)
	}
}

func TestRepairMemoryLearningGapsClassifiesLegacyCreditGapForManualReview(t *testing.T) {
	c, _ := outcomeTestCoordinator(t)
	c.sessionData = &SessionData{LearningGaps: []LearningGap{{
		EventType: "memory_outcome_recorded", TaskID: "task-1", PendingRepair: true,
	}}}
	c.repairMemoryLearningGaps()
	gap := c.sessionData.LearningGaps[0]
	if gap.PendingRepair || !gap.ManualReviewRequired {
		t.Fatalf("legacy credit gap was not reclassified: %+v", gap)
	}
	if saved := LoadSession(c.session.Workspace); saved == nil || len(saved.LearningGaps) != 1 || !saved.LearningGaps[0].ManualReviewRequired {
		t.Fatalf("legacy credit gap reclassification was not durable: %+v", saved)
	}
}

func TestMemoryOutcomeWithoutAttributionDoesNotRecordReadGap(t *testing.T) {
	c, _ := outcomeTestCoordinator(t, "memory-1")
	item := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, nil)
	item.MemoryManifests = nil
	c.eventStore.stateValid = false
	c.eventStore.stateErr = errors.New("injected read failure")
	c.recordMemoryOutcomeSignal(item, "verification_passed", "positive", 1, func(MemoryUseRef) float64 { return 1 })
	if c.sessionData != nil && len(c.sessionData.LearningGaps) != 0 {
		t.Fatalf("no-attribution outcome recorded a learning gap: %+v", c.sessionData.LearningGaps)
	}
}

func TestFastPathUpgradeDoesNotDoubleCountOutcome(t *testing.T) {
	testRepeatedOutcomeDoesNotDoubleCount(t)
}

func testRepeatedOutcomeDoesNotDoubleCount(t *testing.T) {
	t.Helper()
	c, repo := outcomeTestCoordinator(t, "memory-1")
	item := outcomeTestItem([]MemoryUseRef{{RetrievalID: "retrieval-1", ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}, &VerificationResult{ExitCode: 0})
	c.recordMemoryOutcomeForTask(item, "task_completed")
	c.recordMemoryOutcomeForTask(item, "task_completed")
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "memory-policy-v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.PositiveWeight != 1 || aggregate.VerifiedSupportCount != 1 {
		t.Fatalf("duplicate outcome aggregate = %+v", aggregate)
	}
}

func outcomeTestCoordinator(t *testing.T, ids ...string) (*Coordinator, *contextstore.SQLiteRepository) {
	t.Helper()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		fingerprint := sha256.Sum256([]byte("go test ./..."))
		if err := repo.Append(context.Background(), contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: "procedure " + id, Scope: contextstore.Scope{ProjectID: "project"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"action_fingerprint": hex.EncodeToString(fingerprint[:])}}); err != nil {
			t.Fatal(err)
		}
	}
	es, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close(); _ = repo.Close() })
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{MemoryLearning: policy}}, contextRepo: repo, eventStore: es}
	return c, repo
}

func outcomeTestItem(uses []MemoryUseRef, verification *VerificationResult) *TodoItem {
	manifestItems := make([]MemoryInjectionItem, 0, len(uses))
	for _, use := range uses {
		manifestItems = append(manifestItems, MemoryInjectionItem{ContextItemID: use.ContextItemID})
	}
	return &TodoItem{ID: "task-1", Agent: "worker", VerifyResult: verification, TypedResult: &TaskResult{TaskID: "task-1", Attempt: 1, MemoryUses: uses}, MemoryManifests: []MemoryInjectionManifest{{RetrievalID: "retrieval-1", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker", PolicyVersion: "memory-policy-v1", Items: manifestItems}}}
}
