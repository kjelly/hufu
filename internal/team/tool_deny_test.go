package team

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestTeamToolDenyRemovesAlwaysIncludedStateWriters(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			ToolsDenied: []string{"stm_write", "ltm_update", "memory_save"},
		}},
		coreTools: workerInvariantCoreTools(t),
	}
	def := &agent.AgentDef{Name: "reader", Tools: "view"}
	exposed := agentToolNames(c.selectWorkerTools(def))
	for _, denied := range c.session.Config.ToolsDenied {
		if slices.Contains(exposed, denied) {
			t.Fatalf("denied tool %q was exposed to worker: %v", denied, exposed)
		}
	}
	if !slices.Contains(exposed, "view") {
		t.Fatalf("declared read tool was removed: %v", exposed)
	}
	allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(t.Context(), def, exposed))
	for _, denied := range c.session.Config.ToolsDenied {
		if slices.Contains(allowed, denied) {
			t.Fatalf("denied tool %q was retained in runtime allowlist: %v", denied, allowed)
		}
	}
}

func TestParseTeamToolDeny(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: deny-test\ntools:\n  denied: [stm_write]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.ToolsDenied, []string{"stm_write"}) {
		t.Fatalf("ToolsDenied = %v, want [stm_write]", cfg.ToolsDenied)
	}
}

func TestDeniedToolInstructionsAreRemovedWithoutSyntheticToolNames(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			ToolsDenied: []string{"custom_memory_tool", "load_skill"},
		}},
	}
	prompt := c.filterDeniedPromptLines("keep this rule\ncall load_skill for details\nuse custom_memory_tool to save\n")
	if strings.Contains(prompt, "load_skill") || strings.Contains(prompt, "custom_memory_tool") {
		t.Fatalf("denied tool instructions remained in prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "keep this rule") {
		t.Fatalf("allowed prompt content was removed: %q", prompt)
	}
}
