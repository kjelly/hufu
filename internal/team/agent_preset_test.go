package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 3 (spec.md Specification 05 / Specification 01 §6-7): agent-level
// `preset:` frontmatter expands into tools and a default side-effect class.

func writePresetAgentFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPreset_CodingExpandsToolsAndSideEffect(t *testing.T) {
	dir := t.TempDir()
	writePresetAgentFile(t, dir, "developer.md", "---\npreset: coding\n---\nImplement the change.\n")

	def, err := parseAgentFile(filepath.Join(dir, "developer.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	for _, tool := range []string{"view", "write", "edit", "multiedit", "grep", "glob", "ls", "bash"} {
		if !agentDeclaresTool(def, tool) {
			t.Errorf("preset coding: tools = %q, missing %q", def.Tools, tool)
		}
	}
	if def.SideEffect != "workspace_write" {
		t.Errorf("SideEffect = %q, want %q", def.SideEffect, "workspace_write")
	}
}

func TestAgentPreset_ExplicitDenyOverridesPresetGrant(t *testing.T) {
	dir := t.TempDir()
	writePresetAgentFile(t, dir, "developer.md", "---\npreset: coding\ntools:\n  denied: [bash]\n---\nImplement the change.\n")

	def, err := parseAgentFile(filepath.Join(dir, "developer.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if agentDeclaresTool(def, "bash") {
		t.Errorf("tools = %q, want bash denied despite coding preset grant", def.Tools)
	}
	for _, tool := range []string{"view", "write", "edit", "multiedit", "grep", "glob", "ls"} {
		if !agentDeclaresTool(def, tool) {
			t.Errorf("tools = %q, missing %q (only bash should be removed)", def.Tools, tool)
		}
	}
}

func TestAgentPreset_ExplicitAllowMergesWithPreset(t *testing.T) {
	dir := t.TempDir()
	writePresetAgentFile(t, dir, "researcher.md", "---\npreset: research\ntools:\n  allowed: [random]\n---\nResearch the question.\n")

	def, err := parseAgentFile(filepath.Join(dir, "researcher.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	for _, tool := range []string{"view", "grep", "glob", "ls", "fetch", "agentic_fetch", "random"} {
		if !agentDeclaresTool(def, tool) {
			t.Errorf("tools = %q, missing %q", def.Tools, tool)
		}
	}
}

func TestAgentPreset_ExplicitSideEffectOverridesPreset(t *testing.T) {
	dir := t.TempDir()
	writePresetAgentFile(t, dir, "reviewer.md", "---\npreset: review\nside_effect: workspace_write\n---\nReview the change.\n")

	def, err := parseAgentFile(filepath.Join(dir, "reviewer.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.SideEffect != "workspace_write" {
		t.Errorf("SideEffect = %q, want explicit override %q (not preset default %q)", def.SideEffect, "workspace_write", "none")
	}
}

func TestAgentPreset_UnknownPresetFails(t *testing.T) {
	dir := t.TempDir()
	writePresetAgentFile(t, dir, "developer.md", "---\npreset: nonexistent-preset\n---\nImplement the change.\n")

	_, err := parseAgentFile(filepath.Join(dir, "developer.md"), nil)
	if err == nil {
		t.Fatal("expected error for unknown preset, got nil")
	}
	if !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("expected an unknown-preset error, got: %v", err)
	}
}

func TestAgentPreset_OpsPresetNeverGrantsSudo(t *testing.T) {
	dir := t.TempDir()
	writePresetAgentFile(t, dir, "operator.md", "---\npreset: ops\n---\nPerform scoped operational tasks.\n")

	def, err := parseAgentFile(filepath.Join(dir, "operator.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if agentDeclaresTool(def, "sudo") {
		t.Errorf("tools = %q, ops preset must never grant sudo by default", def.Tools)
	}
}
