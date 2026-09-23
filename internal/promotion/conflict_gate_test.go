package promotion

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

const conflictGateDraft = "---\nname: generated-check\ndescription: Run the verified workflow.\n---\n1. Generate the code.\n2. Run the verifier."

// seedConflict adds a confirmed counterpart memory and an open contradiction
// between it and the promotion source "pattern".
func seedConflict(t *testing.T, repo *contextstore.SQLiteRepository) contextstore.PairJudgment {
	t.Helper()
	ctx := context.Background()
	counterpart := contextstore.ContextItem{ID: "counterpart", Kind: contextstore.ContextDecision, Content: "Never generate code before tests", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	if err := repo.Append(ctx, counterpart); err != nil {
		t.Fatal(err)
	}
	source, err := repo.Get(ctx, "pattern")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, "counterpart")
	if err != nil {
		t.Fatal(err)
	}
	j := contextstore.PairJudgment{ProjectID: "p", TeamID: "demo", ItemAID: source.ID, ItemBID: stored.ID, ItemAContentHash: source.ContentHash, ItemBContentHash: stored.ContentHash, Verdict: contextstore.PairVerdictContradicts, JudgePolicyVersion: contextstore.CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
	contextstore.NormalizePairJudgment(&j)
	if _, _, err = repo.SavePairJudgment(ctx, j, "operator", false); err != nil {
		t.Fatal(err)
	}
	return j
}

func approvedSkillProposal(t *testing.T, repo *contextstore.SQLiteRepository, search string) Proposal {
	t.Helper()
	ctx := context.Background()
	a := Analyzer{Repo: repo, Generator: &fakeGenerator{result: DraftResult{Type: TypeSkill, SkillName: "generated-check", Draft: conflictGateDraft}}, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Service{Repo: repo}.Approve(ctx, result.Proposals[0].ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEligibleSourcesExcludesUnresolvedConflict(t *testing.T) {
	repo, _ := newSkillPromotionFixture(t)
	seedConflict(t, repo)
	eligible, diagnostics, err := EligibleSources(context.Background(), repo, EligibilityOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1"}, agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 0 {
		t.Fatalf("eligible = %+v, want the conflicted source excluded", eligible)
	}
	found := false
	for _, d := range diagnostics {
		found = found || (d.SourceID == "pattern" && d.Reason == DiagnosticUnresolvedConflict)
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want unresolved_conflict for pattern", diagnostics)
	}
}

type failingConflictRepository struct{ *contextstore.SQLiteRepository }

func (failingConflictRepository) OpenConflictsForItems(context.Context, string, string, []string) (map[string][]string, error) {
	return nil, errors.New("conflict table unavailable")
}

func TestEligibleSourcesFailsClosedOnConflictQueryError(t *testing.T) {
	repo, _ := newSkillPromotionFixture(t)
	_, _, err := EligibleSources(context.Background(), failingConflictRepository{repo}, EligibilityOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1"}, agent.DefaultMemoryLearningPolicy())
	if err == nil || !strings.Contains(err.Error(), "conflict table unavailable") {
		t.Fatalf("err = %v, want fail-closed conflict query error", err)
	}
}

func TestApplyBlockedWhileConflictOpenThenSucceedsAfterDismiss(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	p := approvedSkillProposal(t, repo, search)
	j := seedConflict(t, repo)
	svc := Service{Repo: repo}
	registry := team.NewTeamRegistry([]string{search})
	if _, err := svc.Apply(ctx, p.ID, "p", "demo", registry); err == nil || !strings.Contains(err.Error(), "unresolved memory conflict") {
		t.Fatalf("apply err = %v, want conflict block", err)
	}
	current, err := svc.Get(ctx, p.ID, "p", "demo")
	if err != nil || current.Status != StatusApproved {
		t.Fatalf("status after block = %s err=%v, want approved", current.Status, err)
	}
	if _, statErr := os.Stat(filepath.Join(search, "demo", "skills", "generated-check", "SKILL.md")); !os.IsNotExist(statErr) {
		t.Fatalf("blocked apply wrote the target: %v", statErr)
	}
	if err = (Service{Repo: repo}).ValidateProposalEvidence(ctx, current); err == nil {
		t.Fatal("improve handoff evidence check ignored the open conflict")
	}
	if _, _, err = repo.DismissConflict(ctx, j.ID, "p", "demo", "Different stages of the workflow.", "operator"); err != nil {
		t.Fatal(err)
	}
	applied, err := svc.Apply(ctx, p.ID, "p", "demo", registry)
	if err != nil || applied.Proposal.Status != StatusApplied {
		t.Fatalf("apply after dismiss = %+v err=%v", applied.Proposal, err)
	}
}

func TestApplySucceedsAfterCounterpartSuperseded(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	p := approvedSkillProposal(t, repo, search)
	seedConflict(t, repo)
	replacement := contextstore.ContextItem{ID: "replacement", Kind: contextstore.ContextDecision, Content: "Generate code, then run tests", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	if err := repo.Append(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSuperseded(ctx, []string{"counterpart"}, "replacement"); err != nil {
		t.Fatal(err)
	}
	applied, err := Service{Repo: repo}.Apply(ctx, p.ID, "p", "demo", team.NewTeamRegistry([]string{search}))
	if err != nil || applied.Proposal.Status != StatusApplied {
		t.Fatalf("apply after supersede = %+v err=%v", applied.Proposal, err)
	}
}

func TestAlreadyWrittenApplyIgnoresNewConflict(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	p := approvedSkillProposal(t, repo, search)
	target := filepath.Join(search, "demo", "skills", "generated-check", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the file write but before the status commit.
	if err := os.WriteFile(target, []byte(conflictGateDraft), 0o644); err != nil {
		t.Fatal(err)
	}
	seedConflict(t, repo)
	applied, err := Service{Repo: repo}.Apply(ctx, p.ID, "p", "demo", team.NewTeamRegistry([]string{search}))
	if err != nil || !applied.AlreadyApplied || applied.Proposal.Status != StatusApplied {
		t.Fatalf("crash recovery = %+v err=%v, want already applied", applied, err)
	}
}
