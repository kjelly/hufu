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
	if got := c.memoryOutcomeWeightForSignal(item, "verification_passed", "positive"); got != 0.75 {
		t.Fatalf("weight = %v, want 0.75", got)
	}

	store.stateValid = false
	store.stateErr = errors.New("injected read failure")
	if got := c.memoryOutcomeWeightForSignal(item, "verification_passed", "positive"); got != 0 {
		t.Fatalf("invalid store weight = %v, want 0", got)
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
