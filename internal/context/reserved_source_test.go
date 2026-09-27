package context

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestGenericMethodsRefuseReservedSourceType(t *testing.T) {
	reserved := SourceRef{Type: SourceTypeConsolidationProposal, Ref: "consolidation-x"}
	binding := CandidateBinding{Evidence: EvidenceRef{Type: "operator_evidence", Ref: "bypass"}}
	cases := []struct {
		name string
		call func(repo *SQLiteRepository, candidateID string) error
		want error
	}{
		{name: "append", call: func(repo *SQLiteRepository, _ string) error {
			return repo.Append(context.Background(), ContextItem{Kind: ContextDecision, Content: "forged", Scope: consolidationTestScope, Source: reserved})
		}, want: ErrReservedSourceType},
		{name: "append reducer", call: func(repo *SQLiteRepository, _ string) error {
			return repo.AppendReducer(context.Background(), ContextItem{Kind: ContextDecision, Content: "forged", Scope: consolidationTestScope, Source: reserved})
		}, want: ErrReservedSourceType},
		{name: "upsert reserved candidate", call: func(repo *SQLiteRepository, _ string) error {
			_, err := repo.UpsertCandidate(context.Background(), ContextItem{Kind: ContextDecision, Content: "forged", Scope: consolidationTestScope, Source: reserved, Lifecycle: LifecycleCandidate})
			return err
		}, want: ErrReservedSourceType},
		{name: "upsert over reserved candidate", call: func(repo *SQLiteRepository, _ string) error {
			_, err := repo.UpsertCandidate(context.Background(), ContextItem{Kind: ContextDecision, Content: "merged guidance", Scope: consolidationTestScope, Source: SourceRef{Type: "shared_memory_candidate"}, Lifecycle: LifecycleCandidate})
			return err
		}, want: ErrCandidateIdentityConflict},
		{name: "confirm", call: func(repo *SQLiteRepository, id string) error {
			return repo.ConfirmCandidates(context.Background(), []string{id}, binding)
		}, want: ErrReservedSourceType},
		{name: "bind", call: func(repo *SQLiteRepository, id string) error {
			return repo.BindCandidates(context.Background(), []string{id}, binding)
		}, want: ErrReservedSourceType},
		{name: "reject", call: func(repo *SQLiteRepository, id string) error {
			return repo.UpdateLifecycle(context.Background(), []string{id}, LifecycleRejected)
		}, want: ErrReservedSourceType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
			before := mustRevision(t, repo)
			if err := tc.call(repo, proposal.CandidateContextItemID); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if after := mustRevision(t, repo); after != before {
				t.Fatalf("refused call wrote records: revision %d -> %d", before, after)
			}
			candidate, err := repo.Get(context.Background(), proposal.CandidateContextItemID)
			if err != nil || candidate.Lifecycle != LifecycleCandidate || candidate.Source.Type != SourceTypeConsolidationProposal {
				t.Fatalf("candidate changed: %+v err=%v", candidate, err)
			}
		})
	}
}

func TestAppendDeduplicationKeepsReservedProvenance(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	if err := repo.Append(context.Background(), ContextItem{Kind: ContextDecision, Content: "merged guidance", Scope: consolidationTestScope, Source: SourceRef{Type: "memory"}, Lifecycle: LifecycleCandidate}); err != nil {
		t.Fatal(err)
	}
	candidate, err := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if err != nil || candidate.Source != (SourceRef{Type: SourceTypeConsolidationProposal, Ref: proposal.ID}) {
		t.Fatalf("append rewrote reserved provenance: %+v err=%v", candidate.Source, err)
	}
}

func TestUpdateLifecycleOnlyRejectsCandidates(t *testing.T) {
	cases := []struct {
		name      string
		lifecycle ContextLifecycle
		target    ContextLifecycle
		id        string
		want      error
	}{
		{name: "candidate to rejected", lifecycle: LifecycleCandidate, target: LifecycleRejected, id: "item"},
		{name: "candidate to confirmed", lifecycle: LifecycleCandidate, target: LifecycleConfirmed, id: "item", want: ErrLifecycleTransition},
		{name: "rejected to confirmed", lifecycle: LifecycleRejected, target: LifecycleConfirmed, id: "item", want: ErrLifecycleTransition},
		{name: "confirmed to rejected", lifecycle: LifecycleConfirmed, target: LifecycleRejected, id: "item", want: ErrLifecycleTransition},
		{name: "rejected to rejected", lifecycle: LifecycleRejected, target: LifecycleRejected, id: "item", want: ErrLifecycleTransition},
		{name: "missing", lifecycle: LifecycleCandidate, target: LifecycleRejected, id: "missing", want: sql.ErrNoRows},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			if err := repo.Append(context.Background(), ContextItem{ID: "item", Kind: ContextDecision, Content: "item", Scope: consolidationTestScope, Lifecycle: tc.lifecycle}); err != nil {
				t.Fatal(err)
			}
			err := repo.UpdateLifecycle(context.Background(), []string{tc.id}, tc.target)
			if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			item, _ := repo.Get(context.Background(), "item")
			want := tc.lifecycle
			if tc.want == nil {
				want = tc.target
			}
			if item.Lifecycle != want {
				t.Fatalf("lifecycle = %s, want %s", item.Lifecycle, want)
			}
		})
	}
}
