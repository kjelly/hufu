package context

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const consolidationTestPolicy = "memory-policy-v1"

var consolidationTestScope = Scope{ProjectID: "project", TeamID: "team"}

func newConsolidationTestRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// seedConsolidationSource appends a confirmed persistent decision with two
// verified independent-task observations, which meets the default support
// thresholds.
func seedConsolidationSource(t *testing.T, repo *SQLiteRepository, id, content string, metadata map[string]string) ContextItem {
	t.Helper()
	ctx := context.Background()
	meta := map[string]string{"memory_lifetime": "persistent"}
	for key, value := range metadata {
		meta[key] = value
	}
	if err := repo.Append(ctx, ContextItem{ID: id, Kind: ContextDecision, Content: content, Scope: consolidationTestScope, Lifecycle: LifecycleConfirmed, Confidence: 0.9, Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := repo.ApplyExperienceObservation(ctx, ExperienceObservation{
			IdempotencyKey: fmt.Sprintf("%s-%d", id, i), ContextItemID: id, PolicyVersion: consolidationTestPolicy,
			ProjectID: consolidationTestScope.ProjectID, TaskID: fmt.Sprintf("task-%d", i), AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	item, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func consolidationInput(text string, ids ...string) ConsolidationCreateInput {
	return ConsolidationCreateInput{
		ConsolidationSourceSelection: ConsolidationSourceSelection{
			ProjectID: consolidationTestScope.ProjectID, TeamID: consolidationTestScope.TeamID, SourceIDs: ids,
			PolicyVersion: consolidationTestPolicy, Support: ConsolidationSupportPolicy{MinConfirmedSupport: 2, MinIndependentTasks: 2},
		},
		Text: text, Origin: "operator",
	}
}

func consolidationReview(id, reason string) ConsolidationReviewInput {
	return ConsolidationReviewInput{ProposalID: id, ProjectID: consolidationTestScope.ProjectID, PolicyVersion: consolidationTestPolicy, Actor: "test", Reason: reason}
}

func seedTwoConsolidationSources(t *testing.T, repo *SQLiteRepository) {
	t.Helper()
	seedConsolidationSource(t, repo, "src-a", "Run go test before committing.", nil)
	seedConsolidationSource(t, repo, "src-b", "Run go vet before committing.", nil)
}

func mustCreateConsolidation(t *testing.T, repo *SQLiteRepository, text string, ids ...string) ConsolidationProposal {
	t.Helper()
	proposal, created, err := repo.CreateConsolidationProposal(context.Background(), consolidationInput(text, ids...))
	if err != nil || !created {
		t.Fatalf("create consolidation: created=%v err=%v", created, err)
	}
	return proposal
}

func countRows(t *testing.T, repo *SQLiteRepository, query string, args ...any) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustRevision(t *testing.T, repo *SQLiteRepository) int64 {
	t.Helper()
	revision, err := repo.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestCreateConsolidationProposalPersistsEveryRecord(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "Run go test and go vet before committing.", "src-b", "src-a")
	got, err := repo.GetConsolidationProposal(context.Background(), proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.SourceIDs, ",") != "src-a,src-b" || got.Status != ConsolidationStatusProposed || got.TeamID != "team" {
		t.Fatalf("proposal = %+v", got)
	}
	if got.AggregateRevisions["src-a"] == 0 || got.SourceRevisions["src-a"] == "" {
		t.Fatalf("proposal did not freeze source revisions: %+v", got)
	}
	candidate, err := repo.Get(context.Background(), got.CandidateContextItemID)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Lifecycle != LifecycleCandidate || candidate.Source != (SourceRef{Type: SourceTypeConsolidationProposal, Ref: proposal.ID}) || candidate.Confidence != 0.9 || candidate.Metadata["derived_from"] != "src-a,src-b" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if n := countRows(t, repo, "SELECT COUNT(*) FROM context_edges WHERE from_id=? AND relation='derived_from'", candidate.ID); n != 2 {
		t.Fatalf("derived_from edges = %d, want 2", n)
	}
	if n := countRows(t, repo, "SELECT COUNT(*) FROM context_events WHERE event_type='consolidation_proposed' AND item_id=?", candidate.ID); n != 1 {
		t.Fatalf("consolidation_proposed events = %d, want 1", n)
	}
	freshness, err := repo.EvaluateConsolidationProposal(context.Background(), got, consolidationTestPolicy, true)
	if err != nil || freshness.State != ConsolidationFresh {
		t.Fatalf("freshness = %+v err=%v", freshness, err)
	}
}

func TestCreateConsolidationProposalLeavesNothingOnFailure(t *testing.T) {
	for _, stage := range []string{"candidate", "edges", "proposal"} {
		t.Run(stage, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			before := mustRevision(t, repo)
			injected := errors.New("injected failure")
			consolidationTxTestHook = func(got string) error {
				if got == stage {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { consolidationTxTestHook = nil })
			if _, _, err := repo.CreateConsolidationProposal(context.Background(), consolidationInput("merged guidance", "src-a", "src-b")); !errors.Is(err, injected) {
				t.Fatalf("err = %v, want injected failure", err)
			}
			if after := mustRevision(t, repo); after != before {
				t.Fatalf("revision moved from %d to %d after a failed create", before, after)
			}
			for table, query := range map[string]string{
				"proposals":  "SELECT COUNT(*) FROM consolidation_proposals",
				"candidates": "SELECT COUNT(*) FROM context_items WHERE json_extract(source_json,'$.type')='consolidation_proposal'",
				"edges":      "SELECT COUNT(*) FROM context_edges WHERE relation='derived_from'",
			} {
				if n := countRows(t, repo, query); n != 0 {
					t.Fatalf("%s left behind: %d", table, n)
				}
			}
		})
	}
}

func TestCreateConsolidationProposalIsIdempotent(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	first := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	before := mustRevision(t, repo)
	for _, text := range []string{"merged guidance", "  merged guidance\r\n"} {
		again, created, err := repo.CreateConsolidationProposal(context.Background(), consolidationInput(text, "src-b", "src-a"))
		if err != nil || created || again.ID != first.ID {
			t.Fatalf("rerun with %q: id=%s created=%v err=%v, want existing %s", text, again.ID, created, err, first.ID)
		}
	}
	if after := mustRevision(t, repo); after != before {
		t.Fatalf("idempotent rerun wrote records: revision %d -> %d", before, after)
	}
}

func TestCreateConsolidationProposalRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, repo *SQLiteRepository)
		input  ConsolidationCreateInput
		target error
		text   string
	}{
		{name: "duplicates confirmed knowledge", setup: func(t *testing.T, repo *SQLiteRepository) {
			seedConsolidationSource(t, repo, "existing", "merged guidance", nil)
		}, input: consolidationInput("merged guidance", "src-a", "src-b"), target: ErrConsolidationCandidateDuplicate, text: "existing"},
		{name: "same text for another source set", setup: func(t *testing.T, repo *SQLiteRepository) {
			seedConsolidationSource(t, repo, "src-c", "Run gofmt before committing.", nil)
			mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-c")
		}, input: consolidationInput("merged guidance", "src-a", "src-b"), target: ErrConsolidationCandidateDuplicate},
		{name: "another pending proposal", setup: func(t *testing.T, repo *SQLiteRepository) {
			mustCreateConsolidation(t, repo, "first merged guidance", "src-a", "src-b")
		}, input: consolidationInput("second merged guidance", "src-a", "src-b"), target: ErrConsolidationPending},
		{name: "superseded source", setup: func(t *testing.T, repo *SQLiteRepository) {
			if err := repo.MarkSuperseded(context.Background(), []string{"src-b"}, "src-a"); err != nil {
				t.Fatal(err)
			}
		}, input: consolidationInput("merged guidance", "src-a", "src-b"), target: ErrConsolidationSourceInvalid, text: "not current confirmed knowledge"},
		{name: "missing source", input: consolidationInput("merged guidance", "src-a", "src-missing"), target: ErrConsolidationSourceInvalid, text: "source_missing"},
		{name: "insufficient support", setup: func(t *testing.T, repo *SQLiteRepository) {
			if err := repo.Append(context.Background(), ContextItem{ID: "src-weak", Kind: ContextDecision, Content: "weak", Scope: consolidationTestScope, Lifecycle: LifecycleConfirmed}); err != nil {
				t.Fatal(err)
			}
		}, input: consolidationInput("merged guidance", "src-a", "src-weak"), target: ErrConsolidationSourceInvalid, text: "support_insufficient"},
		{name: "contradictory sources", setup: func(t *testing.T, repo *SQLiteRepository) {
			seedConsolidationSource(t, repo, "src-x", "Never run go vet.", map[string]string{"contradicts_ids": "src-a"})
		}, input: consolidationInput("merged guidance", "src-a", "src-x"), target: ErrConsolidationSourceInvalid, text: "contradictory"},
		{name: "widening scope", setup: func(t *testing.T, repo *SQLiteRepository) {
			if err := repo.Append(context.Background(), ContextItem{ID: "src-other-team", Kind: ContextDecision, Content: "other team", Scope: Scope{ProjectID: "project", TeamID: "other"}, Lifecycle: LifecycleConfirmed}); err != nil {
				t.Fatal(err)
			}
		}, input: consolidationInput("merged guidance", "src-a", "src-other-team"), target: ErrConsolidationSourceInvalid, text: "widen"},
		{name: "secret text", input: consolidationInput("use password=SuperSecret!123 for deploys", "src-a", "src-b"), text: "secret-like"},
		{name: "single source", input: consolidationInput("merged guidance", "src-a", "src-a"), text: "at least two"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			if tc.setup != nil {
				tc.setup(t, repo)
			}
			before := mustRevision(t, repo)
			_, _, err := repo.CreateConsolidationProposal(context.Background(), tc.input)
			if err == nil || (tc.target != nil && !errors.Is(err, tc.target)) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.target, tc.text)
			}
			if after := mustRevision(t, repo); after != before {
				t.Fatalf("rejected create wrote records: revision %d -> %d", before, after)
			}
		})
	}
}

