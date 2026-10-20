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

// TestTeamLintRejectLegacyFanOutProcessExitContract is PR-3 of
// docs/tmp/now/05-workset-legacy-tsv-removal.md: --reject-legacy-fanout must
// not change the default exit code for a legacy fan-out source (it stays a
// warning, so default --fail-on=error still exits 0), but must fail the
// process once opted in.
func TestTeamLintRejectLegacyFanOutProcessExitContract(t *testing.T) {
	binary := buildProcessContractBinary(t)
	dir := writeTeamLintFanOutFixture(t, "items.tsv")

	if code, stdout, _ := runProcessContract(t, binary, "team", "lint", dir, "--format", "json"); code != 0 || !json.Valid(stdout) {
		t.Fatalf("default process: code=%d stdout=%q", code, stdout)
	}
	if code, stdout, _ := runProcessContract(t, binary, "team", "lint", dir, "--format", "json", "--reject-legacy-fanout"); code != 1 || !json.Valid(stdout) {
		t.Fatalf("reject-legacy-fanout process: code=%d stdout=%q", code, stdout)
	}
}

func writeTeamLintFanOutFixture(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "name: cli-lint-fanout\ntasks:\n  - id: fanout\n    agent: worker\n    goal: fan out\n    fan_out:\n      source: " + source + "\n      goal-template: process {key}\n"
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	coordinator := "---\nname: coordinator\nrole: coordinator\ntools: view\n---\nCoordinate.\n"
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte(coordinator), 0o644); err != nil {
		t.Fatal(err)
	}
	worker := "---\nname: worker\nrole: worker\ntools: view\n---\nWork.\n"
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
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

func TestTeamLintRejectLegacyTasksFlag(t *testing.T) {
	dir := writeTeamLintFixture(t, "name: cli-contracts\ntasks: []\n")
	previousPolicy := teamLintPolicy
	previousName, previousFormat, previousFailOn, previousIgnore := teamLintName, teamLintFormat, teamLintFailOn, teamLintIgnore
	t.Cleanup(func() {
		teamLintPolicy = previousPolicy
		teamLintName, teamLintFormat, teamLintFailOn, teamLintIgnore = previousName, previousFormat, previousFailOn, previousIgnore
	})
	teamLintName, teamLintFormat, teamLintFailOn, teamLintIgnore = "", "json", "error", nil
	teamLintPolicy.RejectLegacyTasks = false
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := runTeamLint(cmd, []string{dir}); err != nil {
		t.Fatal(err)
	}
	var document teamLintJSONDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Findings == nil {
		t.Fatal("missing findings")
	}
	count := 0
	for _, finding := range *document.Findings {
		if finding.Code == "legacy_tasks_key_deprecated" {
			count++
			if finding.Severity != "warning" || finding.Line == 0 || finding.FieldPath != "tasks" || finding.Suggestion == "" {
				t.Fatalf("warning = %+v", finding)
			}
		}
	}
	if count != 1 {
		t.Fatalf("legacy warning count = %d", count)
	}
	if teamLintCmd.Flags().Lookup("reject-legacy-tasks") == nil {
		t.Fatal("strict flag not registered")
	}
	teamLintPolicy.RejectLegacyTasks = true
	output.Reset()
	if err := runTeamLint(cmd, []string{dir}); err == nil {
		t.Fatal("strict lint accepted legacy tasks")
	}
}

func TestTeamLintFallbackProcessContract(t *testing.T) {
	binary := buildProcessContractBinary(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("---\npreset: readonly\n---\nInspect the project.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runProcessContract(t, binary, "team", "validate", dir); code != 0 {
		t.Fatalf("validate: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, test := range []struct {
		threshold string
		ignored   bool
		want      int
	}{{"error", false, 0}, {"warning", false, 0}, {"info", false, 1}, {"info", true, 0}} {
		args := []string{"team", "lint", dir, "--format", "json", "--fail-on", test.threshold}
		if test.ignored {
			args = append(args, "--ignore", "missing_coordinator")
		}
		code, stdout, stderr := runProcessContract(t, binary, args...)
		if code != test.want {
			t.Fatalf("lint %v: code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
		var doc teamLintJSONDocument
		if err := json.Unmarshal(stdout, &doc); err != nil {
			t.Fatal(err)
		}
		if doc.SchemaVersion != 1 || !doc.Complete || doc.Findings == nil || len(*doc.Findings) != 1 {
			t.Fatalf("document = %#v", doc)
		}
		finding := (*doc.Findings)[0]
		if finding.Code != "missing_coordinator" || finding.Severity != "info" || finding.Ignored != test.ignored {
			t.Fatalf("finding = %#v", finding)
		}
	}
}
