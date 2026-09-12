package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectCommandRunJSON(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--format", "json", "run", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		SchemaVersion int                `json:"schema_version"`
		Kind          inspectpkg.Kind    `json:"kind"`
		Data          inspectpkg.RunData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout.String())
	}
	if envelope.SchemaVersion != inspectpkg.SchemaVersion || envelope.Kind != inspectpkg.KindRun || envelope.Data.RunID != runID {
		t.Fatalf("envelope = %#v", envelope)
	}
}

func TestInspectCommandTaskTextDoesNotExposeRawOutput(t *testing.T) {
	workspace, runID, taskID := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "task", taskID, "--run", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Execution target: ollama/frozen-cli-model") {
		t.Fatalf("text output = %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "private worker output") {
		t.Fatalf("text output exposed task output: %q", stdout.String())
	}
}

func TestInspectCommandUsageErrorsExitTwo(t *testing.T) {
	command := newInspectCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"task", "task-1"})
	err := command.Execute()
	if err == nil {
		t.Fatal("task without --run succeeded")
	}
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitUsage {
		t.Fatalf("error = %v, want exit code %d", err, inspectpkg.ExitUsage)
	}
}

func TestRootCommandIncludesInspect(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"inspect", "run"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "run" || command.Parent().Name() != "inspect" {
		t.Fatalf("resolved command = %s", command.CommandPath())
	}
}

func buildInspectCommandFixture(t *testing.T) (workspace, runID, taskID string) {
	t.Helper()
	workspace = t.TempDir()
	runID = "run-cli-inspect"
	taskID = "task-cli-inspect"
	store, err := team.NewEventStore(workspace, runID, "session-cli-inspect")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	target := execution.ExecutionTarget{Backend: "ollama", Model: "frozen-cli-model"}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: inspectCommandJSON(t, map[string]any{"goal": "inspect CLI"})})
	appendEvent(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: taskID, Payload: inspectCommandJSON(t, map[string]any{
		"id": taskID, "status": team.TaskPending, "agent": "worker", "execution_target": target,
		"execution_topology": []execution.ExecutionTarget{target},
	})})
	appendEvent(team.RunEvent{Type: "task_completed", Actor: "worker", TaskID: taskID, Payload: inspectCommandJSON(t, map[string]any{
		"id": taskID, "status": team.TaskDone, "agent": "worker", "output": "private worker output", "execution_target": target,
		"execution_topology": []execution.ExecutionTarget{target},
	})})
	appendEvent(team.RunEvent{Type: "run_finished", Actor: "coordinator", Payload: inspectCommandJSON(t, team.RunResult{
		RunID: runID, Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks,
		Stats: team.RunStats{TasksTotal: 1, TasksDone: 1},
	})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return workspace, runID, taskID
}

func inspectCommandJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
