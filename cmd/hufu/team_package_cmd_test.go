package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/teampkg"
)

func TestTeamPackCommandWritesPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: cli-package\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("---\nrole: worker\n---\nWork.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "cli-package.hufu")
	command := newTeamPackCommand()
	stdout := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetArgs([]string{dir, "--version", "test-1", "--output", output})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "packed team cli-package version test-1") || !strings.Contains(got, "sha256:") {
		t.Fatalf("output = %q", got)
	}
}

func TestTeamPackageInspectCommandRendersTextAndJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: cli-inspect\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("---\nname: worker\nrole: worker\n---\nWork.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "cli-inspect.hufu")
	if _, err := teampkg.Pack(teampkg.PackOptions{TeamDir: dir, Version: "test-2", Output: output}); err != nil {
		t.Fatal(err)
	}

	textCommand := newTeamPackageInspectCommand()
	textOutput := new(bytes.Buffer)
	textCommand.SetOut(textOutput)
	textCommand.SetArgs([]string{output})
	if err := textCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := textOutput.String(); !strings.Contains(got, "authenticity: unverified") || !strings.Contains(got, "status: passed") || !strings.Contains(got, "normalized_name: cli-inspect") {
		t.Fatalf("text output = %q", got)
	}

	jsonCommand := newTeamPackageInspectCommand()
	jsonOutput := new(bytes.Buffer)
	jsonCommand.SetOut(jsonOutput)
	jsonCommand.SetArgs([]string{output, "--format", "json"})
	if err := jsonCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	var report teampkg.InspectionReport
	if err := json.Unmarshal(jsonOutput.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Name != "cli-inspect" || report.Authenticity != teampkg.AuthenticityUnverified || report.Compile.Status != "passed" {
		t.Fatalf("JSON report = %#v", report)
	}
}

func TestTeamPackageInspectCommandRendersFindingBeforeFailure(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("../escape")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("unsafe")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unsafe.hufu")
	if err := os.WriteFile(path, archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	command := newTeamPackageInspectCommand()
	command.SilenceErrors = true
	command.SilenceUsage = true
	stdout := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetArgs([]string{path, "--format", "json"})
	if err := command.Execute(); err == nil {
		t.Fatal("unsafe package was accepted")
	}
	var report teampkg.InspectionReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Compile.Status != "not_run" || len(report.Findings) != 1 || !strings.HasPrefix(report.Findings[0].Category, "path_") {
		t.Fatalf("failure report = %#v", report)
	}
}
