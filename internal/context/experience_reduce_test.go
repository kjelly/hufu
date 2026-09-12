package context

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestReduceExperienceAggregatesMatchesRepositoryRebuild(t *testing.T) {
	observed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	observations := []ExperienceObservation{
		{IdempotencyKey: "b", ContextItemID: "memory-1", PolicyVersion: "v1", ProjectID: "project-1", TaskID: "task-1", ObservedAt: observed.Add(time.Second), AppliedDelta: 1, PositiveWeight: 0.5, PriorAlpha: 1, PriorBeta: 1, UtilityPercentile: 0.1},
		{IdempotencyKey: "a", ContextItemID: "memory-1", PolicyVersion: "v1", ProjectID: "project-1", TaskID: "task-1", ObservedAt: observed, ExposureDelta: 1, PriorAlpha: 1, PriorBeta: 1, UtilityPercentile: 0.1},
		{IdempotencyKey: "b", ContextItemID: "memory-1", PolicyVersion: "v1", ProjectID: "project-1", TaskID: "task-1", ObservedAt: observed.Add(time.Second), AppliedDelta: 1},
	}
	expected, err := ReduceExperienceAggregates(observations)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Append(t.Context(), ContextItem{ID: "memory-1", Kind: ContextPattern, Content: "procedure", Scope: Scope{ProjectID: "project-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildExperienceAggregates(t.Context(), observations); err != nil {
		t.Fatal(err)
	}
	actual, err := repo.ListExperienceAggregates(t.Context(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("repository aggregates = %#v, in-memory = %#v", actual, expected)
	}
}
