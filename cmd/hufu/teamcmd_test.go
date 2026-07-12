package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildGeneratedTeam_SelectsBugfixRoles(t *testing.T) {
	g := buildGeneratedTeam("oauth-bugfix", "Fix the OAuth callback error and add regression tests", "", "workspace")
	if g.Category != "bugfix" {
		t.Fatalf("category = %q, want bugfix", g.Category)
	}
	for _, name := range []string{"team.yaml", "coordinator.md", "reproducer.md", "fixer.md", "reviewer.md"} {
		if _, ok := g.Files[name]; !ok {
			t.Errorf("generated files missing %s", name)
		}
	}
}

func TestBuildGeneratedTeam_SelectsResearchRoles(t *testing.T) {
	g := buildGeneratedTeam("api-research", "研究並比較 API authentication options，寫成文件", "", "workspace")
	if g.Category != "research" {
		t.Fatalf("category = %q, want research", g.Category)
	}
	for _, name := range []string{"researcher.md", "writer.md", "fact-checker.md"} {
		if _, ok := g.Files[name]; !ok {
			t.Errorf("generated files missing %s", name)
		}
	}
}

func TestValidateGeneratedTeam(t *testing.T) {
	g := buildGeneratedTeam("feature-work", "Add a new CLI command", "ollama/qwen3:8b", "workspace")
	if err := validateGeneratedTeam(g); err != nil {
		t.Fatalf("validateGeneratedTeam() error = %v", err)
	}
	delete(g.Files, "coordinator.md")
	if err := validateGeneratedTeam(g); err == nil {
		t.Fatal("validateGeneratedTeam() accepted a team without a coordinator")
	}
}

func TestWriteGeneratedTeamRefusesOverwrite(t *testing.T) {
	g := buildGeneratedTeam("feature-work", "Add a new CLI command", "", "workspace")
	target := filepath.Join(t.TempDir(), g.Name)
	if err := writeGeneratedTeam(target, g); err != nil {
		t.Fatalf("first writeGeneratedTeam() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "coordinator.md")); err != nil {
		t.Fatalf("coordinator.md was not written: %v", err)
	}
	if err := writeGeneratedTeam(target, g); err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("second writeGeneratedTeam() error = %v, want overwrite refusal", err)
	}
}

func TestNormalizeGeneratedTeamName(t *testing.T) {
	if got, err := normalizeGeneratedTeamName("  Go-Fix-2 "); err != nil || got != "go-fix-2" {
		t.Fatalf("normalizeGeneratedTeamName() = %q, %v", got, err)
	}
	if _, err := normalizeGeneratedTeamName("go_fix"); err == nil {
		t.Fatal("expected invalid name error")
	}
}
