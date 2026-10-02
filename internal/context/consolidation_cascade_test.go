package context

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func consolidationState(t *testing.T, repo *SQLiteRepository, proposalID string) (string, ContextLifecycle) {
	t.Helper()
	proposal, err := repo.GetConsolidationProposal(context.Background(), proposalID)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if err != nil {
		t.Fatal(err)
	}
	return proposal.Status, candidate.Lifecycle
}

func approvedConsolidation(t *testing.T, repo *SQLiteRepository, text string, ids ...string) ConsolidationProposal {
	t.Helper()
	proposal := mustCreateConsolidation(t, repo, text, ids...)
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); err != nil {
		t.Fatal(err)
	}
	return proposal
}

func TestSupersedingASourceDemotesApprovedConsolidation(t *testing.T) {
	cases := []struct {
		name       string
		invalidate func(t *testing.T, repo *SQLiteRepository)
	}{
		{name: "mark superseded", invalidate: func(t *testing.T, repo *SQLiteRepository) {
			seedConsolidationSource(t, repo, "src-a2", "Run go test -race before committing.", nil)
			if err := repo.MarkSuperseded(context.Background(), []string{"src-a"}, "src-a2"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "retire", invalidate: func(t *testing.T, repo *SQLiteRepository) {
			if err := repo.RetireConfirmed(context.Background(), []string{"src-a"}, "stale guidance"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "confirm with supersedes_ids", invalidate: func(t *testing.T, repo *SQLiteRepository) {
			if err := repo.Append(context.Background(), ContextItem{ID: "src-a2", Kind: ContextDecision, Content: "Run go test -race before committing.", Scope: consolidationTestScope, Lifecycle: LifecycleCandidate, Metadata: map[string]string{"supersedes_ids": "src-a"}}); err != nil {
				t.Fatal(err)
			}
			if err := repo.ConfirmCandidates(context.Background(), []string{"src-a2"}, CandidateBinding{Evidence: EvidenceRef{Type: "evidence_manifest", Ref: "manifest"}}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			proposal := approvedConsolidation(t, repo, "merged guidance", "src-a", "src-b")
			tc.invalidate(t, repo)
			status, lifecycle := consolidationState(t, repo, proposal.ID)
			if status != ConsolidationStatusStale || lifecycle != LifecycleCandidate {
				t.Fatalf("after invalidation proposal=%s candidate=%s, want stale/candidate", status, lifecycle)
			}
			visible, err := repo.QuerySharedPersistentProjection(context.Background(), consolidationTestScope)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range visible {
				if item.ID == proposal.CandidateContextItemID {
					t.Fatalf("demoted consolidation is still prompt-eligible")
				}
			}
			if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "again")); !errors.Is(err, ErrConsolidationNotPending) {
				t.Fatalf("approve stale err = %v", err)
			}
			if _, err := repo.RejectConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "stale")); err != nil {
				t.Fatalf("reject stale err = %v", err)
			}
			if status, lifecycle = consolidationState(t, repo, proposal.ID); status != ConsolidationStatusRejected || lifecycle != LifecycleRejected {
				t.Fatalf("after reject proposal=%s candidate=%s", status, lifecycle)
			}
		})
	}
}

func TestDemotionIsTransitive(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	seedConsolidationSource(t, repo, "src-c", "Run gofmt before committing.", nil)
	inner := approvedConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	outer, _, err := repo.CreateConsolidationProposal(context.Background(), ConsolidationCreateInput{
		ConsolidationSourceSelection: ConsolidationSourceSelection{ProjectID: "project", TeamID: "team", SourceIDs: []string{inner.CandidateContextItemID, "src-c"}, PolicyVersion: consolidationTestPolicy},
		Text:                         "merged guidance with formatting", Origin: "operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(outer.ID, "approve")); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSuperseded(context.Background(), []string{"src-b"}, "src-a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{inner.ID, outer.ID} {
		if status, lifecycle := consolidationState(t, repo, id); status != ConsolidationStatusStale || lifecycle != LifecycleCandidate {
			t.Fatalf("%s proposal=%s candidate=%s, want stale/candidate", id, status, lifecycle)
		}
	}
	outerNow, _ := repo.GetConsolidationProposal(context.Background(), outer.ID)
	if outerNow.Reason != string(ReasonSourceNotConfirmed) {
		t.Fatalf("outer stale reason = %q", outerNow.Reason)
	}
}

func TestSupersedingASourceMarksPendingConsolidationStale(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	if err := repo.MarkSuperseded(context.Background(), []string{"src-b"}, "src-a"); err != nil {
		t.Fatal(err)
	}
	if status, lifecycle := consolidationState(t, repo, proposal.ID); status != ConsolidationStatusStale || lifecycle != LifecycleCandidate {
		t.Fatalf("proposal=%s candidate=%s", status, lifecycle)
	}
	if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); !errors.Is(err, ErrConsolidationNotPending) {
		t.Fatalf("approve err = %v", err)
	}
}

func TestDemotionLeavesMismatchedLegacyCandidateAlone(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	proposal := approvedConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	if _, err := repo.db.Exec("UPDATE context_items SET source_json=? WHERE id=?", mustJSON(SourceRef{Type: SourceTypeConsolidationProposal, Ref: "consolidation-other"}), proposal.CandidateContextItemID); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSuperseded(context.Background(), []string{"src-b"}, "src-a"); err != nil {
		t.Fatal(err)
	}
	if status, lifecycle := consolidationState(t, repo, proposal.ID); status != ConsolidationStatusStale || lifecycle != LifecycleConfirmed {
		t.Fatalf("proposal=%s candidate=%s, want stale with the candidate untouched", status, lifecycle)
	}
}

func TestConsolidationDoctorReportsWithoutWriting(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedTwoConsolidationSources(t, repo)
	seedConsolidationSource(t, repo, "src-c", "Run gofmt before committing.", nil)
	seedConsolidationSource(t, repo, "src-d", "Run golangci-lint before committing.", nil)
	fresh := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
	expired := approvedConsolidation(t, repo, "formatting and linting guidance", "src-c", "src-d")
	if _, err := repo.db.Exec("UPDATE context_items SET expires_at=1 WHERE id='src-d'"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.Exec(`INSERT INTO context_items(id,kind,content,content_hash,project_id,team_id,authority,trust_level,priority,source_json,created_at,updated_at,lifecycle) VALUES('orphan','decision','orphan','h','project','team','agent','internal',50,?,1,1,'candidate')`, mustJSON(SourceRef{Type: SourceTypeConsolidationProposal, Ref: "consolidation-gone"})); err != nil {
		t.Fatal(err)
	}
	before := mustRevision(t, repo)
	report, err := repo.ConsolidationDoctor(context.Background(), "project", "team", false, consolidationTestPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if after := mustRevision(t, repo); after != before {
		t.Fatalf("doctor wrote records: revision %d -> %d", before, after)
	}
	if report.Counts[ConsolidationFresh] != 1 || report.Counts[ConsolidationStale] != 1 || report.Healthy() {
		t.Fatalf("counts = %v healthy=%v", report.Counts, report.Healthy())
	}
	states := map[string]ConsolidationFreshness{}
	for _, proposal := range report.Proposals {
		states[proposal.ProposalID] = proposal
	}
	if states[fresh.ID].State != ConsolidationFresh || !slices.Contains(states[expired.ID].Reasons, ReasonSourceExpired) {
		t.Fatalf("proposals = %+v", report.Proposals)
	}
	if len(report.Orphans) != 1 || report.Orphans[0].ItemID != "orphan" || report.Orphans[0].Reason != ReasonOrphanCandidate {
		t.Fatalf("orphans = %+v", report.Orphans)
	}
	otherTeam, err := repo.ConsolidationDoctor(context.Background(), "project", "other", false, consolidationTestPolicy)
	if err != nil || len(otherTeam.Proposals) != 0 || len(otherTeam.Orphans) != 0 {
		t.Fatalf("other team report = %+v err=%v", otherTeam, err)
	}
}
