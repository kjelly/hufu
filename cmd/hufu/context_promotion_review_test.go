package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/promotion"
)

func TestPromotionReviewApproveAndApplyRemainSeparate(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")
	helperSeedEligibleLTM(t, workspace, "project", "demo", "source-1", "api_key: sk-proj-abcdefghijklmnopqrstuvwxyz123456\x1b]52;c;ZXhmaWw=\x07")
	id := helperCreateProposalInRepo(t, workspace, search, "project", "demo", "coordinator.md", "## Policy\n- Never expose credentials in operator output.")
	setPromotionReviewScope(workspace, search)

	var output bytes.Buffer
	err := runPromotionReviewSession(t.Context(), promotionReviewSession{
		input: bufio.NewReader(strings.NewReader("a\n\n")), output: &output,
	}, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "sk-proj-") {
		t.Fatalf("review leaked a secret-like value: %s", output.String())
	}
	if strings.Contains(output.String(), "\x1b") || strings.Contains(output.String(), "ZXhmaWw=") {
		t.Fatalf("review emitted a terminal control sequence: %q", output.String())
	}
	if !strings.Contains(output.String(), "approved-not-applied") {
		t.Fatalf("review did not explain publication state: %s", output.String())
	}
	target := filepath.Join(search, "demo", "coordinator.md")
	beforeApply, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(beforeApply), "hufu-promotion") {
		t.Fatal("approval changed the target file")
	}

	output.Reset()
	err = runPromotionReviewSession(t.Context(), promotionReviewSession{
		input: bufio.NewReader(strings.NewReader("apply " + id + "\n")), output: &output,
	}, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	afterApply, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(afterApply), "hufu-promotion:"+id+":start") || !strings.Contains(output.String(), "Status: applied") {
		t.Fatalf("explicit apply did not publish proposal: output=%s target=%s", output.String(), afterApply)
	}
}

func TestPromotionReviewRejectsStaleIntentAndDoubleApproveIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")
	helperSeedEligibleLTM(t, workspace, "project", "demo", "source-1", "stable source")
	id := helperCreateProposalInRepo(t, workspace, search, "project", "demo", "coordinator.md", "## Policy\n- Original review draft.")
	setPromotionReviewScope(workspace, search)

	readOnly, err := openPromotionReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := readOnly.GetPromotion(t.Context(), id, "project", "demo")
	if err != nil {
		t.Fatal(err)
	}
	staleRevision, err := promotionReviewRevision(t.Context(), readOnly, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err = readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	draftFile := filepath.Join(t.TempDir(), "draft.md")
	if err = os.WriteFile(draftFile, []byte("## Policy\n- Updated review draft."), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, service, err := openPromotionContext(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Edit(t.Context(), id, "project", "demo", draftFile); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err = mutateReviewedPromotion(t.Context(), id, staleRevision, func(context.Context, promotion.Service) (promotion.Proposal, error) {
		called = true
		return promotion.Proposal{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed during review") || called {
		t.Fatalf("stale mutation result: called=%v err=%v", called, err)
	}

	idSource := helperCreateProposalInRepo(t, workspace, search, "project", "demo", "worker.md", "## Worker Policy\n- Review current evidence.")
	sourceRevision := loadPromotionReviewRevision(t, idSource)
	repo, err = contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ApplyExperienceObservation(t.Context(), contextstore.ExperienceObservation{
		IdempotencyKey: "source-changed-during-review", ContextItemID: "source-1", PolicyVersion: "memory-policy-v1",
		ProjectID: "project", TaskID: "task-new", AppliedDelta: 1, VerifiedSupportDelta: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	called = false
	_, err = mutateReviewedPromotion(t.Context(), idSource, sourceRevision, func(context.Context, promotion.Service) (promotion.Proposal, error) {
		called = true
		return promotion.Proposal{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "outcome evidence changed") || called {
		t.Fatalf("stale source mutation result: called=%v err=%v", called, err)
	}

	idTarget := helperCreateProposalInRepo(t, workspace, search, "project", "demo", "coordinator.md", "## Policy\n- Review the current target.")
	targetRevision := loadPromotionReviewRevision(t, idTarget)
	if err = os.WriteFile(filepath.Join(search, "demo", "coordinator.md"), []byte("---\nname: coordinator\nrole: coordinator\n---\nChanged concurrently."), 0o644); err != nil {
		t.Fatal(err)
	}
	called = false
	_, err = mutateReviewedPromotion(t.Context(), idTarget, targetRevision, func(context.Context, promotion.Service) (promotion.Proposal, error) {
		called = true
		return promotion.Proposal{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "target changed") || called {
		t.Fatalf("stale target mutation result: called=%v err=%v", called, err)
	}

	first, err := helperRunCLI("context", "promotion", "approve", id, "--workspace", workspace, "--project", "project", "--team", "demo", "--team-search-path", search)
	if err != nil {
		t.Fatal(err)
	}
	second, err := helperRunCLI("context", "promotion", "approve", id, "--workspace", workspace, "--project", "project", "--team", "demo", "--team-search-path", search)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "approved") || !strings.Contains(second, "approved") {
		t.Fatalf("idempotent approve output: first=%q second=%q", first, second)
	}
	repo, err = contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repo.PendingPromotionEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	for _, event := range pending {
		if event.EventType == "memory_promotion_approved" {
			t.Fatal("approved event should have been delivered exactly once, not left duplicated in outbox")
		}
	}
}

func loadPromotionReviewRevision(t *testing.T, id string) string {
	t.Helper()
	repository, err := openPromotionReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := repository.GetPromotion(t.Context(), id, promotionProject, promotionTeam)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := promotionReviewRevision(t.Context(), repository, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestPromotionReviewCommandRefusesNonTTYAndUnattended(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")
	setPromotionReviewScope(workspace, search)
	opts.unattended = false
	t.Cleanup(func() { opts.unattended = false })

	root := newRootCommand()
	root.SetIn(strings.NewReader("a\n"))
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs([]string{"context", "promotion", "review", "proposal", "--workspace", workspace, "--project", "project", "--team", "demo", "--team-search-path", search})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "interactive TTY") {
		t.Fatalf("non-TTY review error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "context.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("non-TTY review created a database: %v", err)
	}

	root = newRootCommand()
	root.SetIn(os.Stdin)
	root.SetArgs([]string{"context", "promotion", "review", "proposal", "--workspace", workspace, "--project", "project", "--team", "demo", "--team-search-path", search, "--unattended"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "requires a human operator") {
		t.Fatalf("unattended review error = %v", err)
	}
}

func setPromotionReviewScope(workspace, search string) {
	promotionWorkspace = workspace
	promotionProject = "project"
	promotionTeam = "demo"
	promotionSearchPath = search
	promotionPolicyVersion = "memory-policy-v1"
	promotionJSON = false
}
