package main

import (
	"bytes"
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/improver"
)

func TestProbePlanJSONAndExclusiveOutput(t *testing.T) {
	config := "../../internal/improver/testdata/extraction/experiment.yaml"
	destination := filepath.Join(t.TempDir(), "preview.json")
	run := func() ([]byte, error) {
		command := newImproveProbeCommand()
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs([]string{"plan", "--config", config, "--json", "--out", destination})
		err := command.Execute()
		return output.Bytes(), err
	}
	raw, err := run()
	if err != nil {
		t.Fatal(err)
	}
	var preview improver.PlanPreview
	if err := json.Unmarshal(raw, &preview, json.RejectUnknownMembers(true)); err != nil || preview.Executable {
		t.Fatalf("stdout is not a static JSON preview: %s, %v", raw, err)
	}
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run(); err == nil {
		t.Fatal("existing preview overwritten")
	} else if exit, ok := errors.AsType[*probeExitError](err); !ok || exit.ProcessExitCode() != 2 {
		t.Fatalf("wrong failure exit code: %v", err)
	}
	after, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("exclusive output changed existing bytes")
	}
}
