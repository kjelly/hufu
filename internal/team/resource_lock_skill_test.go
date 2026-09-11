package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func TestResolvedSkillContentUsesLockedSnapshotWhenDeclared(t *testing.T) {
	c := &Coordinator{}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "verification-policy", Kind: agent.ResourceSkill}, Content: "locked snapshot body"},
	})

	def := &skill.SkillDef{Name: "verification-policy", Content: "stale cached copy"}
	if got := c.resolvedSkillContent(def); got != "locked snapshot body" {
		t.Fatalf("resolvedSkillContent() = %q, want the locked snapshot", got)
	}
}

func TestResolvedSkillContentFallsBackWithoutDeclaration(t *testing.T) {
	c := &Coordinator{}
	def := &skill.SkillDef{Name: "undeclared-skill", Content: "ordinary cached copy"}
	if got := c.resolvedSkillContent(def); got != "ordinary cached copy" {
		t.Fatalf("resolvedSkillContent() = %q, want the ordinary cached content", got)
	}
}

func TestResolvedSkillContentNilDefIsSafe(t *testing.T) {
	c := &Coordinator{}
	if got := c.resolvedSkillContent(nil); got != "" {
		t.Fatalf("resolvedSkillContent(nil) = %q, want empty", got)
	}
}

func TestBuildSkillContextItemsUsesLockedSnapshotInFullFallback(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "x", Content: "stale cached copy"}}}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "x", Kind: agent.ResourceSkill}, Content: "verified locked copy"},
	})
	agentDef := &agent.AgentDef{Name: "worker", Skills: "x"}

	items, err := c.buildSkillContextItems(agentDef, "worker", "goal", "", map[string]bool{"load_skill": false})
	if err != nil {
		t.Fatalf("buildSkillContextItems() error = %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want exactly 1", items)
	}
	if items[0].Content != "verified locked copy" {
		t.Fatalf("items[0].Content = %q, want the locked copy", items[0].Content)
	}
}

func TestBuildSkillContextItemsUsesCachedContentWithoutLock(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "x", Content: "ordinary cached copy"}}}
	agentDef := &agent.AgentDef{Name: "worker", Skills: "x"}

	items, err := c.buildSkillContextItems(agentDef, "worker", "goal", "", map[string]bool{"load_skill": false})
	if err != nil {
		t.Fatalf("buildSkillContextItems() error = %v", err)
	}
	if len(items) != 1 || items[0].Content != "ordinary cached copy" {
		t.Fatalf("items = %+v, want the ordinary cached content unchanged", items)
	}
}

func TestLoadSkillToolReturnsLockedSnapshot(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "x", Content: "stale cached copy"}}}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "x", Kind: agent.ResourceSkill}, Content: "verified locked copy"},
	})
	tool := &loadSkillTool{coordinator: c}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"name":"x"}`})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(resp.Content, "verified locked copy") {
		t.Fatalf("response = %q, want it to contain the locked copy", resp.Content)
	}
	if strings.Contains(resp.Content, "stale cached copy") {
		t.Fatalf("response = %q, want it to NOT contain the stale cached copy", resp.Content)
	}
}

func TestLoadSkillToolReturnsCachedContentWithoutLock(t *testing.T) {
	c := &Coordinator{skills: []*skill.SkillDef{{Name: "x", Content: "ordinary cached copy"}}}
	tool := &loadSkillTool{coordinator: c}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"name":"x"}`})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(resp.Content, "ordinary cached copy") {
		t.Fatalf("response = %q, want the ordinary cached content unchanged", resp.Content)
	}
}
