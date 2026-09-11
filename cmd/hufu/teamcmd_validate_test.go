package main

import (
	"os"
	"path/filepath"
	"strings"
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

// TestRunTeamValidate_ReportsSchemaVersion pins docs/architecture/
// team-schema-versioning.md §9: `hufu team validate` reports which schema
// the team declared (legacy-flat-v0 or hufu.io/v1alpha1).
func TestRunTeamValidate_ReportsSchemaVersion(t *testing.T) {
	agentContent := "---\nname: developer\nrole: worker\ntools: view,write,edit,grep,glob,ls,bash\n---\nImplement the change.\n"

	legacyDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(legacyDir, "developer.md"), []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyOut, err := runTeamValidateCaptured(t, []string{legacyDir})
	if err != nil {
		t.Fatalf("runTeamValidate(legacy) error = %v", err)
	}
	if !strings.Contains(legacyOut, "schema: legacy-flat-v0") {
		t.Errorf("legacy output missing schema line; got %q", legacyOut)
	}

	v1Dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(v1Dir, "team.yaml"), []byte("apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: v1-validate-team\nspec: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1Dir, "developer.md"), []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}
	v1Out, err := runTeamValidateCaptured(t, []string{v1Dir})
	if err != nil {
		t.Fatalf("runTeamValidate(v1alpha1) error = %v", err)
	}
	if !strings.Contains(v1Out, "schema: hufu.io/v1alpha1") {
		t.Errorf("v1alpha1 output missing schema line; got %q", v1Out)
	}
}

func runTeamValidateCaptured(t *testing.T, args []string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = runTeamValidate(nil, args) })
	return out, runErr
}

func TestRunTeamValidate_RejectsBothDirAndTeamFlag(t *testing.T) {
	original := teamValidateName
	teamValidateName = "some-team"
	t.Cleanup(func() { teamValidateName = original })

	if err := runTeamValidate(nil, []string{t.TempDir()}); err == nil {
		t.Fatal("runTeamValidate() error = nil, want an error when both a directory and --team are given")
	}
}
