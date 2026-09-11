package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestLockedResourceContextItemsFiltersByInjectInto(t *testing.T) {
	loaded := []*LoadedResource{
		{LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"coordinator", "coder"}}, Content: "rules"},
		{LockedResource: LockedResource{Name: "reviewer-only", Kind: agent.ResourceProjectRules, InjectInto: []string{"reviewer"}}, Content: "reviewer stuff"},
	}

	items := LockedResourceContextItems(loaded, "coordinator")
	if len(items) != 1 || items[0].ID != "locked_resource:team-rules" {
		t.Fatalf("items = %+v, want only team-rules for coordinator", items)
	}

	items = LockedResourceContextItems(loaded, "reviewer")
	if len(items) != 1 || items[0].ID != "locked_resource:reviewer-only" {
		t.Fatalf("items = %+v, want only reviewer-only for reviewer", items)
	}

	if items := LockedResourceContextItems(loaded, "nobody"); len(items) != 0 {
		t.Fatalf("items = %+v, want none for an agent named in no InjectInto list", items)
	}
}

func TestLockedResourceContextItemsSkipsResourcesWithNoInjectTargets(t *testing.T) {
	loaded := []*LoadedResource{
		{LockedResource: LockedResource{Name: "locked-but-not-injected", Kind: agent.ResourceProjectRules}, Content: "content"},
	}
	if items := LockedResourceContextItems(loaded, "coordinator"); len(items) != 0 {
		t.Fatalf("items = %+v, want none for a resource with an empty InjectInto list", items)
	}
}

func TestLockedResourceContextItemsAreRequiredAndNormative(t *testing.T) {
	loaded := []*LoadedResource{
		{LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"coordinator"}}, Content: "rules content"},
	}
	items := LockedResourceContextItems(loaded, "coordinator")
	if len(items) != 1 {
		t.Fatalf("items = %+v, want 1", items)
	}
	item := items[0]
	if !item.Required {
		t.Error("Required = false, want true")
	}
	if item.Authority != ContextAuthorityNormative {
		t.Errorf("Authority = %q, want %q", item.Authority, ContextAuthorityNormative)
	}
	if item.Content != "rules content" {
		t.Errorf("Content = %q, want %q", item.Content, "rules content")
	}
	if item.DedupKey == "" {
		t.Error("DedupKey is empty")
	}
}

// TestLockedResourceContentNeverReReadAtInjectTime proves
// LockedResourceContextItems never touches disk: CanonicalPath points at a
// file that does not exist, yet the item still carries the correct content
// because it comes solely from LoadedResource.Content (runtime invariant
// 11 — re-reading at inject time would reopen the TOCTOU window resource
// locking exists to close).
func TestLockedResourceContentNeverReReadAtInjectTime(t *testing.T) {
	loaded := []*LoadedResource{
		{
			LockedResource: LockedResource{
				Name: "team-rules", Kind: agent.ResourceProjectRules,
				CanonicalPath: "/does/not/exist/AGENTS.md",
				InjectInto:    []string{"coordinator"},
			},
			Content: "content captured at lock time",
		},
	}
	items := LockedResourceContextItems(loaded, "coordinator")
	if len(items) != 1 || items[0].Content != "content captured at lock time" {
		t.Fatalf("items = %+v, want the in-memory content despite a nonexistent CanonicalPath", items)
	}
}

// TestRequiredResourceCannotBeDroppedByTokenBudget runs a
// LockedResourceContextItems output through the real BudgetContextItems
// pipeline (context_compiler.go) — the same function every other required
// context item goes through — and confirms it fails closed instead of
// silently dropping or truncating the locked resource (runtime invariant
// 8). LockedResourceContextItems deliberately never sets Compressible, so
// there is no truncation path available for it either.
func TestRequiredResourceCannotBeDroppedByTokenBudget(t *testing.T) {
	loaded := []*LoadedResource{
		{
			LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"coordinator"}},
			Content:        strings.Repeat("word ", 1000), // far larger than the tiny budget below
		},
	}
	items := LockedResourceContextItems(loaded, "coordinator")

	_, _, err := BudgetContextItems(items, ContextBudget{Available: 5})
	if err == nil {
		t.Fatal("BudgetContextItems() = nil error, want a fail-closed error for a required item that cannot fit")
	}
	if !strings.Contains(err.Error(), "team-rules") {
		t.Fatalf("error = %v, want it to name the required item", err)
	}
}

func TestRequiredSkillUsesLockedSnapshot(t *testing.T) {
	loaded := []*LoadedResource{
		{LockedResource: LockedResource{Name: "verification-policy", Kind: agent.ResourceSkill}, Content: "skill body"},
		{LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules}, Content: "not a skill"},
	}

	content, ok := LockedSkillContent(loaded, "verification-policy")
	if !ok || content != "skill body" {
		t.Fatalf("LockedSkillContent() = (%q, %v), want (%q, true)", content, ok, "skill body")
	}

	if _, ok := LockedSkillContent(loaded, "team-rules"); ok {
		t.Fatal("LockedSkillContent() = ok for a project_rules resource, want false (wrong kind)")
	}
	if _, ok := LockedSkillContent(loaded, "does-not-exist"); ok {
		t.Fatal("LockedSkillContent() = ok for an undeclared name, want false")
	}
}

func TestInjectLockedRequiredResourcesAppendsToExistingItems(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir+"/AGENTS.md", "rules content")
	specs := []agent.RequiredResourceSpec{{Name: "team-rules", Kind: agent.ResourceProjectRules, Path: "AGENTS.md", Required: true, InjectInto: []string{"coordinator"}}}
	c := newResourceLockTestCoordinator(t, dir, specs)
	if err := c.ValidateRequiredResourceLocks(t.Context(), dir); err != nil {
		t.Fatalf("ValidateRequiredResourceLocks() error = %v", err)
	}

	existing := []ContextItem{{ID: "current_task", Kind: "current_task", Content: "goal"}}
	items := c.InjectLockedRequiredResources(existing, "coordinator")

	if len(items) != 2 {
		t.Fatalf("items = %+v, want the original item plus the locked resource", items)
	}
	if items[0].ID != "current_task" {
		t.Fatalf("items[0] = %+v, want the pre-existing item preserved first", items[0])
	}
}
