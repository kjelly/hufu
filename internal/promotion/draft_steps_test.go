package promotion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestDraftStepsIgnoresFrontmatter(t *testing.T) {
	cases := []struct {
		name  string
		typ   Type
		draft string
		want  int
	}{
		{name: "dash bullets", typ: TypeSkill, draft: "---\nname: s\ndescription: d\n---\n- Build.\n- Test.", want: 2},
		{name: "star and plus bullets", typ: TypeSkill, draft: "---\nname: s\ndescription: d\n---\n* Build.\n+ Test.", want: 2},
		{name: "numbered beyond two", typ: TypeSkill, draft: "---\nname: s\ndescription: d\n---\n1. Build.\n2. Test.\n3) Ship.", want: 3},
		{name: "headings only", typ: TypeSkill, draft: "---\nname: s\ndescription: d\n---\n## Build\n## Test", want: 0},
		{name: "frontmatter list is not a step", typ: TypeSkill, draft: "---\nname: s\ndescription: d\ntags:\n- one\n- two\n---\nRun the workflow.", want: 0},
		{name: "empty bullet is not a step", typ: TypeSkill, draft: "---\nname: s\ndescription: d\n---\n- \n1. Build.", want: 1},
		{name: "unparsable draft", typ: TypeSkill, draft: "- Build.\n- Test.", want: 0},
		{name: "policy has no steps", typ: TypeTeamPolicy, draft: "- Build.\n- Test.", want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DraftSteps(tc.typ, tc.draft); len(got) != tc.want {
				t.Fatalf("DraftSteps = %q, want %d steps", got, tc.want)
			}
		})
	}
}

func TestValidateDraftRequiresBodySteps(t *testing.T) {
	cases := []struct {
		name    string
		draft   string
		wantErr string
	}{
		{name: "two body steps", draft: "---\nname: s\ndescription: d\n---\n* Build.\n3) Test."},
		{name: "heading steps only", draft: "---\nname: s\ndescription: d\n---\n## Build\n## Test", wantErr: "two verifiable steps"},
		{name: "missing description", draft: "---\nname: s\n---\n1. Build.\n2. Test.", wantErr: "description"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDraft(TypeSkill, tc.draft, "s")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateDraft: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateDraft error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func newSkillPromotionFixture(t *testing.T) (*contextstore.SQLiteRepository, string) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	search := t.TempDir()
	writeTeam(t, search)
	item := contextstore.ContextItem{ID: "pattern", Kind: contextstore.ContextPattern, Content: "Generate code then run two verification commands", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	appendEligible(t, repo, item)
	return repo, search
}

func TestAnalyzeRejectsDraftThatApplyWouldReject(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	// The model claims two steps, but the body has none; analyze used to
	// accept this and apply then failed with apply_failed.
	draft := "---\nname: generated-check\ndescription: Run the verified workflow.\n---\n## Generate\n## Verify"
	g := &fakeGenerator{result: DraftResult{Type: TypeSkill, SkillName: "generated-check", Draft: draft, Steps: []string{"Generate", "Verify"}}}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	_, err := a.Analyze(ctx, AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")})
	if err == nil || !strings.Contains(err.Error(), "two verifiable steps") {
		t.Fatalf("Analyze error = %v, want step validation failure", err)
	}
	proposals, err := repo.ListPromotions(ctx, "p", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 {
		t.Fatalf("persisted %d proposals, want 0", len(proposals))
	}
}

func TestEditTracksGeneratedDraftAndApplySharesStepRule(t *testing.T) {
	ctx := context.Background()
	repo, search := newSkillPromotionFixture(t)
	draft := "---\nname: generated-check\ndescription: Run the verified workflow.\n---\n1. Generate the code.\n2. Run the verifier."
	g := &fakeGenerator{result: DraftResult{Type: TypeSkill, SkillName: "generated-check", Draft: draft}}
	a := Analyzer{Repo: repo, Generator: g, Policy: agent.DefaultMemoryLearningPolicy()}
	opts := AnalyzeOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1", TeamDir: filepath.Join(search, "demo")}
	result, err := a.Analyze(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	created := result.Proposals[0]
	if edited, known := created.DraftEdited(); !known || edited {
		t.Fatalf("new proposal DraftEdited = %v/%v, want false/true", edited, known)
	}

	// An operator edit that uses star bullets and a "3)" step must pass edit
	// and apply with the same step rule analyze uses.
	editedDraft := "---\nname: generated-check\ndescription: Run the verified workflow.\n---\n* Generate the code.\n* Run the verifier.\n3) Record the result."
	file := filepath.Join(t.TempDir(), "draft.md")
	if err := os.WriteFile(file, []byte(editedDraft), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := Service{Repo: repo}
	p, err := svc.Edit(ctx, created.ID, "p", "demo", file)
	if err != nil {
		t.Fatal(err)
	}
	if edited, known := p.DraftEdited(); !known || !edited {
		t.Fatalf("edited proposal DraftEdited = %v/%v, want true/true", edited, known)
	}
	if p.GeneratedDraftHash != created.GeneratedDraftHash {
		t.Fatalf("edit changed generated draft hash %q -> %q", created.GeneratedDraftHash, p.GeneratedDraftHash)
	}

	// Re-analyzing returns the existing proposal and keeps the edit state.
	again, err := a.Analyze(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if again.Proposals[0].GeneratedDraftHash != created.GeneratedDraftHash || again.Proposals[0].DraftHash != p.DraftHash {
		t.Fatalf("re-analyze proposal = %+v", again.Proposals[0])
	}

	if _, err = svc.Approve(ctx, p.ID, "p", "demo"); err != nil {
		t.Fatal(err)
	}
	applied, err := svc.Apply(ctx, p.ID, "p", "demo", team.NewTeamRegistry([]string{search}))
	if err != nil {
		t.Fatal(err)
	}
	if applied.Proposal.Status != StatusApplied {
		t.Fatalf("status = %s", applied.Proposal.Status)
	}
}
