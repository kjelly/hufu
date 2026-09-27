package context

import (
	"context"
	"errors"
	"testing"
)

func TestUpsertCandidateIdentityRules(t *testing.T) {
	const content = "use the verified adapter"
	shared := SourceRef{Type: "shared_memory_candidate", Ref: "memory_save"}
	cases := []struct {
		name          string
		existing      SourceRef
		lifecycle     ContextLifecycle
		operatorBlock bool
		want          error
		wantLifecycle ContextLifecycle
		wantSource    string
	}{
		{name: "same source candidate refreshes", existing: shared, lifecycle: LifecycleCandidate, wantLifecycle: LifecycleCandidate, wantSource: shared.Type},
		{name: "other source candidate conflicts", existing: SourceRef{Type: "legacy_memory_record"}, lifecycle: LifecycleCandidate, want: ErrCandidateIdentityConflict, wantLifecycle: LifecycleCandidate, wantSource: "legacy_memory_record"},
		{name: "run rejection reopens", existing: shared, lifecycle: LifecycleRejected, wantLifecycle: LifecycleCandidate, wantSource: shared.Type},
		{name: "operator rejection is sticky", existing: shared, lifecycle: LifecycleCandidate, operatorBlock: true, want: ErrOperatorRejected, wantLifecycle: LifecycleRejected, wantSource: shared.Type},
		{name: "confirmed stays confirmed", existing: SourceRef{Type: "legacy_memory_record"}, lifecycle: LifecycleConfirmed, wantLifecycle: LifecycleConfirmed, wantSource: "legacy_memory_record"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			ctx := context.Background()
			if err := repo.Append(ctx, ContextItem{ID: "existing", Kind: ContextPattern, Content: content, Scope: consolidationTestScope, Source: tc.existing, Lifecycle: tc.lifecycle}); err != nil {
				t.Fatal(err)
			}
			if tc.operatorBlock {
				if err := repo.BindCandidates(ctx, []string{"existing"}, CandidateBinding{Evidence: EvidenceRef{Type: EvidenceTypeOperatorRejection, Ref: "not true"}}); err != nil {
					t.Fatal(err)
				}
				if err := repo.UpdateLifecycle(ctx, []string{"existing"}, LifecycleRejected); err != nil {
					t.Fatal(err)
				}
			}
			before := mustRevision(t, repo)
			_, err := repo.UpsertCandidate(ctx, ContextItem{Kind: ContextPattern, Content: content, Scope: consolidationTestScope, Source: shared, Lifecycle: LifecycleCandidate, Metadata: map[string]string{"run_id": "run-2"}})
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if tc.want != nil && mustRevision(t, repo) != before {
				t.Fatalf("refused upsert wrote records")
			}
			item, err := repo.Get(ctx, "existing")
			if err != nil {
				t.Fatal(err)
			}
			if item.Lifecycle != tc.wantLifecycle || item.Source.Type != tc.wantSource {
				t.Fatalf("existing = lifecycle %s source %s, want %s %s", item.Lifecycle, item.Source.Type, tc.wantLifecycle, tc.wantSource)
			}
			if tc.operatorBlock && !hasEvidenceType(item, EvidenceTypeOperatorRejection) {
				t.Fatalf("operator rejection evidence was wiped: %+v", item.Evidence)
			}
		})
	}
}
