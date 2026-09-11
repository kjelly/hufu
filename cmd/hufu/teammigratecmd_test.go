package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTeamMigrateCaptured(t *testing.T, args []string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = runTeamMigrate(nil, args) })
	return out, runErr
}

func writeMigrateCmdFixture(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunTeamMigrate_RequiresDryRun(t *testing.T) {
	dir := t.TempDir()
	writeMigrateCmdFixture(t, dir, "name: needs-dry-run\n")

	original := teamMigrateDryRun
	teamMigrateDryRun = false
	t.Cleanup(func() { teamMigrateDryRun = original })

	if _, err := runTeamMigrateCaptured(t, []string{dir}); err == nil {
		t.Fatal("runTeamMigrate() error = nil, want an error when --dry-run is not set")
	}
}

func TestRunTeamMigrate_RejectsUnsupportedTarget(t *testing.T) {
	dir := t.TempDir()
	writeMigrateCmdFixture(t, dir, "name: unsupported-target\n")

	teamMigrateDryRun = true
	t.Cleanup(func() { teamMigrateDryRun = false })
	originalTo := teamMigrateTo
	teamMigrateTo = "hufu.io/v2beta1"
	t.Cleanup(func() { teamMigrateTo = originalTo })

	if _, err := runTeamMigrateCaptured(t, []string{dir}); err == nil {
		t.Fatal("runTeamMigrate() error = nil, want an error for an unsupported --to target")
	}
}

func TestRunTeamMigrate_PrintsV1Alpha1Manifest(t *testing.T) {
	dir := t.TempDir()
	writeMigrateCmdFixture(t, dir, `name: migrate-cmd-fixture
description: exercised by the CLI test
max-rounds: 4
`)

	teamMigrateDryRun = true
	teamMigrateTo = "hufu.io/v1alpha1"
	t.Cleanup(func() {
		teamMigrateDryRun = false
		teamMigrateTo = "hufu.io/v1alpha1"
	})

	out, err := runTeamMigrateCaptured(t, []string{dir})
	if err != nil {
		t.Fatalf("runTeamMigrate() error = %v", err)
	}
	for _, want := range []string{
		"apiVersion: hufu.io/v1alpha1",
		"kind: AgentTeam",
		"name: migrate-cmd-fixture",
		"description: exercised by the CLI test",
		"max-rounds: 4",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("migrated output missing %q; got:\n%s", want, out)
		}
	}
}

func TestRunTeamMigrate_RejectsBothDirAndTeamFlag(t *testing.T) {
	teamMigrateDryRun = true
	t.Cleanup(func() { teamMigrateDryRun = false })
	original := teamMigrateName
	teamMigrateName = "some-team"
	t.Cleanup(func() { teamMigrateName = original })

	if _, err := runTeamMigrateCaptured(t, []string{t.TempDir()}); err == nil {
		t.Fatal("runTeamMigrate() error = nil, want an error when both a directory and --team are given")
	}
}
