package context

import (
	"context"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestRetrievedMemoryDoesNotReceiveReward(t *testing.T) {
	repo := openExperienceTestRepository(t)
	observation := ExperienceObservation{IdempotencyKey: "retrieved-1", ContextItemID: "memory-1", PolicyVersion: "v1", ExposureDelta: 1, ObservedAt: time.Unix(10, 0)}
	if applied, err := repo.ApplyExperienceObservation(context.Background(), observation); err != nil || !applied {
		t.Fatalf("ApplyExperienceObservation = %v, %v", applied, err)
	}
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.ExposureCount != 1 || aggregate.PositiveWeight != 0 || aggregate.NegativeWeight != 0 {
		t.Fatalf("retrieval aggregate = %+v", aggregate)
	}
}

func TestOutcomeReducerIsIdempotent(t *testing.T) {
	repo := openExperienceTestRepository(t)
	observation := ExperienceObservation{IdempotencyKey: "verified-1", ContextItemID: "memory-1", PolicyVersion: "v1", TaskID: "task-1", AppliedDelta: 1, PositiveWeight: 1, VerifiedSupportDelta: 1}
	for i, wantApplied := range []bool{true, false} {
		applied, err := repo.ApplyExperienceObservation(context.Background(), observation)
		if err != nil || applied != wantApplied {
			t.Fatalf("apply %d = %v, %v; want %v", i, applied, err, wantApplied)
		}
	}
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.PositiveWeight != 1 || aggregate.AppliedCount != 1 || aggregate.VerifiedSupportCount != 1 || aggregate.IndependentTaskCount != 1 {
		t.Fatalf("idempotent aggregate = %+v", aggregate)
	}
}

func TestAggregateCanRebuildFromRunEvents(t *testing.T) {
	repo := openExperienceTestRepository(t)
	observations := []ExperienceObservation{
		{IdempotencyKey: "b", ContextItemID: "memory-1", PolicyVersion: "v1", TaskID: "task-2", NegativeWeight: 0.25, CausalFailureDelta: 1, ObservedAt: time.Unix(20, 0)},
		{IdempotencyKey: "a", ContextItemID: "memory-1", PolicyVersion: "v1", TaskID: "task-1", PositiveWeight: 1, VerifiedSupportDelta: 1, ObservedAt: time.Unix(10, 0)},
	}
	if err := repo.RebuildExperienceAggregates(context.Background(), observations); err != nil {
		t.Fatal(err)
	}
	first, err := repo.ListExperienceAggregates(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildExperienceAggregates(context.Background(), observations); err != nil {
		t.Fatal(err)
	}
	second, err := repo.ListExperienceAggregates(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("rebuild is not deterministic:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestBetaQuantileUniform(t *testing.T) {
	if got := BetaQuantile(1, 1, 0.1); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("BetaQuantile(1,1,.1) = %.12f", got)
	}
}

func openExperienceTestRepository(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Append(context.Background(), ContextItem{ID: "memory-1", Kind: ContextPattern, Content: "safe reusable procedure", Scope: Scope{ProjectID: "project-1"}, Lifecycle: LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	return repo
}
