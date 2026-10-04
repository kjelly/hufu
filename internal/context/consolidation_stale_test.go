package context

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// seedVerifiedSource appends a confirmed persistent source with two verified
// independent-task observations, last verified at verifiedAt. A zero
// verifiedAt records the support without strong evidence, as rows written
// before strong evidence was recorded have.
func seedVerifiedSource(t *testing.T, repo *SQLiteRepository, id string, verifiedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := repo.Append(ctx, ContextItem{ID: id, Kind: ContextDecision, Content: "Guidance " + id, Scope: consolidationTestScope, Lifecycle: LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}); err != nil {
		t.Fatal(err)
	}
	observedAt := verifiedAt
	if observedAt.IsZero() {
		observedAt = time.Now().Add(-time.Hour)
	}
	for i := 1; i <= 2; i++ {
		if _, err := repo.ApplyExperienceObservation(ctx, ExperienceObservation{
			IdempotencyKey: fmt.Sprintf("%s-%d", id, i), ContextItemID: id, PolicyVersion: consolidationTestPolicy,
			ProjectID: consolidationTestScope.ProjectID, TaskID: fmt.Sprintf("task-%d", i), AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 1,
			StrongEvidence: !verifiedAt.IsZero(), ObservedAt: observedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func staleSourceReasons(err error) map[string][]ConsolidationReason {
	var sourceErr *ConsolidationSourceError
	if errors.As(err, &sourceErr) {
		return sourceErr.SourceReasons
	}
	return nil
}

// TestConsolidationCreateRequiresRecentStrongEvidence refuses a proposal
// whose sources were not verified within the stale-after window.
func TestConsolidationCreateRequiresRecentStrongEvidence(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		verifiedB  time.Time
		staleAfter time.Duration
		wantStale  bool
	}{
		{name: "both recently verified", verifiedB: now.Add(-time.Hour), staleAfter: 30 * 24 * time.Hour},
		{name: "one verified long ago", verifiedB: now.Add(-40 * 24 * time.Hour), staleAfter: 30 * 24 * time.Hour, wantStale: true},
		{name: "one with no recorded verification", staleAfter: 30 * 24 * time.Hour, wantStale: true},
		{name: "zero stale-after skips the check", staleAfter: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedVerifiedSource(t, repo, "src-a", now.Add(-time.Hour))
			seedVerifiedSource(t, repo, "src-b", tc.verifiedB)
			input := consolidationInput("merged guidance", "src-a", "src-b")
			input.Support.StaleAfter = tc.staleAfter
			_, created, err := repo.CreateConsolidationProposal(context.Background(), input)
			if !tc.wantStale {
				if err != nil || !created {
					t.Fatalf("create = %v, %v", created, err)
				}
				return
			}
			reasons := staleSourceReasons(err)
			if !errors.Is(err, ErrConsolidationSourceInvalid) || !slices.Equal(reasons["src-b"], []ConsolidationReason{ReasonStrongEvidenceStale}) || len(reasons["src-a"]) != 0 {
				t.Fatalf("err = %v reasons = %v, want only src-b %s", err, reasons, ReasonStrongEvidenceStale)
			}
			if got := countRows(t, repo, "SELECT COUNT(*) FROM consolidation_proposals"); got != 0 {
				t.Fatalf("a refused create stored %d proposals", got)
			}
		})
	}
}

// TestConsolidationApprovalRefusesEvidenceThatAged covers evidence that was
// recent at create but is too old at approval: nothing was written, so only
// the time check can see it. Inspection reports the same state.
func TestConsolidationApprovalRefusesEvidenceThatAged(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	verified := time.Now().Add(-2 * time.Hour)
	seedVerifiedSource(t, repo, "src-a", verified)
	seedVerifiedSource(t, repo, "src-b", verified)
	input := consolidationInput("merged guidance", "src-a", "src-b")
	input.Support.StaleAfter = 30 * 24 * time.Hour
	proposal, _, err := repo.CreateConsolidationProposal(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	// A one-hour window makes the two-hour-old verification stale.
	review := consolidationReview(proposal.ID, "approve")
	review.StaleAfter = time.Hour
	_, err = repo.ApproveConsolidationProposal(context.Background(), review)
	if reasons := staleSourceReasons(err); !errors.Is(err, ErrConsolidationSourceInvalid) || !slices.Contains(reasons["src-a"], ReasonStrongEvidenceStale) {
		t.Fatalf("approve err = %v, want %s", err, ReasonStrongEvidenceStale)
	}
	if got, _ := repo.GetConsolidationProposal(context.Background(), proposal.ID); got.Status != ConsolidationStatusProposed {
		t.Fatalf("refused approval changed the proposal to %s", got.Status)
	}
	freshness, err := repo.EvaluateConsolidationProposal(context.Background(), proposal, consolidationTestPolicy, false, time.Hour)
	if err != nil || freshness.State != ConsolidationStale || !slices.Contains(freshness.Reasons, ReasonStrongEvidenceStale) {
		t.Fatalf("inspection = %+v err=%v, want stale for %s", freshness, err, ReasonStrongEvidenceStale)
	}
	report, err := repo.ConsolidationDoctor(context.Background(), consolidationTestScope.ProjectID, consolidationTestScope.TeamID, false, consolidationTestPolicy, time.Hour)
	if err != nil || report.Counts[ConsolidationStale] != 1 {
		t.Fatalf("doctor = %+v err=%v, want one stale proposal", report.Counts, err)
	}
	review.StaleAfter = 30 * 24 * time.Hour
	if approved, err := repo.ApproveConsolidationProposal(context.Background(), review); err != nil || approved.Status != ConsolidationStatusApproved {
		t.Fatalf("approve within the window = %+v err=%v", approved, err)
	}
}