func TestCreateConsolidationProposalRefusesReviewedIdentity(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	if _, err := repo.RejectConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "not useful")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CreateConsolidationProposal(context.Background(), consolidationInput("merged guidance", "src-a", "src-b")); !errors.Is(err, ErrConsolidationProposalExists) {
		t.Fatalf("recreate after reject err = %v", err)
	}
	candidate, err := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if err != nil || candidate.Lifecycle != LifecycleRejected {
		t.Fatalf("rejected candidate reopened: %+v err=%v", candidate, err)
	}
}

func TestCreateConsolidationProposalRefusesOpenConflict(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	a, _ := repo.Get(context.Background(), "src-a")
	b, _ := repo.Get(context.Background(), "src-b")
	j := PairJudgment{ProjectID: "project", TeamID: "team", ItemAID: a.ID, ItemBID: b.ID, ItemAContentHash: a.ContentHash, ItemBContentHash: b.ContentHash, Verdict: PairVerdictContradicts, JudgePolicyVersion: CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
	NormalizePairJudgment(&j)
	if _, _, err := repo.SavePairJudgment(context.Background(), j, "operator", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.CreateConsolidationProposal(context.Background(), consolidationInput("merged guidance", "src-a", "src-b")); !errors.Is(err, ErrConsolidationSourceInvalid) || !strings.Contains(err.Error(), "unresolved memory conflict") {
		t.Fatalf("err = %v", err)
	}
}

func TestApproveConsolidationProposal(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	approved, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "explicit operator approval"))
	if err != nil || approved.Status != ConsolidationStatusApproved || approved.ReviewedAt == nil {
		t.Fatalf("approve = %+v err=%v", approved, err)
	}
	candidate, err := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if err != nil || candidate.Lifecycle != LifecycleConfirmed || candidate.Metadata["approved_by"] != "test" {
		t.Fatalf("candidate = %+v err=%v", candidate, err)
	}
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "again")); !errors.Is(err, ErrConsolidationNotPending) {
		t.Fatalf("second approve err = %v", err)
	}
}

