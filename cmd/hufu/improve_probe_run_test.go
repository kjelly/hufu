package main

import (
	"bytes"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/improver"
)

func executeProbeCommand(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	command := newImproveProbeCommand()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs(args)
	err := command.Execute()
	if bytes.Contains(stdout.Bytes(), []byte("\x1b")) {
		t.Fatal("probe stdout contains terminal control sequences")
	}
	return stdout.Bytes(), err
}

func TestProbeRunShowResumeFrozenJSON(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.CopyFS(source, os.DirFS("../../internal/improver/testdata/extraction")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspace")
	raw, err := executeProbeCommand(t, "run", "--config", filepath.Join(source, "experiment.yaml"), "--workspace", path, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report improver.ExperimentReport
	if err := json.Unmarshal(raw, &report, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if report.Decision != "simulation_only" || report.Budget.SpentTokens != 240 || report.Coverage.Scored != 2 {
		t.Fatalf("unexpected CLI report: %+v", report)
	}
	if _, err := executeProbeCommand(t, "run", "--config", filepath.Join(source, "experiment.yaml"), "--workspace", path, "--json"); err == nil {
		t.Fatal("duplicate experiment overwritten")
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"show", "resume"} {
		raw, err := executeProbeCommand(t, operation, "--experiment", "extraction_pair", "--workspace", path, "--json")
		if err != nil {
			t.Fatalf("%s: %v", operation, err)
		}
		var loaded improver.ExperimentReport
		if err := json.Unmarshal(raw, &loaded, json.RejectUnknownMembers(true)); err != nil || !reflect.DeepEqual(report, loaded) {
			t.Fatalf("%s report changed: %v", operation, err)
		}
	}
}

func TestProbeShowMissingAndNonterminalTUIAreSideEffectFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent")
	if _, err := executeProbeCommand(t, "show", "--experiment", "missing", "--workspace", path, "--json"); err == nil {
		t.Fatal("show accepted a missing workspace")
	}
	if _, err := executeProbeCommand(t, "run", "--config", "unused.yaml", "--workspace", path, "--tui"); err == nil {
		t.Fatal("TUI accepted nonterminal streams")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid read/display created workspace")
	}
}

func TestRefineCLIUsesFrozenOfflineExample(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.CopyFS(source, os.DirFS("../../internal/improver/testdata/refinement")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "workspace")
	command := newProbeExecutionCommand("refine")
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--config", filepath.Join(source, "experiment.yaml"), "--workspace", path, "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var report improver.ExperimentReport
	if err := json.Unmarshal(stdout.Bytes(), &report, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	if report.Decision != "simulation_only" || report.SelectedVariant != "domain" ||
		report.Coverage.Scored != 4 || report.Budget.SpentTokens != 830 || len(report.Stages) != 8 {
		t.Fatalf("unexpected refine report: %+v", report)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"show", "resume"} {
		raw, err := executeProbeCommand(t, operation, "--experiment", "extraction_refinement", "--workspace", path, "--json")
		if err != nil {
			t.Fatal(err)
		}
		var loaded improver.ExperimentReport
		if err := json.Unmarshal(raw, &loaded, json.RejectUnknownMembers(true)); err != nil || !reflect.DeepEqual(loaded, report) {
			t.Fatalf("%s changed the frozen refinement: %v", operation, err)
		}
	}
}
