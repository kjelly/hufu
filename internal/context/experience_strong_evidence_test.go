package context

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestStrongEvidenceRecency pins which observations move the strong-evidence
// time, and that the live reducer, a rebuild, and the in-memory reducer agree
// on it at the precision the projection stores.
func TestStrongEvidenceRecency(t *testing.T) {
	base := time.Date(2026, 3, 1, 9, 0, 0, 123456789, time.UTC)
	at := func(minutes int) time.Time { return base.Add(time.Duration(minutes) * time.Minute) }
	observation := func(key string, minutes int, edit func(*ExperienceObservation)) ExperienceObservation {
		o := ExperienceObservation{IdempotencyKey: key, ContextItemID: "memory-1", PolicyVersion: "v1", ProjectID: "project-1", TaskID: "task-" + key, ObservedAt: at(minutes)}
		edit(&o)
		return o
	}
	exposure := func(o *ExperienceObservation) { o.ExposureDelta = 1 }
	applied := func(o *ExperienceObservation) { o.AppliedDelta = 1 }
	verified := func(o *ExperienceObservation) {
		o.PositiveWeight, o.VerifiedSupportDelta, o.StrongEvidence = 1, 1, true
	}
	causalFailure := func(o *ExperienceObservation) {
		o.NegativeWeight, o.CausalFailureDelta, o.StrongEvidence = 1, 1, true
	}
	weakSuccess := func(o *ExperienceObservation) { o.PositiveWeight = 0.2 }

	cases := []struct {
		name         string
		observations []ExperienceObservation
		wantStrong   time.Time
		wantObserved time.Time
	}{
		{
			name:         "retrieval and use reports are not strong evidence",
			observations: []ExperienceObservation{observation("a", 0, exposure), observation("b", 5, applied), observation("c", 9, weakSuccess)},
			wantObserved: at(9),
		},
		{
			name:         "retrieval after verification does not refresh it",
			observations: []ExperienceObservation{observation("a", 0, verified), observation("b", 60, exposure), observation("c", 90, applied)},
			wantStrong:   at(0), wantObserved: at(90),
		},
		{
			name:         "causal failure is strong evidence",
			observations: []ExperienceObservation{observation("a", 0, exposure), observation("b", 30, causalFailure)},
			wantStrong:   at(30), wantObserved: at(30),
		},
		{
			// Reducers apply observations in key order, not time order.
			name:         "an older verification applied later keeps the newest",
			observations: []ExperienceObservation{observation("a", 45, verified), observation("b", 10, verified)},
			wantStrong:   at(45), wantObserved: at(45),
		},
	}
	stored := func(want time.Time) time.Time {
		if want.IsZero() {
			return want
		}
		return time.UnixMilli(want.UnixMilli()).UTC()
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := openExperienceTestRepository(t)
			for _, o := range tc.observations {
				if _, err := live.ApplyExperienceObservation(t.Context(), o); err != nil {
					t.Fatal(err)
				}
			}
			liveAggregates, err := live.ListExperienceAggregates(t.Context(), "v1")
			if err != nil {
				t.Fatal(err)
			}
			rebuilt := openExperienceTestRepository(t)
			if err := rebuilt.RebuildExperienceAggregates(t.Context(), tc.observations); err != nil {
				t.Fatal(err)
			}
			rebuiltAggregates, err := rebuilt.ListExperienceAggregates(t.Context(), "v1")
			if err != nil {
				t.Fatal(err)
			}
			reduced, err := ReduceExperienceAggregates(tc.observations)
			if err != nil {
				t.Fatal(err)
			}
			if len(liveAggregates) != 1 {
				t.Fatalf("live aggregates = %+v", liveAggregates)
			}
			got := liveAggregates[0]
			if !got.LastStrongEvidenceAt.Equal(stored(tc.wantStrong)) || got.LastStrongEvidenceAt.IsZero() != tc.wantStrong.IsZero() {
				t.Fatalf("last strong evidence = %v, want %v", got.LastStrongEvidenceAt, stored(tc.wantStrong))
			}
			if !got.LastObservedAt.Equal(stored(tc.wantObserved)) {
				t.Fatalf("last observed = %v, want %v", got.LastObservedAt, stored(tc.wantObserved))
			}
			if !reflect.DeepEqual(rebuiltAggregates, liveAggregates) || !reflect.DeepEqual(reduced, liveAggregates) {
				t.Fatalf("reducers disagree:\nlive=%#v\nrebuilt=%#v\nreduced=%#v", liveAggregates, rebuiltAggregates, reduced)
			}
		})
	}
}

// TestReadOnlyRepositoryBeforeStrongEvidenceMigration keeps inspection of a
// store that predates migration 12 working; it reads no strong evidence.
func TestReadOnlyRepositoryBeforeStrongEvidenceMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.sqlite")
	repo, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(ctx, ContextItem{ID: "memory-1", Kind: ContextPattern, Content: "safe reusable procedure", Scope: Scope{ProjectID: "project-1"}, Lifecycle: LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyExperienceObservation(ctx, ExperienceObservation{IdempotencyKey: "a", ContextItemID: "memory-1", PolicyVersion: "v1", TaskID: "task-1", PositiveWeight: 1, VerifiedSupportDelta: 1, StrongEvidence: true, ObservedAt: time.Unix(10, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.db.ExecContext(ctx, `ALTER TABLE experience_aggregates DROP COLUMN last_strong_evidence_at; DELETE FROM schema_migrations WHERE version >= 12`); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	aggregate, err := readOnly.ExperienceAggregate(ctx, "memory-1", "v1")
	if err != nil || aggregate.VerifiedSupportCount != 1 || !aggregate.LastStrongEvidenceAt.IsZero() {
		t.Fatalf("ExperienceAggregate = %+v, %v; want support and no strong evidence", aggregate, err)
	}
	listed, err := readOnly.ListExperienceAggregates(ctx, "v1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListExperienceAggregates = %+v, %v", listed, err)
	}
	scoped, err := readOnly.ListExperienceAggregatesForScope(ctx, "v1", Scope{ProjectID: "project-1"})
	if err != nil || len(scoped) != 1 {
		t.Fatalf("ListExperienceAggregatesForScope = %+v, %v", scoped, err)
	}
}
