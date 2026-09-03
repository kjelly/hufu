package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunTeamShow_ShowsAuthoredFilesWithoutDefaults(t *testing.T) {
	resetTeamCreateFlags()
	dir := chdirTemp(t)
	teamCreateModel = "local-model"
	t.Cleanup(resetTeamCreateFlags)
	if err := runTeamCreate(nil, []string{"dev"}); err != nil {
		t.Fatalf("runTeamCreate: %v", err)
	}

	out := captureStdout(t, func() {
		teamShowName = ""
		if err := runTeamShow(nil, []string{filepath.Join(dir, ".agent-teams", "dev")}); err != nil {
			t.Fatalf("runTeamShow: %v", err)
		}
	})

	if !strings.Contains(out, "worker.md") {
		t.Errorf("show output missing worker.md; got:\n%s", out)
	}
	if !strings.Contains(out, "model: local-model") {
		t.Errorf("show output missing authored model override; got:\n%s", out)
	}
	if !strings.Contains(out, "preset: coding") {
		t.Errorf("show output missing worker's authored preset; got:\n%s", out)
	}
	// Show must not expand built-in defaults that were never authored.
	if strings.Contains(out, "max-rounds") {
		t.Errorf("show output should not expand unauthored defaults; got:\n%s", out)
	}
}

func TestRunTeamShow_NonexistentDirectoryFails(t *testing.T) {
	dir := chdirTemp(t)
	teamShowName = ""
	if _, err := os.Stat(filepath.Join(dir, "nope")); !os.IsNotExist(err) {
		t.Fatal("test precondition failed: directory should not exist")
	}
	if err := runTeamShow(nil, []string{filepath.Join(dir, "nope")}); err == nil {
		t.Fatal("runTeamShow() error = nil, want an error for a nonexistent directory")
	}
}
