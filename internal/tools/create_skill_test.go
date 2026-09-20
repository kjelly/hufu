package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestCreateSkillToolRejectsPathTraversal(t *testing.T) {
	workDir := t.TempDir()
	tool := NewCreateSkillTool(WithWorkDir(workDir))
	ctx := SetToolsAllowed(t.Context(), []string{"create_skill"})

	tests := []string{
		"../../../../etc/evil",
		"../outside",
		"a/b",
		"a\\b",
		"..",
		".",
	}

	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			input := fmt.Sprintf(`{"name": %q, "description": "d", "content": "c"}`, name)
			result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected error for name %q, got success: %s", name, result.Content)
			}
			if !strings.Contains(result.Content, "invalid name") {
				t.Fatalf("expected 'invalid name' error for %q, got: %s", name, result.Content)
			}
		})
	}

	// Confirm nothing escaped the skills directory.
	if _, err := os.Stat(filepath.Join(workDir, "etc", "evil")); !os.IsNotExist(err) {
		t.Fatalf("path traversal wrote outside workDir: %v", err)
	}
}

func TestCreateSkillToolAcceptsValidName(t *testing.T) {
	workDir := t.TempDir()
	tool := NewCreateSkillTool(WithWorkDir(workDir))
	ctx := SetToolsAllowed(t.Context(), []string{"create_skill"})

	input := `{"name": "data-analyzer", "description": "d", "content": "# hello"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %s", result.Content)
	}

	skillPath := filepath.Join(workDir, "skills", "data-analyzer", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("expected skill file at %s: %v", skillPath, err)
	}
	if string(data) != "# hello" {
		t.Errorf("skill content = %q, want %q", string(data), "# hello")
	}
}

func TestCreateSkillToolRejectsRuntimeControlName(t *testing.T) {
	workDir := t.TempDir()
	tool := NewCreateSkillTool(WithWorkDir(workDir))
	ctx := SetToolsAllowed(t.Context(), []string{"create_skill"})

	for _, name := range []string{"submit_result", "submit-result", "save_skill"} {
		t.Run(name, func(t *testing.T) {
			input := fmt.Sprintf(`{"name": %q, "description": "d", "content": "# control"}`, name)
			result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if !result.IsError || !strings.Contains(result.Content, "reserved for runtime control") {
				t.Fatalf("reserved name %q result = %#v", name, result)
			}
			if _, err := os.Stat(filepath.Join(workDir, "skills", name)); !os.IsNotExist(err) {
				t.Fatalf("reserved skill directory exists: %v", err)
			}
		})
	}
}
