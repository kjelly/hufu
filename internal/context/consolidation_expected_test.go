package context

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// TestCreateConsolidationRefusesSourcesThatMovedDuringDrafting pins B5: a
// drafted text records the source revisions it was generated from, and
// creation refuses sources that moved since, writing nothing. Operator text
// carries no expected revisions and keeps the threshold-only check.
func TestCreateConsolidationRefusesSourcesThatMovedDuringDrafting(t *testing.T) {
	cases := []struct {
		name        string
		expected    func(ConsolidationSourceRevisions) *ConsolidationSourceRevisions
		moveSourceA bool
		wantReason  ConsolidationReason
	}{
		{name: "aggregate moved during drafting", expected: keepRevisions, moveSourceA: true, wantReason: ReasonAggregateRevisionChanged},
		{name: "content changed during drafting", expected: func(r ConsolidationSourceRevisions) *ConsolidationSourceRevisions {
			r.Content = map[string]string{"src-a": "hash-before-edit", "src-b": r.Content["src-b"]}
			return &r
		}, wantReason: ReasonSourceRevisionChanged},
		{name: "sources unchanged", expected: keepRevisions},
		{name: "operator text ignores later evidence", expected: func(ConsolidationSourceRevisions) *ConsolidationSourceRevisions { return nil }, moveSourceA: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			input := consolidationInput("Run go test and go vet before committing.", "src-a", "src-b")
			_, _, revisions, err := repo.ValidateConsolidationSourceRevisions(ctx, input.ConsolidationSourceSelection)
			if err != nil {
				t.Fatal(err)
			}
			if tc.moveSourceA {
				if _, err = repo.ApplyExperienceObservation(ctx, ExperienceObservation{
					IdempotencyKey: "src-a-while-drafting", ContextItemID: "src-a", PolicyVersion: consolidationTestPolicy,
					ProjectID: consolidationTestScope.ProjectID, TaskID: "task-3", AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 1,
				}); err != nil {
					t.Fatal(err)
				}
			}
			input.Origin, input.DraftModel = "model", "test-model"
			if expected := tc.expected(revisions); expected != nil {
				input.Expected = expected
			} else {
				input.Origin, input.DraftModel = "operator", ""
			}
			_, created, err := repo.CreateConsolidationProposal(ctx, input)
			if tc.wantReason == "" {
				if err != nil || !created {
					t.Fatalf("create: created=%v err=%v, want created", created, err)
				}
				return
			}
			var sourceErr *ConsolidationSourceError
			if !errors.As(err, &sourceErr) || !slices.Contains(sourceErr.SourceReasons["src-a"], tc.wantReason) {
				t.Fatalf("create err = %v, want %s for src-a", err, tc.wantReason)
			}
			proposals, _, err := repo.ListConsolidationProposals(ctx, consolidationTestScope.ProjectID, consolidationTestScope.TeamID, false)
			if err != nil || len(proposals) != 0 {
				t.Fatalf("refused creation left proposals %+v (err %v)", proposals, err)
			}
			candidates, err := repo.Query(ctx, RepositoryQuery{Scope: consolidationTestScope, Visibility: VisibilityExact, IncludeCandidates: true, SourceTypes: []string{SourceTypeConsolidationProposal}})
			if err != nil || len(candidates) != 0 {
				t.Fatalf("refused creation left candidates %+v (err %v)", candidates, err)
			}
		})
	}
}

func keepRevisions(r ConsolidationSourceRevisions) *ConsolidationSourceRevisions { return &r }
