package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestTeamLintJSONStable(t *testing.T) {
	dir := writeTeamLintFixture(t, "name: cli-lint\n")
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	teamLintName = ""
	teamLintFormat = "json"
	teamLintFailOn = "error"
	if err := runTeamLint(cmd, []string{dir}); err != nil {
		t.Fatal(err)
	}
	var document teamLintJSONDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("decode output %q: %v", output.String(), err)
	}
	if document.SchemaVersion != 1 || document.Team != "cli-lint" || !document.Complete || document.Findings == nil || len(*document.Findings) != 0 || document.Summary == nil {
		t.Fatalf("document = %#v", document)
	}
}

func TestTeamLintExit2UsesTypedError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: [bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	teamLintName = ""
	teamLintFormat = "json"
	teamLintFailOn = "error"
	err := runTeamLint(cmd, []string{dir})
	withCode, ok := err.(interface{ ProcessExitCode() int })
	if !ok || withCode.ProcessExitCode() != 2 {
		t.Fatalf("error = %v, want ProcessExitCode() == 2", err)
	}
	var document teamLintJSONDocument
	if decodeErr := json.Unmarshal(output.Bytes(), &document); decodeErr != nil {
		t.Fatalf("decode error envelope %q: %v", output.String(), decodeErr)
	}
	if document.Error == nil || document.Error.Kind != "load_error" || document.Complete {
		t.Fatalf("document = %#v", document)
	}
}

func writeTeamLintFixture(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "---\nname: coordinator\nrole: coordinator\ntools: view\n---\nCoordinate.\n"
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
