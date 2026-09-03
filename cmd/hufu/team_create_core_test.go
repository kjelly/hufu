package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// chdirTemp chdirs into a fresh temp directory for the duration of the
// test, restoring the original working directory on cleanup, and returns
// the temp directory's path.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	return dir
}

func TestResolvePresetFiles_EmptyNameProducesNoFiles(t *testing.T) {
	files, err := resolvePresetFiles("")
	if err != nil {
		t.Fatalf("resolvePresetFiles(\"\"): %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("resolvePresetFiles(\"\") = %v, want no files", files)
	}
}

func TestResolvePresetFiles_TeamPresetTakesPrecedenceOverAgentPreset(t *testing.T) {
	// "research" is both a team preset and an agent preset name; team
	// create --preset research must produce the team composition
	// (researcher.md + writer.md), not a single research-preset worker.
	files, err := resolvePresetFiles("research")
	if err != nil {
		t.Fatalf("resolvePresetFiles(\"research\"): %v", err)
	}
	for _, want := range []string{"researcher.md", "writer.md"} {
		if _, ok := files[want]; !ok {
			t.Errorf("resolvePresetFiles(\"research\") missing %s; got %v", want, files)
		}
	}
}

func TestResolvePresetFiles_AgentPresetFallback(t *testing.T) {
	files, err := resolvePresetFiles("readonly")
	if err != nil {
		t.Fatalf("resolvePresetFiles(\"readonly\"): %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("resolvePresetFiles(\"readonly\") = %v, want exactly one worker.md", files)
	}
	if _, ok := files["worker.md"]; !ok {
		t.Fatalf("resolvePresetFiles(\"readonly\") missing worker.md; got %v", files)
	}
}

func TestResolvePresetFiles_UnknownNameFails(t *testing.T) {
	if _, err := resolvePresetFiles("does-not-exist"); err == nil {
		t.Fatal("resolvePresetFiles(\"does-not-exist\") error = nil, want an error")
	}
}

func TestWriteTeamFilesWithValidation_RefusesOverwriteWithoutForce(t *testing.T) {
	dir := chdirTemp(t)
	teamDir := filepath.Join(dir, ".agent-teams", "dev")
	files, _ := resolvePresetFiles("coding-single")

	if err := writeTeamFilesWithValidation(teamDir, files, false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeTeamFilesWithValidation(teamDir, files, false); err == nil {
		t.Fatal("second write without --force error = nil, want a refusal")
	}
	if err := writeTeamFilesWithValidation(teamDir, files, true); err != nil {
		t.Fatalf("second write with --force: %v", err)
	}
}

func TestWriteTeamFilesWithValidation_RollsBackFreshDirectoryOnInvalidTeam(t *testing.T) {
	dir := chdirTemp(t)
	teamDir := filepath.Join(dir, ".agent-teams", "broken")

	invalid := map[string]string{
		"developer.md": "---\npreset: not-a-real-preset\n---\nContent.\n",
	}
	if err := writeTeamFilesWithValidation(teamDir, invalid, false); err == nil {
		t.Fatal("expected an error for an invalid team")
	}
	if _, err := os.Stat(teamDir); !os.IsNotExist(err) {
		t.Fatalf("team directory should have been rolled back, stat err = %v", err)
	}
}

func TestWriteTeamFilesWithValidation_DoesNotDeletePreexistingDirectoryOnFailure(t *testing.T) {
	dir := chdirTemp(t)
	teamDir := filepath.Join(dir, ".agent-teams", "existing")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(teamDir, "unrelated.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	invalid := map[string]string{
		"developer.md": "---\npreset: not-a-real-preset\n---\nContent.\n",
	}
	if err := writeTeamFilesWithValidation(teamDir, invalid, true); err == nil {
		t.Fatal("expected an error for an invalid team")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("pre-existing unrelated file must survive a failed --force overwrite: %v", err)
	}
}

func TestApplyExpandedDefaults_PrependsWithoutDroppingExisting(t *testing.T) {
	got := applyExpandedDefaults("model: local-model\n")
	for _, want := range []string{"max-rounds: 10", "timeout: 600", "max-retries: 2", "workspace: workspace", "model: local-model"} {
		if !strings.Contains(got, want) {
			t.Errorf("applyExpandedDefaults output missing %q; got:\n%s", want, got)
		}
	}
}
