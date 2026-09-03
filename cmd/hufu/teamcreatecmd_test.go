package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetTeamCreateFlags() {
	teamCreatePreset = ""
	teamCreateFrom = ""
	teamCreateModel = ""
	teamCreateForce = false
	teamCreateExpanded = false
}

func TestRunTeamCreate_DefaultScaffoldsSingleWorker(t *testing.T) {
	resetTeamCreateFlags()
	dir := chdirTemp(t)

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	teamDir := filepath.Join(dir, ".agent-teams", "dev")
	if _, err := os.Stat(filepath.Join(teamDir, "worker.md")); err != nil {
		t.Fatalf("worker.md not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(teamDir, "team.yaml")); !os.IsNotExist(err) {
		t.Fatalf("team.yaml should not be created without --model, stat err = %v", err)
	}
}

func TestRunTeamCreate_TeamPreset(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	dir := chdirTemp(t)
	teamCreatePreset = "coding-reviewed"

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	teamDir := filepath.Join(dir, ".agent-teams", "dev")
	for _, want := range []string{"developer.md", "reviewer.md"} {
		if _, err := os.Stat(filepath.Join(teamDir, want)); err != nil {
			t.Errorf("%s not created: %v", want, err)
		}
	}
}

func TestRunTeamCreate_ModelWritesMinimalTeamYAML(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	dir := chdirTemp(t)
	teamCreatePreset = "coding-reviewed"
	teamCreateModel = "local-model"

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".agent-teams", "dev", "team.yaml"))
	if err != nil {
		t.Fatalf("read team.yaml: %v", err)
	}
	if string(got) != "model: local-model\n" {
		t.Fatalf("team.yaml = %q, want minimal model-only content", got)
	}
}

func TestRunTeamCreate_ExpandedPinsDefaults(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	dir := chdirTemp(t)
	teamCreateExpanded = true

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".agent-teams", "dev", "team.yaml"))
	if err != nil {
		t.Fatalf("read team.yaml: %v", err)
	}
	if !strings.Contains(string(got), "max-rounds: 10") {
		t.Fatalf("team.yaml = %q, want pinned defaults", got)
	}
}

func TestRunTeamCreate_FromGeneratesTaskSpecificTeam(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	dir := chdirTemp(t)
	teamCreateFrom = "Fix the OAuth callback bug and add regression tests"

	if err := runTeamCreate(nil, []string{"oauth-fix"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	teamDir := filepath.Join(dir, ".agent-teams", "oauth-fix")
	if _, err := os.Stat(filepath.Join(teamDir, "coordinator.md")); err != nil {
		t.Fatalf("coordinator.md not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(teamDir, "fixer.md")); err != nil {
		t.Fatalf("fixer.md not created (expected bugfix classification): %v", err)
	}
}

func TestRunTeamCreate_PresetAndFromConflict(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	chdirTemp(t)
	teamCreatePreset = "coding-reviewed"
	teamCreateFrom = "Fix a bug"

	if err := runTeamCreate(nil, []string{"dev"}); err == nil {
		t.Fatal("runTeamCreate() error = nil, want a conflict error for --preset with --from")
	}
}

func TestRunTeamCreate_RefusesOverwriteWithoutForce(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	chdirTemp(t)

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("first runTeamCreate: %v", err)
	}
	if err := runTeamCreate(nil, []string{"dev"}); err == nil {
		t.Fatal("second runTeamCreate() error = nil, want a refusal without --force")
	}
	teamCreateForce = true
	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate with --force: %v", err)
	}
}

func TestRunTeamCreate_UnknownPresetFails(t *testing.T) {
	resetTeamCreateFlags()
	t.Cleanup(resetTeamCreateFlags)
	chdirTemp(t)
	teamCreatePreset = "does-not-exist"

	if err := runTeamCreate(nil, []string{"dev"}); err == nil {
		t.Fatal("runTeamCreate() error = nil, want an error for an unknown preset")
	}
}
