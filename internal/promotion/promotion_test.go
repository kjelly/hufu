package promotion

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team"
)

type fakeGenerator struct {
	result DraftResult
	calls  int
}

type fakeTextGenerator struct{ text string }

func (f fakeTextGenerator) GenerateText(context.Context, string) (string, error) { return f.text, nil }

func (f *fakeGenerator) Generate(context.Context, DraftRequest) (DraftResult, error) {
	f.calls++
	return f.result, nil
}

func appendEligible(t *testing.T, repo *contextstore.SQLiteRepository, item contextstore.ContextItem) {
	t.Helper()
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		_, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{IdempotencyKey: item.ID + string(rune('0'+i)), ContextItemID: item.ID, PolicyVersion: "memory-policy-v1", ProjectID: item.Scope.ProjectID, TaskID: "task-" + string(rune('0'+i)), AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 1})
		if err != nil {
			t.Fatal(err)
		}
	}
}
func writeTeam(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"coordinator.md": "---\nname: coordinator\nrole: coordinator\n---\nCoordinate.", "worker.md": "---\nname: worker\nrole: worker\n---\nWork."} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEligibilityHardGatesAndScope(t *testing.T) {
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	base := contextstore.ContextItem{Kind: contextstore.ContextPattern, Content: "Run two verified checks", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	base.ID = "eligible"
	appendEligible(t, repo, base)
	bad := base
	bad.ID = "session"
	bad.Content = "session only"
	bad.Metadata = map[string]string{"memory_lifetime": "session"}
	appendEligible(t, repo, bad)
	secret := base
	secret.ID = "secret"
	secret.Content = "api_token=super-secret-value"
	appendEligible(t, repo, secret)
	other := base
	other.ID = "other-team"
	other.Scope.TeamID = "other"
	appendEligible(t, repo, other)
	got, diagnostics, err := EligibleSources(context.Background(), repo, EligibilityOptions{ProjectID: "p", TeamID: "team", PolicyVersion: "memory-policy-v1"}, agent.DefaultMemoryLearningPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Item.ID != "eligible" {
		t.Fatalf("eligible=%v", got)
	}
	if len(diagnostics) != 1 || diagnostics[0].SourceID != "secret" || diagnostics[0].Reason != "secret_like_content" {
		t.Fatalf("diagnostics=%v", diagnostics)
	}
}

func TestAnalyzeApproveApplyPolicyAndRetry(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	search := t.TempDir()
	writeTeam(t, search)
	item := contextstore.ContextItem{ID: "decision", Kind: contextstore.ContextDecision, Content: "Always run the focused verifier", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	appendEligible(t, repo, item)
	g := &fakeGenerator{result: DraftResult{Type: TypeTeamPolicy, Draft: "## Promoted policy: verifier\nRun the focused verifier before completion."}}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 1 || g.calls != 1 {
		t.Fatalf("result=%#v calls=%d", result, g.calls)
	}
	target := filepath.Join(search, "demo", "coordinator.md")
	before, _ := os.ReadFile(target)
	if strings.Contains(string(before), "hufu-promotion") {
		t.Fatal("analyze modified target")
	}
	svc := Service{Repo: repo}
	approved, err := svc.Approve(ctx, result.Proposals[0].ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	afterApprove, _ := os.ReadFile(target)
	if string(afterApprove) != string(before) {
		t.Fatal("approve modified target")
	}
	registry := team.NewTeamRegistry([]string{search})
	applied, err := svc.Apply(ctx, approved.ID, "p", "demo", registry)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Proposal.Status != StatusApplied {
		t.Fatal(applied.Proposal.Status)
	}
	content, _ := os.ReadFile(target)
	if strings.Count(string(content), "hufu-promotion:"+approved.ID+":start") != 1 {
		t.Fatalf("target=%s", content)
	}
	retry, err := svc.Apply(ctx, approved.ID, "p", "demo", registry)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.AlreadyApplied {
		t.Fatal("expected idempotent retry")
	}
	content2, _ := os.ReadFile(target)
	if string(content2) != string(content) {
		t.Fatal("retry appended duplicate")
	}
	events, err := repo.PendingPromotionEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("lifecycle events=%d want proposed, approved, applied", len(events))
	}
}

func TestApplyRecoversFileWrittenBeforeStatusCommit(t *testing.T) {
	ctx := context.Background()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	search := t.TempDir()
	writeTeam(t, search)
	item := contextstore.ContextItem{ID: "crash-source", Kind: contextstore.ContextDecision, Content: "Recover promotion status without duplicate append", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	appendEligible(t, repo, item)
	g := &fakeGenerator{result: DraftResult{Type: TypeTeamPolicy, Draft: "## Recovery\nDo not append this block twice."}}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, result.Proposals[0].ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(search, "demo", "coordinator.md")
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	written := string(original) + "\n\n" + policyBlock(p) + "\n"
	if err = os.WriteFile(target, []byte(written), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered, err := svc.Apply(ctx, p.ID, "p", "demo", team.NewTeamRegistry([]string{search}))
	if err != nil {
		t.Fatal(err)
	}
	if !recovered.AlreadyApplied || recovered.Proposal.Status != StatusApplied {
		t.Fatalf("recovered=%#v", recovered)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(after), "hufu-promotion:"+p.ID+":start") != 1 {
		t.Fatalf("duplicate block: %s", after)
	}
}

func TestApplyMarksStaleEvidenceWithoutWriting(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	search := t.TempDir()
	writeTeam(t, search)
	item := contextstore.ContextItem{ID: "decision", Kind: contextstore.ContextDecision, Content: "Use stable behavior", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	appendEligible(t, repo, item)
	g := &fakeGenerator{result: DraftResult{Type: TypeTeamPolicy, Draft: "## Stable\nKeep behavior stable."}}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	r, err := a.Analyze(ctx, AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, r.Proposals[0].ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.ApplyExperienceObservation(ctx, contextstore.ExperienceObservation{IdempotencyKey: "later", ContextItemID: item.ID, PolicyVersion: "memory-policy-v1", ProjectID: "p", TaskID: "task-3", AppliedDelta: 1})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(search, "demo", "coordinator.md")
	before, _ := os.ReadFile(target)
	_, err = svc.Apply(ctx, p.ID, "p", "demo", team.NewTeamRegistry([]string{search}))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("err=%v", err)
	}
	after, _ := os.ReadFile(target)
	if string(after) != string(before) {
		t.Fatal("stale apply wrote target")
	}
	stored, err := svc.Get(ctx, p.ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusStale {
		t.Fatal(stored.Status)
	}
}

func TestJSONDraftGeneratorFailsClosed(t *testing.T) {
	cases := []string{`{"type":"team_policy","agent_id":"","skill_name":"","draft":"ok","steps":[],"unexpected":true}`, "```json\n{}\n```", `{"type":"unknown","draft":"x"} trailing`}
	for _, raw := range cases {
		g := JSONDraftGenerator{Generator: fakeTextGenerator{text: raw}}
		if _, err := g.Generate(context.Background(), DraftRequest{AllowedTypes: []Type{TypeTeamPolicy}}); err == nil {
			t.Fatalf("Generate(%q) succeeded", raw)
		}
	}
}

func TestAnalyzeApproveApplySkill(t *testing.T) {
	ctx := context.Background()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	search := t.TempDir()
	writeTeam(t, search)
	item := contextstore.ContextItem{ID: "pattern", Kind: contextstore.ContextPattern, Content: "Generate code then run two verification commands", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	appendEligible(t, repo, item)
	draft := "---\nname: generated-check\ndescription: Run the verified generation workflow.\n---\n## Steps\n\n1. Generate the code.\n2. Run the verifier."
	g := &fakeGenerator{result: DraftResult{Type: TypeSkill, SkillName: "generated-check", Draft: draft, Steps: []string{"Generate code", "Run verifier"}}}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err != nil {
		t.Fatal(err)
	}
	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, result.Proposals[0].ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	applied, err := svc.Apply(ctx, p.ID, "p", "demo", team.NewTeamRegistry([]string{search}))
	if err != nil {
		t.Fatal(err)
	}
	if applied.Proposal.Status != StatusApplied {
		t.Fatal(applied.Proposal.Status)
	}
	path := filepath.Join(search, "demo", "skills", "generated-check", "SKILL.md")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != draft {
		t.Fatalf("skill=%q", got)
	}
	found := false
	for _, def := range skill.DiscoverSkills([]string{filepath.Join(search, "demo", "skills")}, false) {
		found = found || def.Name == "generated-check"
	}
	if !found {
		t.Fatal("applied skill not discoverable")
	}
}

func TestAnalyzeApproveApplyTemplatedTeam(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	teamDir := filepath.Join(search, "templated-demo")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	teamYAML := `name: templated-demo
vars:
  project_name: hufu-super
  target_env: production
`
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(teamYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	coordMD := `---
name: coordinator
role: coordinator
tools: ask_user
---
Coordinator for {@ .project_name @} targeting {@ .target_env @}.
`
	if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte(coordMD), 0o644); err != nil {
		t.Fatal(err)
	}

	item := contextstore.ContextItem{
		ID:        "tmpl-decision",
		Kind:      contextstore.ContextDecision,
		Content:   "Always check staging deployment before production release",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "templated-demo"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	g := &fakeGenerator{
		result: DraftResult{
			Type:  TypeTeamPolicy,
			Draft: "## Promoted policy: release gate\nCheck staging deployment before production release.",
		},
	}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{
		ProjectID:     "p",
		TeamID:        "templated-demo",
		PolicyVersion: "memory-policy-v1",
		TeamDir:       teamDir,
	})
	if err != nil {
		t.Fatalf("Analyze failed on templated team: %v", err)
	}
	if len(result.Proposals) != 1 {
		t.Fatalf("proposals = %d, want 1", len(result.Proposals))
	}

	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, result.Proposals[0].ID, "p", "templated-demo")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	registry := team.NewTeamRegistry([]string{search})
	applied, err := svc.Apply(ctx, p.ID, "p", "templated-demo", registry)
	if err != nil {
		t.Fatalf("Apply on templated team failed: %v", err)
	}
	if applied.Proposal.Status != StatusApplied {
		t.Fatalf("status = %s, want applied", applied.Proposal.Status)
	}

	// Verify target file after application passes ValidateAgentFileWithVars
	targetPath := filepath.Join(teamDir, "coordinator.md")
	vars, err := team.ResolveTeamTemplateVars(teamDir, nil)
	if err != nil {
		t.Fatalf("ResolveTeamTemplateVars: %v", err)
	}
	if _, err := team.ValidateAgentFileWithVars(targetPath, vars); err != nil {
		t.Fatalf("applied target fails ValidateAgentFileWithVars: %v", err)
	}
}

func TestAnalyzeApproveApplyAgentPolicy(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	teamDir := filepath.Join(search, "multi-agent")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"coordinator.md": "---\nname: coordinator\nrole: coordinator\n---\nCoordinate.",
		"worker-a.md":    "---\nname: worker-a\nrole: worker\n---\nWorker A role.",
		"worker-b.md":    "---\nname: worker-b\nrole: worker\n---\nWorker B role.",
	} {
		if err := os.WriteFile(filepath.Join(teamDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	item := contextstore.ContextItem{
		ID:        "worker-a-item",
		Kind:      contextstore.ContextInstruction,
		Content:   "Worker A must always format output with markdown tables",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "multi-agent", AgentID: "worker-a"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	g := &fakeGenerator{
		result: DraftResult{
			Type:    TypeAgentPolicy,
			AgentID: "worker-a",
			Draft:   "## Promoted policy: markdown tables\nFormat output with markdown tables.",
		},
	}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{
		ProjectID:     "p",
		TeamID:        "multi-agent",
		PolicyVersion: "memory-policy-v1",
		TeamDir:       teamDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Proposals) != 1 || result.Proposals[0].TargetPath != "worker-a.md" {
		t.Fatalf("proposal target=%v, want worker-a.md", result.Proposals)
	}

	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, result.Proposals[0].ID, "p", "multi-agent")
	if err != nil {
		t.Fatal(err)
	}

	registry := team.NewTeamRegistry([]string{search})
	applied, err := svc.Apply(ctx, p.ID, "p", "multi-agent", registry)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Proposal.Status != StatusApplied {
		t.Fatal(applied.Proposal.Status)
	}

	// Verify worker-a was modified with promotion marker
	workerAContent, _ := os.ReadFile(filepath.Join(teamDir, "worker-a.md"))
	if !strings.Contains(string(workerAContent), "hufu-promotion:"+p.ID+":start") {
		t.Fatalf("worker-a missing promotion block: %s", workerAContent)
	}

	// Verify coordinator.md and worker-b.md are completely untouched
	coordContent, _ := os.ReadFile(filepath.Join(teamDir, "coordinator.md"))
	if string(coordContent) != "---\nname: coordinator\nrole: coordinator\n---\nCoordinate." {
		t.Fatalf("coordinator.md was unexpectedly modified: %s", coordContent)
	}
	workerBContent, _ := os.ReadFile(filepath.Join(teamDir, "worker-b.md"))
	if string(workerBContent) != "---\nname: worker-b\nrole: worker\n---\nWorker B role." {
		t.Fatalf("worker-b.md was unexpectedly modified: %s", workerBContent)
	}
}

func TestRejectPreservesSourceAndTarget(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	writeTeam(t, search)

	item := contextstore.ContextItem{
		ID:        "reject-item",
		Kind:      contextstore.ContextDecision,
		Content:   "Reject this proposed policy",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "demo"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	g := &fakeGenerator{
		result: DraftResult{
			Type:  TypeTeamPolicy,
			Draft: "## Promoted policy: reject\nDraft to be rejected.",
		},
	}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	result, err := a.Analyze(ctx, AnalyzeOptions{
		ProjectID:     "p",
		TeamID:        "demo",
		PolicyVersion: "memory-policy-v1",
		TeamDir:       filepath.Join(search, "demo"),
	})
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(search, "demo", "coordinator.md")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	svc := Service{Repo: repo}
	rejected, err := svc.Reject(ctx, result.Proposals[0].ID, "p", "demo", "not aligned with guidelines")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != StatusRejected || rejected.RejectionReason != "not aligned with guidelines" {
		t.Fatalf("rejected proposal = %#v", rejected)
	}

	// Verify target file is completely unmodified
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("reject modified target file")
	}

	// Verify source context item remains confirmed
	itemGot, err := repo.Get(ctx, "reject-item")
	if err != nil {
		t.Fatal(err)
	}
	if itemGot.Lifecycle != contextstore.LifecycleConfirmed {
		t.Fatalf("source context item modified on reject: %#v", itemGot)
	}

	// Verify rejection event is in outbox
	events, err := repo.PendingPromotionEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundRejectEvent := false
	for _, ev := range events {
		if ev.EventType == "memory_promotion_rejected" {
			foundRejectEvent = true
		}
	}
	if !foundRejectEvent {
		t.Fatal("missing memory_promotion_rejected event in outbox")
	}
}

func TestApplyFailsOnManualTargetDrift(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	writeTeam(t, search)

	item := contextstore.ContextItem{
		ID:        "drift-item",
		Kind:      contextstore.ContextDecision,
		Content:   "Drift detection verification",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "demo"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	g := &fakeGenerator{
		result: DraftResult{
			Type:  TypeTeamPolicy,
			Draft: "## Policy\nTarget drift test.",
		},
	}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	res, err := a.Analyze(ctx, AnalyzeOptions{
		ProjectID:     "p",
		TeamID:        "demo",
		PolicyVersion: "memory-policy-v1",
		TeamDir:       filepath.Join(search, "demo"),
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, res.Proposals[0].ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}

	// Manually edit the target file before apply
	target := filepath.Join(search, "demo", "coordinator.md")
	manualEdit := "---\nname: coordinator\nrole: coordinator\n---\nCoordinate.\n# Manual edit by developer\n"
	if err := os.WriteFile(target, []byte(manualEdit), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := team.NewTeamRegistry([]string{search})
	_, err = svc.Apply(ctx, p.ID, "p", "demo", registry)
	if err == nil || (!strings.Contains(err.Error(), "target base hash mismatch") && !strings.Contains(err.Error(), "promotion target changed")) {
		t.Fatalf("expected target base hash mismatch or changed error, got %v", err)
	}

	// Check proposal status is updated to stale
	stored, err := svc.Get(ctx, p.ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusStale {
		t.Fatalf("status = %s, want stale", stored.Status)
	}

	// Check target file is not overwritten
	current, _ := os.ReadFile(target)
	if string(current) != manualEdit {
		t.Fatalf("target was corrupted: %s", current)
	}
}

func TestPathTraversalAndSymlinkEscapeRefused(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	writeTeam(t, search)

	svc := Service{Repo: repo}

	// 1. Direct path traversal is refused at validation
	pTraversal := contextstore.PromotionProposal{
		ProjectID:      "p",
		TeamID:         "demo",
		Type:           contextstore.PromotionTypeTeamPolicy,
		TargetPath:     "../outside.md",
		TargetBaseHash: "",
		Draft:          "## Bad\nPath traversal.",
		DraftHash:      contextstore.HashPromotionContent("## Bad\nPath traversal."),
		PolicyVersion:  "memory-policy-v1",
		Sources:        []contextstore.PromotionSourceSnapshot{{ContextItemID: "src", ContentHash: "h", AggregateRevision: 1}},
		Status:         contextstore.PromotionStatusProposed,
	}
	pTraversal.ID = contextstore.PromotionProposalID(pTraversal)
	payload, _ := json.Marshal(map[string]any{"schema_version": 1, "proposal_id": pTraversal.ID})
	_, _, err = repo.CreatePromotion(ctx, pTraversal, contextstore.PromotionOutboxEvent{
		IdempotencyKey: pTraversal.ID + ":proposed",
		EventType:      "memory_promotion_proposed",
		Payload:        payload,
	})
	if err == nil || !strings.Contains(err.Error(), "clean relative path") {
		t.Fatalf("expected clean relative path error, got %v", err)
	}

	// 2. Symlink escaping team directory is refused at apply time
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.md")
	if err := os.WriteFile(outsideFile, []byte("---\nname: outside\nrole: worker\n---\nOutside."), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(search, "demo", "escaped.md")
	if err := os.Symlink(outsideFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	pSymlink := contextstore.PromotionProposal{
		ProjectID:      "p",
		TeamID:         "demo",
		Type:           contextstore.PromotionTypeAgentPolicy,
		AgentID:        "outside",
		TargetPath:     "escaped.md",
		TargetBaseHash: "",
		Draft:          "## Bad\nSymlink escape.",
		DraftHash:      contextstore.HashPromotionContent("## Bad\nSymlink escape."),
		PolicyVersion:  "memory-policy-v1",
		Sources:        []contextstore.PromotionSourceSnapshot{{ContextItemID: "src", ContentHash: "h", AggregateRevision: 1}},
		Status:         contextstore.PromotionStatusProposed,
	}
	pSymlink.ID = contextstore.PromotionProposalID(pSymlink)
	payload2, _ := json.Marshal(map[string]any{"schema_version": 1, "proposal_id": pSymlink.ID})
	_, _, err = repo.CreatePromotion(ctx, pSymlink, contextstore.PromotionOutboxEvent{
		IdempotencyKey: pSymlink.ID + ":proposed",
		EventType:      "memory_promotion_proposed",
		Payload:        payload2,
	})
	if err != nil {
		t.Fatal(err)
	}

	approved, err := svc.Approve(ctx, pSymlink.ID, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}

	registry := team.NewTeamRegistry([]string{search})
	_, err = svc.Apply(ctx, approved.ID, "p", "demo", registry)
	if err == nil || (!strings.Contains(err.Error(), "escapes team directory") && !strings.Contains(err.Error(), "outside team directory")) {
		t.Fatalf("expected escapes or outside team directory error on symlink apply, got: %v", err)
	}
}

func TestExistingSkillRefusal(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	writeTeam(t, search)

	// Create existing skill
	existingSkillDir := filepath.Join(search, "demo", "skills", "existing-skill")
	if err := os.MkdirAll(existingSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingSkillFile := filepath.Join(existingSkillDir, "SKILL.md")
	if err := os.WriteFile(existingSkillFile, []byte("---\nname: existing-skill\ndescription: Already here\n---\nOld content"), 0o644); err != nil {
		t.Fatal(err)
	}

	item := contextstore.ContextItem{
		ID:        "skill-item",
		Kind:      contextstore.ContextPattern,
		Content:   "Create a skill that conflicts with existing one",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "demo"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	draft := "---\nname: existing-skill\ndescription: Attempt to overwrite\n---\n## Steps\n1. Step 1\n2. Step 2"
	g := &fakeGenerator{
		result: DraftResult{
			Type:      TypeSkill,
			SkillName: "existing-skill",
			Draft:     draft,
			Steps:     []string{"Step 1", "Step 2"},
		},
	}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	_, err = a.Analyze(ctx, AnalyzeOptions{
		ProjectID:     "p",
		TeamID:        "demo",
		PolicyVersion: "memory-policy-v1",
		TeamDir:       filepath.Join(search, "demo"),
	})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got: %v", err)
	}

	// Verify existing skill content is unchanged
	after, _ := os.ReadFile(existingSkillFile)
	if !strings.Contains(string(after), "Old content") {
		t.Fatalf("existing skill was overwritten: %s", after)
	}
}

func TestPolicyApplyPreservesFullFrontmatter(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	teamDir := filepath.Join(search, "rich-team")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}

	richAgent := `---
name: specialized-worker
role: worker
tools: view,ls,write,edit
model: ollama/qwen3:8b
guard: [rule 1, rule 2]
memory:
  inject: true
---
Original prompt text with custom line breaks.
`
	target := filepath.Join(teamDir, "specialized-worker.md")
	if err := os.WriteFile(target, []byte(richAgent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte("---\nname: coordinator\nrole: coordinator\n---\nCoord."), 0o644); err != nil {
		t.Fatal(err)
	}

	item := contextstore.ContextItem{
		ID:        "rich-item",
		Kind:      contextstore.ContextInstruction,
		Content:   "Specialized instruction for worker",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "rich-team", AgentID: "specialized-worker"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	g := &fakeGenerator{
		result: DraftResult{
			Type:    TypeAgentPolicy,
			AgentID: "specialized-worker",
			Draft:   "## Promoted policy\nNew instruction.",
		},
	}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	res, err := a.Analyze(ctx, AnalyzeOptions{
		ProjectID:     "p",
		TeamID:        "rich-team",
		PolicyVersion: "memory-policy-v1",
		TeamDir:       teamDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc := Service{Repo: repo}
	p, err := svc.Approve(ctx, res.Proposals[0].ID, "p", "rich-team")
	if err != nil {
		t.Fatal(err)
	}

	registry := team.NewTeamRegistry([]string{search})
	applied, err := svc.Apply(ctx, p.ID, "p", "rich-team", registry)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Proposal.Status != StatusApplied {
		t.Fatal(applied.Proposal.Status)
	}

	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	// Split by frontmatter delimiters
	parts := strings.SplitN(string(content), "---", 3)
	if len(parts) < 3 {
		t.Fatalf("missing frontmatter delimiter: %s", content)
	}
	expectedFrontmatter := `
name: specialized-worker
role: worker
tools: view,ls,write,edit
model: ollama/qwen3:8b
guard: [rule 1, rule 2]
memory:
  inject: true
`
	if strings.TrimSpace(parts[1]) != strings.TrimSpace(expectedFrontmatter) {
		t.Fatalf("frontmatter modified! got:\n%s\nwant:\n%s", parts[1], expectedFrontmatter)
	}
	if !strings.Contains(parts[2], "Original prompt text with custom line breaks.") {
		t.Fatal("original prompt body missing")
	}
	if !strings.Contains(parts[2], "<!-- hufu-promotion:"+p.ID+":start -->") {
		t.Fatal("promotion block missing from body")
	}
}

func TestInvalidOrSecretDraftNotPersisted(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	search := t.TempDir()
	writeTeam(t, search)

	item := contextstore.ContextItem{
		ID:        "item-1",
		Kind:      contextstore.ContextDecision,
		Content:   "General decision for failure testing",
		Scope:     contextstore.Scope{ProjectID: "p", TeamID: "demo"},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	appendEligible(t, repo, item)

	testCases := []struct {
		name   string
		result DraftResult
	}{
		{
			name: "secret in draft",
			result: DraftResult{
				Type:  TypeTeamPolicy,
				Draft: "## Leaked Secret\nUse api_token=ghp_1234567890abcdef1234567890abcdef for access.",
			},
		},
		{
			name: "policy containing frontmatter",
			result: DraftResult{
				Type:  TypeTeamPolicy,
				Draft: "---\nname: fake-agent\nrole: worker\n---\nShould not contain frontmatter.",
			},
		},
		{
			name: "skill with fewer than 2 steps",
			result: DraftResult{
				Type:      TypeSkill,
				SkillName: "one-step",
				Draft:     "---\nname: one-step\ndescription: only one step\n---\n## Step\n1. Only step.",
				Steps:     []string{"Only step"},
			},
		},
		{
			name: "empty draft",
			result: DraftResult{
				Type:  TypeTeamPolicy,
				Draft: "   \n\t",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			g := &fakeGenerator{result: tc.result}
			a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
			res, err := a.Analyze(ctx, AnalyzeOptions{
				ProjectID:     "p",
				TeamID:        "demo",
				PolicyVersion: "memory-policy-v1",
				TeamDir:       filepath.Join(search, "demo"),
			})
			if err == nil && len(res.Proposals) != 0 {
				t.Fatalf("expected error or 0 proposals for %s, got %d proposals", tc.name, len(res.Proposals))
			}

			// Verify nothing persisted in SQLite
			proposals, err := repo.ListPromotions(ctx, "p", "demo")
			if err != nil {
				t.Fatal(err)
			}
			if len(proposals) != 0 {
				t.Fatalf("expected 0 persisted proposals, got %d", len(proposals))
			}
		})
	}
}
