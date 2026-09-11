package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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
	teamLintIgnore = nil
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
	want, err := os.ReadFile(filepath.Join("testdata", "teamlint_empty.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), want) {
		t.Fatalf("JSON output changed\nwant:\n%s\ngot:\n%s", want, output.Bytes())
	}
}

func TestTeamLintJSONWritesOneDocument(t *testing.T) {
	dir := writeTeamLintFixture(t, "name: cli-lint\n")
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	teamLintName, teamLintFormat, teamLintFailOn, teamLintIgnore = "", "json", "error", nil
	if err := runTeamLint(cmd, []string{dir}); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	var document teamLintJSONDocument
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("second JSON decode = %v, want EOF", err)
	}
}

func TestTeamLintJSONBackwardCompatibleV1(t *testing.T) {
	type v1Consumer struct {
		SchemaVersion int    `json:"schema_version"`
		Team          string `json:"team"`
		Complete      bool   `json:"complete"`
		Findings      []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		} `json:"findings"`
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "teamlint_empty.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var consumer v1Consumer
	if err := json.Unmarshal(raw, &consumer); err != nil {
		t.Fatal(err)
	}
	if consumer.SchemaVersion != 1 || consumer.Team != "cli-lint" || !consumer.Complete || consumer.Findings == nil {
		t.Fatalf("v1 consumer = %#v", consumer)
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

func TestTeamLintIgnoreByCode(t *testing.T) {
	dir := writeTeamLintFixture(t, "name: cli-lint\nunattended: true\n")
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	teamLintName, teamLintFormat, teamLintFailOn = "", "json", "error"
	teamLintIgnore = []string{"acceptance_missing_for_unattended"}
	defer func() { teamLintIgnore = nil }()
	if err := runTeamLint(cmd, []string{dir}); err != nil {
		t.Fatal(err)
	}
	var document teamLintJSONDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Findings == nil || len(*document.Findings) != 1 || !(*document.Findings)[0].Ignored || document.Summary == nil || document.Summary.Error != 1 || document.Summary.Ignored != 1 {
		t.Fatalf("document = %#v", document)
	}
}

func TestTeamLintIgnoreByFileAndLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: cli-lint\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "---\nname: coordinator\nrole: coordinator\ntools: view\n---\nUse `ghost-tool` tool.\n"
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	teamLintName, teamLintFormat, teamLintFailOn = "", "json", "warning"
	teamLintIgnore = []string{"prompt_unknown_tool@coordinator.md:6"}
	defer func() { teamLintIgnore = nil }()
	if err := runTeamLint(cmd, []string{dir}); err != nil {
		t.Fatal(err)
	}
	var document teamLintJSONDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Findings == nil || len(*document.Findings) != 1 || !(*document.Findings)[0].Ignored {
		t.Fatalf("document = %#v", document)
	}
}

func TestTeamLintRejectsUnknownIgnoreCode(t *testing.T) {
	dir := writeTeamLintFixture(t, "name: cli-lint\n")
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	teamLintName, teamLintFormat, teamLintFailOn = "", "json", "error"
	teamLintIgnore = []string{"made_up_finding"}
	defer func() { teamLintIgnore = nil }()
	err := runTeamLint(cmd, []string{dir})
	if withCode, ok := err.(interface{ ProcessExitCode() int }); !ok || withCode.ProcessExitCode() != 2 {
		t.Fatalf("error = %v, want typed exit 2", err)
	}
	var document teamLintJSONDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Error == nil || document.Error.Kind != "cli_error" {
		t.Fatalf("document = %#v", document)
	}
}

func TestTeamLintProcessExitContract(t *testing.T) {
	binary := buildProcessContractBinary(t)
	dir := writeTeamLintFixture(t, "name: cli-lint\nunattended: true\n")
	if code, stdout, _ := runProcessContract(t, binary, "team", "lint", dir, "--format", "json"); code != 1 || !json.Valid(stdout) {
		t.Fatalf("threshold process: code=%d stdout=%q", code, stdout)
	}
	if code, stdout, _ := runProcessContract(t, binary, "team", "lint", dir, "--format", "json", "--ignore", "acceptance_missing_for_unattended"); code != 0 || !json.Valid(stdout) {
		t.Fatalf("ignored process: code=%d stdout=%q", code, stdout)
	}
	if code, stdout, _ := runProcessContract(t, binary, "team", "lint", dir, "--format", "json", "--ignore", "not_a_real_code"); code != 2 || !json.Valid(stdout) {
		t.Fatalf("invalid selector process: code=%d stdout=%q", code, stdout)
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
