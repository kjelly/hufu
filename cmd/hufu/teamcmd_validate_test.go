package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Phase 4 (spec.md Specification 05): `hufu team validate` now goes through
// internalteam.CompileTeam/ValidateEffectiveTeam instead of LoadTeam/
// LintTeamContracts directly, so validate/dry-run/explain/runtime converge
// on one pipeline (Specification 02 §7).

func TestRunTeamValidate_ValidTeamSucceeds(t *testing.T) {
	dir := t.TempDir()
	agentPath := filepath.Join(dir, "developer.md")
	content := "---\nname: developer\nrole: worker\ntools: view,write,edit,grep,glob,ls,bash\n---\nImplement the change.\n"
	if err := os.WriteFile(agentPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runTeamValidate(nil, []string{dir}); err != nil {
		t.Fatalf("runTeamValidate() error = %v, want nil for a valid team", err)
	}
}

func TestRunTeamValidate_NonexistentDirectoryFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")

	if err := runTeamValidate(nil, []string{dir}); err == nil {
		t.Fatal("runTeamValidate() error = nil, want an error for a nonexistent team directory")
	}
}

func TestRunTeamValidate_RejectsBothDirAndTeamFlag(t *testing.T) {
	original := teamValidateName
	teamValidateName = "some-team"
	t.Cleanup(func() { teamValidateName = original })

	if err := runTeamValidate(nil, []string{t.TempDir()}); err == nil {
		t.Fatal("runTeamValidate() error = nil, want an error when both a directory and --team are given")
	}
}
