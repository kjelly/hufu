package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func buildControlDecisionInspectFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	for _, run := range []struct {
		id, point, value, legacy string
	}{
		{id: "run-a", point: "path-reviewer", value: "false", legacy: "false"},
		{id: "run-b", point: "path-reviewer", value: "true", legacy: "false"},
	} {
		store, err := team.NewEventStore(workspace, run.id, "session-control")
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{"version": 1, "point": run.point, "mode": "shadow", "applied": "legacy", "status": "decided",
			"value": run.value, "confidence": 0.95, "accepted": true, "threshold": 0.9, "duration_ms": 150, "legacy": run.legacy, "agree": run.value == run.legacy})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range []team.RunEvent{
			{Type: "run_started", Actor: "coordinator", Payload: json.RawMessage(`{"goal":"inspect control decisions in /srv/private"}`)},
			{Type: string(team.EventControlDecisionObserved), Actor: "coder", TaskID: "1", Attempt: 1, Payload: payload},
		} {
			if _, err := store.AppendPersisted(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

func runInspectControlDecisions(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs(args)
	err := command.Execute()
	return stdout.String(), err
}

func TestInspectControlDecisionsAggregatesTheLineage(t *testing.T) {
	workspace := buildControlDecisionInspectFixture(t)
	output, err := runInspectControlDecisions(t, "--workspace", workspace, "--format", "json", "control-decisions")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind                 `json:"kind"`
		Data inspectpkg.ControlDecisionsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, output)
	}
	if envelope.Kind != inspectpkg.KindControlDecisions || len(envelope.Data.Summaries) != 1 {
		t.Fatalf("envelope = %#v", envelope)
	}
	if summary := envelope.Data.Summaries[0]; summary.Calls != 2 || summary.Compared != 2 || summary.Agreed != 1 {
		t.Fatalf("summary = %#v", summary)
	}
	if strings.Contains(output, "/srv/private") {
		t.Fatalf("inspect exposed run content: %s", output)
	}

	text, err := runInspectControlDecisions(t, "--workspace", workspace, "control-decisions", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Control decisions (run run-a", "path-reviewer", "1/1", "not a calibrated accuracy"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text missing %q:\n%s", want, text)
		}
	}
	all, err := runInspectControlDecisions(t, "--workspace", workspace, "control-decisions", "--all-branches")
	if err != nil || !strings.Contains(all, "Control decisions (all runs, all branches)") {
		t.Fatalf("all-branches text = %q, %v", all, err)
	}
	if _, err := runInspectControlDecisions(t, "--workspace", workspace, "--branch", "main", "control-decisions", "--all-branches"); err == nil {
		t.Fatal("--all-branches with --branch was accepted")
	}
	if _, err := runInspectControlDecisions(t, "--workspace", workspace, "control-decisions", "run-missing"); err == nil {
		t.Fatal("an unknown run was accepted")
	}
}
