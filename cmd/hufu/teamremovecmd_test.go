package main

import (
	"os"
	"path/filepath"
	"testing"
)

func resetTeamRemoveFlags() {
	teamRemoveForce = false
}

func TestRunTeamRemove_WithoutForceReportsOnly(t *testing.T) {
	resetTeamCreateFlags()
	resetTeamRemoveFlags()
	t.Cleanup(resetTeamRemoveFlags)
	dir := chdirTemp(t)
	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}

	if err := runTeamRemove(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamRemove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agent-teams", "dev")); err != nil {
		t.Fatalf("team directory should still exist without --force: %v", err)
	}
}

func TestRunTeamRemove_WithForceDeletes(t *testing.T) {
	resetTeamCreateFlags()
	resetTeamRemoveFlags()
	t.Cleanup(resetTeamRemoveFlags)
	dir := chdirTemp(t)
	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}

	teamRemoveForce = true
	if err := runTeamRemove(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamRemove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agent-teams", "dev")); !os.IsNotExist(err) {
		t.Fatalf("team directory should have been deleted, stat err = %v", err)
	}
}

func TestRunTeamRemove_NonexistentTeamFails(t *testing.T) {
	resetTeamRemoveFlags()
	t.Cleanup(resetTeamRemoveFlags)
	chdirTemp(t)

	if err := runTeamRemove(nil, []string{"does-not-exist"}); err == nil {
		t.Fatal("runTeamRemove() error = nil, want an error for a nonexistent team")
	}
}
