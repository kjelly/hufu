package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/skill"
)

func newMinimalCoordinator(t *testing.T) (*Coordinator, string) {
	t.Helper()
	dir := t.TempDir()
	session := &TeamSession{
		Dir:       dir,
		Workspace: filepath.Join(dir, "workspace"),
		Config:    agent.TeamConfig{},
		Agents:    map[string]*agent.AgentDef{},
	}
	return &Coordinator{
		session:        session,
		skillUsage:     make(map[string]*skillUsageState),
		delegatedTasks: make(map[string]int),
	}, dir
}

func runSaveSkill(t *testing.T, c *Coordinator, input string) fantasy.ToolResponse {
	t.Helper()
	tool := &saveSkillTool{coordinator: c}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("tool.Run error: %v", err)
	}
	return resp
}

func TestSaveAndReloadSkill_CreatesFile(t *testing.T) {
	c, dir := newMinimalCoordinator(t)

	path, err := c.saveAndReloadSkill("my-skill", "Does something useful.", "# My Skill\n\nStep 1: do the thing.\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := filepath.Join(dir, "skills", "my-skill", "SKILL.md")
	if path != wantPath {
		t.Errorf("returned path = %q, want %q", path, wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("skill file not written: %v", err)
	}
	content := string(data)
	for _, want := range []string{"name: my-skill", "Does something useful.", "Step 1: do the thing."} {
		if !strings.Contains(content, want) {
			t.Errorf("SKILL.md missing %q:\n%s", want, content)
		}
	}
}

func findSkill(skills []*skill.SkillDef, name string) *skill.SkillDef {
	for _, s := range skills {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func TestSaveAndReloadSkill_HotReload(t *testing.T) {
	c, _ := newMinimalCoordinator(t)

	before := len(c.getSkills())

	if _, err := c.saveAndReloadSkill("hot-skill", "A hot-reloaded skill.", "# Hot Skill\n\nContent here.\n"); err != nil {
		t.Fatalf("saveAndReloadSkill: %v", err)
	}

	skills := c.getSkills()
	if len(skills) <= before {
		t.Fatalf("expected skills list to grow after save (before=%d, after=%d)", before, len(skills))
	}
	if findSkill(skills, "hot-skill") == nil {
		t.Errorf("hot-skill not found in skills list after save")
	}
}

func TestSaveAndReloadSkill_Overwrite(t *testing.T) {
	c, _ := newMinimalCoordinator(t)

	if _, err := c.saveAndReloadSkill("overwrite-skill", "v1 description.", "v1 content"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if _, err := c.saveAndReloadSkill("overwrite-skill", "v2 description.", "v2 content"); err != nil {
		t.Fatalf("second save: %v", err)
	}

	// Overwrite must not create duplicates.
	skills := c.getSkills()
	count := 0
	for _, s := range skills {
		if s.Name == "overwrite-skill" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 'overwrite-skill', got %d", count)
	}
	got := findSkill(skills, "overwrite-skill")
	if got.Description != "v2 description." {
		t.Errorf("description not updated to v2: %q", got.Description)
	}
}

func TestSaveAndReloadSkill_SlugifyName(t *testing.T) {
	c, dir := newMinimalCoordinator(t)

	if _, err := c.saveAndReloadSkill("My Cool Skill!", "desc", "body"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Directory slug should be lowercase-hyphenated.
	wantDir := filepath.Join(dir, "skills", "my-cool-skill")
	if _, err := os.Stat(wantDir); os.IsNotExist(err) {
		t.Errorf("expected slug directory %q to exist", wantDir)
	}
	// But the name field in the file preserves the original.
	data, _ := os.ReadFile(filepath.Join(wantDir, "SKILL.md"))
	if !strings.Contains(string(data), "name: My Cool Skill!") {
		t.Errorf("SKILL.md should preserve original name:\n%s", data)
	}
}

func TestSaveAndReloadSkill_Filtering(t *testing.T) {
	c, _ := newMinimalCoordinator(t)
	c.session.Config.Skills = "allowed-skill"

	if _, err := c.saveAndReloadSkill("allowed-skill", "Allowed.", "content"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.saveAndReloadSkill("blocked-skill", "Blocked.", "content"); err != nil {
		t.Fatal(err)
	}

	skills := c.getSkills()
	if len(skills) != 1 || skills[0].Name != "allowed-skill" {
		names := make([]string, len(skills))
		for i, s := range skills {
			names[i] = s.Name
		}
		t.Errorf("expected only [allowed-skill], got %v", names)
	}
}

func TestSaveSkillTool_InvalidArgs(t *testing.T) {
	c, _ := newMinimalCoordinator(t)
	cases := []struct {
		input string
		want  string
	}{
		{`{}`, "name is required"},
		{`{"name":"x"}`, "description is required"},
		{`{"name":"x","description":"d"}`, "content is required"},
	}
	for _, tc := range cases {
		resp := runSaveSkill(t, c, tc.input)
		if !resp.IsError {
			t.Errorf("input %q: expected error response", tc.input)
		}
		if !strings.Contains(resp.Content, tc.want) {
			t.Errorf("input %q: got %q, want it to contain %q", tc.input, resp.Content, tc.want)
		}
	}
}

func TestSaveSkillTool_Success(t *testing.T) {
	c, _ := newMinimalCoordinator(t)
	resp := runSaveSkill(t, c, `{"name":"test-skill","description":"test desc","content":"# Test\n\ncontent"}`)
	if resp.IsError {
		t.Errorf("unexpected error response: %s", resp.Content)
	}
	if !strings.Contains(resp.Content, "test-skill") {
		t.Errorf("response should mention skill name: %s", resp.Content)
	}
}