func TestApproveConsolidationProposalRefusesChangedEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, repo *SQLiteRepository)
		text   string
	}{
		{name: "source aggregate moved", change: func(t *testing.T, repo *SQLiteRepository) {
			if _, err := repo.ApplyExperienceObservation(context.Background(), ExperienceObservation{IdempotencyKey: "extra", ContextItemID: "src-a", PolicyVersion: consolidationTestPolicy, ProjectID: "project", TaskID: "task-9", ExposureDelta: 1}); err != nil {
				t.Fatal(err)
			}
		}, text: "aggregate revision changed"},
		{name: "source revision changed", change: func(t *testing.T, repo *SQLiteRepository) {
			if _, err := repo.db.Exec("UPDATE context_items SET content_hash='changed' WHERE id='src-b'"); err != nil {
				t.Fatal(err)
			}
		}, text: "revision changed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
			tc.change(t, repo)
			if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); !errors.Is(err, ErrConsolidationSourceInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("err = %v, want %q", err, tc.text)
			}
			candidate, _ := repo.Get(context.Background(), proposal.CandidateContextItemID)
			got, _ := repo.GetConsolidationProposal(context.Background(), proposal.ID)
			if candidate.Lifecycle != LifecycleCandidate || got.Status != ConsolidationStatusProposed {
				t.Fatalf("failed approve changed state: candidate=%s proposal=%s", candidate.Lifecycle, got.Status)
			}
		})
	}
}

