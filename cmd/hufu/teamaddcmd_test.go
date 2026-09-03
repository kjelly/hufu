package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resetTeamAddFlags() {
	teamAddPreset = ""
	teamAddForce = false
}

func TestRunTeamAdd_AddsAgentWithPreset(t *testing.T) {
	resetTeamCreateFlags()
	resetTeamAddFlags()
	t.Cleanup(resetTeamAddFlags)
	dir := chdirTemp(t)

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	teamAddPreset = "review"
	if err := runTeamAdd(nil, []string{"dev", "reviewer"}); err != nil {
		t.Fatalf("runTeamAdd: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".agent-teams", "dev", "reviewer.md"))
	if err != nil {
		t.Fatalf("read reviewer.md: %v", err)
	}
	if !strings.Contains(string(content), "preset: review") {
		t.Errorf("reviewer.md = %q, want preset: review", content)
	}
}

func TestRunTeamAdd_RequiresExistingTeam(t *testing.T) {
	resetTeamAddFlags()
	t.Cleanup(resetTeamAddFlags)
	chdirTemp(t)

	if err := runTeamAdd(nil, []string{"does-not-exist", "reviewer"}); err == nil {
		t.Fatal("runTeamAdd() error = nil, want an error for a nonexistent team")
	}
}

func TestRunTeamAdd_RefusesOverwriteWithoutForce(t *testing.T) {
	resetTeamCreateFlags()
	resetTeamAddFlags()
	t.Cleanup(resetTeamAddFlags)
	chdirTemp(t)

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	if err := runTeamAdd(nil, []string{"dev", "worker"}); err == nil {
		t.Fatal("runTeamAdd() error = nil, want a refusal for an existing file without --force")
	}
}

func TestRunTeamAdd_RollsBackOnValidationFailure(t *testing.T) {
	resetTeamCreateFlags()
	resetTeamAddFlags()
	t.Cleanup(resetTeamAddFlags)
	dir := chdirTemp(t)

	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}
	teamDir := filepath.Join(dir, ".agent-teams", "dev")
	// Pre-seed two coordinator-role agents directly (bypassing team add) so
	// the team is already invalid (Phase 2's "more than one coordinator"
	// rule) before the add under test ever runs.
	for _, name := range []string{"lead-a", "lead-b"} {
		content := "---\nrole: coordinator\n---\nLeads the team.\n"
		if err := os.WriteFile(filepath.Join(teamDir, name+".md"), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	teamAddPreset = "coding"
	if err := runTeamAdd(nil, []string{"dev", "newagent"}); err == nil {
		t.Fatal("runTeamAdd() error = nil, want a failure for an already-invalid team")
	}
	if _, err := os.Stat(filepath.Join(teamDir, "newagent.md")); !os.IsNotExist(err) {
		t.Fatalf("newagent.md should have been rolled back, stat err = %v", err)
	}
}
