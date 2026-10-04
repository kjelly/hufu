package promotion

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func proposedSkillProposal(t *testing.T, repo *contextstore.SQLiteRepository, search string) Proposal {
	t.Helper()
	a := Analyzer{Repo: repo, Generator: &fakeGenerator{result: DraftResult{Type: TypeSkill, SkillName: "generated-check", Draft: conflictGateDraft}}, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(context.Background(), AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].Status != StatusProposed {
		t.Fatalf("analyze proposals = %+v, want one proposed", result.Proposals)
	}
	return result.Proposals[0]
}

// TestApproveRefusesStaleEvidence pins B6: approval checks the same source
// evidence apply does, so a proposal whose source changed after analyze is
// marked stale instead of approved.
func TestApproveRefusesStaleEvidence(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, repo *contextstore.SQLiteRepository)
		want   string
	}{
		{name: "aggregate revision moved", want: "aggregate changed", change: func(t *testing.T, repo *contextstore.SQLiteRepository) {
			if _, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{
				IdempotencyKey: "later-use", ContextItemID: "pattern", PolicyVersion: "memory-policy-v1", ProjectID: "p",
				TaskID: "run-9/1", ObservedAt: time.Now().UTC(), AppliedDelta: 1,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "source superseded", want: "changed", change: func(t *testing.T, repo *contextstore.SQLiteRepository) {
			replacement := contextstore.ContextItem{ID: "replacement", Kind: contextstore.ContextPattern, Content: "Generate code, then run the verifier twice", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
			if err := repo.Append(context.Background(), replacement); err != nil {
				t.Fatal(err)
			}
			if err := repo.MarkSuperseded(context.Background(), []string{"pattern"}, "replacement"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "source retired", want: "expired", change: func(t *testing.T, repo *contextstore.SQLiteRepository) {
			if err := repo.RetireConfirmed(context.Background(), []string{"pattern"}, "found wrong"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, search := newSkillPromotionFixture(t)
			p := proposedSkillProposal(t, repo, search)
			tc.change(t, repo)
			svc := Service{Repo: repo}
			got, err := svc.Approve(ctx, p.ID, "p", "demo")
			if err == nil || !strings.Contains(err.Error(), "evidence is stale") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approve err = %v, want stale evidence mentioning %q", err, tc.want)
			}
			if got.Status != StatusStale {
				t.Fatalf("returned status = %s, want stale", got.Status)
			}
			stored, err := svc.Get(ctx, p.ID, "p", "demo")
			if err != nil || stored.Status != StatusStale {
				t.Fatalf("stored status = %s err=%v, want stale", stored.Status, err)
			}
		})
	}
}

// TestApproveBlocksOpenConflictWithoutStaling keeps a conflict a blocking
// check: the proposal stays proposed and approves once the conflict is
// dismissed, with no new analyze.
func TestApproveBlocksOpenConflictWithoutStaling(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	p := proposedSkillProposal(t, repo, search)
	j := seedConflict(t, repo)
	svc := Service{Repo: repo}
	if _, err := svc.Approve(ctx, p.ID, "p", "demo"); err == nil || !strings.Contains(err.Error(), "unresolved memory conflict") {
		t.Fatalf("approve err = %v, want conflict block", err)
	}
	if stored, err := svc.Get(ctx, p.ID, "p", "demo"); err != nil || stored.Status != StatusProposed {
		t.Fatalf("status after block = %s err=%v, want proposed", stored.Status, err)
	}
	if _, _, err := repo.DismissConflict(ctx, j.ID, "p", "demo", "Different stages of the workflow.", "operator"); err != nil {
		t.Fatal(err)
	}
	approved, err := svc.Approve(ctx, p.ID, "p", "demo")
	if err != nil || approved.Status != StatusApproved {
		t.Fatalf("approve after dismiss = %s err=%v", approved.Status, err)
	}
}

func TestApproveWithCurrentEvidenceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	p := proposedSkillProposal(t, repo, search)
	svc := Service{Repo: repo}
	for i := range 2 {
		approved, err := svc.Approve(ctx, p.ID, "p", "demo")
		if err != nil || approved.Status != StatusApproved {
			t.Fatalf("approve #%d = %s err=%v", i+1, approved.Status, err)
		}
	}
}