func TestApproveConsolidationProposalIsAtomic(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	injected := errors.New("injected failure")
	consolidationTxTestHook = func(stage string) error {
		if stage == "approve_candidate" {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { consolidationTxTestHook = nil })
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); !errors.Is(err, injected) {
		t.Fatalf("err = %v", err)
	}
	consolidationTxTestHook = nil
	candidate, _ := repo.Get(context.Background(), proposal.CandidateContextItemID)
	got, _ := repo.GetConsolidationProposal(context.Background(), proposal.ID)
	if candidate.Lifecycle != LifecycleCandidate || got.Status != ConsolidationStatusProposed {
		t.Fatalf("partial approve: candidate=%s proposal=%s", candidate.Lifecycle, got.Status)
	}
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); err != nil {
		t.Fatalf("retry approve err = %v", err)
	}
}

func TestRejectConsolidationProposal(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	rejected, err := repo.RejectConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "not useful"))
	if err != nil || rejected.Status != ConsolidationStatusRejected {
		t.Fatalf("reject = %+v err=%v", rejected, err)
	}
	candidate, _ := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if candidate.Lifecycle != LifecycleRejected || candidate.Metadata["rejection_reason"] != "not useful" || !hasEvidenceType(candidate, EvidenceTypeOperatorRejection) {
		t.Fatalf("rejected candidate = %+v", candidate)
	}
	before := mustRevision(t, repo)
	if _, err := repo.RejectConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "again")); !errors.Is(err, ErrConsolidationNotPending) {
		t.Fatalf("second reject err = %v", err)
	}
	if after := mustRevision(t, repo); after != before {
		t.Fatalf("second reject wrote records")
	}
}

func TestRejectApprovedConsolidationKeepsKnowledge(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RejectConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "late reject")); !errors.Is(err, ErrConsolidationNotPending) || !strings.Contains(err.Error(), "supersede") {
		t.Fatalf("reject approved err = %v", err)
	}
	candidate, _ := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if candidate.Lifecycle != LifecycleConfirmed {
		t.Fatalf("approved knowledge became %s", candidate.Lifecycle)
	}
}

func TestConsolidationReviewErrors(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	missing := consolidationReview("consolidation-missing", "x")
	otherProject := consolidationReview(proposal.ID, "x")
	otherProject.ProjectID = "other"
	for _, review := range []ConsolidationReviewInput{missing, otherProject} {
		if _, err := repo.ApproveConsolidationProposal(context.Background(), review); !errors.Is(err, ErrConsolidationNotFound) {
			t.Fatalf("approve %s/%s err = %v", review.ProjectID, review.ProposalID, err)
		}
		if _, err := repo.RejectConsolidationProposal(context.Background(), review); !errors.Is(err, ErrConsolidationNotFound) {
			t.Fatalf("reject %s/%s err = %v", review.ProjectID, review.ProposalID, err)
		}
	}
	if _, err := repo.db.Exec("UPDATE context_items SET source_json=? WHERE id=?", mustJSON(SourceRef{Type: SourceTypeConsolidationProposal, Ref: "consolidation-other"}), proposal.CandidateContextItemID); err != nil {
		t.Fatal(err)
	}
	for name, review := range map[string]func(context.Context, ConsolidationReviewInput) (ConsolidationProposal, error){"approve": repo.ApproveConsolidationProposal, "reject": repo.RejectConsolidationProposal} {
		if _, err := review(context.Background(), consolidationReview(proposal.ID, "x")); !errors.Is(err, ErrConsolidationInconsistent) {
			t.Fatalf("%s with a mismatched candidate err = %v", name, err)
		}
	}
}

func hasEvidenceType(item ContextItem, evidenceType string) bool {
	for _, evidence := range item.Evidence {
		if evidence.Type == evidenceType {
			return true
		}
	}
	return false
}
