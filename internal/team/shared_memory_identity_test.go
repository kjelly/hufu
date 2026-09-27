package team

import (
	"context"
	"errors"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// A model re-proposing a pending consolidation's exact text must not take the
// candidate over and have ConfirmRun confirm it without operator approval.
func TestSharedMemoryProposalCannotTakeOverConsolidationCandidate(t *testing.T) {
	ctx := context.Background()
	repo := sharedMemoryTestRepo(t)
	sqlRepo := repo.(*contextstore.SQLiteRepository)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	for _, id := range []string{"src-a", "src-b"} {
		if err := repo.Append(ctx, contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: "source " + id, Scope: scope, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
			t.Fatal(err)
		}
	}
	proposal, _, err := sqlRepo.CreateConsolidationProposal(ctx, contextstore.ConsolidationCreateInput{
		ConsolidationSourceSelection: contextstore.ConsolidationSourceSelection{ProjectID: "project", TeamID: "team", SourceIDs: []string{"src-a", "src-b"}, PolicyVersion: "memory-policy-v1"},
		Text:                         "use the verified adapter", Origin: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewSharedMemoryService(repo)
	if _, err := svc.Propose(ctx, SharedMemoryProposal{Scope: scope, Content: "use the verified adapter", Section: ltmSectionPatterns, Source: "memory_save", RunID: "run-1", TaskID: "task-1"}); !errors.Is(err, contextstore.ErrCandidateIdentityConflict) {
		t.Fatalf("Propose err = %v, want identity conflict", err)
	}
	if _, err := svc.ConfirmRun(ctx, SharedMemoryPromotion{Scope: scope, Manifest: &EvidenceManifest{RunID: "run-1", Status: "accepted", ManifestHash: "manifest-1"}}); err != nil {
		t.Fatal(err)
	}
	candidate, err := repo.Get(ctx, proposal.CandidateContextItemID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Lifecycle != contextstore.LifecycleCandidate || candidate.Source.Type != contextstore.SourceTypeConsolidationProposal || candidate.Metadata["run_id"] != "" {
		t.Fatalf("consolidation candidate was taken over: %+v", candidate)
	}
}
