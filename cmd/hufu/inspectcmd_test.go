package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
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

func TestRenderInspectTaskKnowledgeCoverage(t *testing.T) {
	var output bytes.Buffer
	envelope := &inspectpkg.Envelope{
		Kind: inspectpkg.KindTask,
		Data: inspectpkg.TaskData{
			RunID: "run-1", TaskID: "task-1",
			ExecutionTopology: []string{}, Attempts: []inspectpkg.AttemptData{}, ArtifactRefs: []string{}, ContextRefs: []string{}, MemoryRefs: []string{},
			KnowledgeCoverage: &inspectpkg.KnowledgeCoverageData{
				OutcomeCoverage:   inspectpkg.OutcomeCoverageData{KnownCount: 3, AssumedCount: 1, StaleCount: 1},
				InvariantCoverage: inspectpkg.InvariantCoverageData{TouchedPathCount: 4, UncoveredPathCount: 1},
			},
		},
	}
	if err := renderInspectText(&output, envelope); err != nil {
		t.Fatal(err)
	}
	if want := "Knowledge: 3 known, 1 assumed, 1 stale; invariants: 3/4 paths covered"; !strings.Contains(output.String(), want) {
		t.Fatalf("inspect output missing %q:\n%s", want, output.String())
	}
}

func TestInspectTextAndJSONUseSameRunProjection(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	jsonCommand := newInspectCommand()
	var jsonOutput bytes.Buffer
	jsonCommand.SetOut(&jsonOutput)
	jsonCommand.SetErr(&bytes.Buffer{})
	jsonCommand.SetArgs([]string{"--workspace", workspace, "--format", "json", "run", runID})
	if err := jsonCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data inspectpkg.RunData `json:"data"`
	}
	if err := json.Unmarshal(jsonOutput.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}

	textCommand := newInspectCommand()
	var textOutput bytes.Buffer
	textCommand.SetOut(&textOutput)
	textCommand.SetErr(&bytes.Buffer{})
	textCommand.SetArgs([]string{"--workspace", workspace, "run", runID})
	if err := textCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Run: " + envelope.Data.RunID,
		"Outcome: " + envelope.Data.Outcome,
		fmt.Sprintf("Tasks: %d total, %d done, %d unresolved", envelope.Data.TaskSummary.Total, envelope.Data.TaskSummary.Done, envelope.Data.TaskSummary.Unresolved),
	} {
		if !strings.Contains(textOutput.String(), expected) {
			t.Fatalf("text output does not contain JSON projection %q:\n%s", expected, textOutput.String())
		}
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

func TestInspectContextCommandRequiresRunAndProject(t *testing.T) {
	workspace, _, taskID := buildInspectCommandFixture(t)
	command := newInspectCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "context", taskID, "--project", "project-1"})
	err := command.Execute()
	if err == nil {
		t.Fatal("context without --run succeeded")
	}
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitUsage {
		t.Fatalf("error = %v, want exit code %d", err, inspectpkg.ExitUsage)
	}
}

func TestInspectEvidenceCommandJSON(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--format", "json", "evidence", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind         `json:"kind"`
		Data inspectpkg.EvidenceData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != inspectpkg.KindEvidence || envelope.Data.RunID != runID {
		t.Fatalf("evidence envelope = %#v", envelope)
	}
}

func TestInspectTraceCommandJSON(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--format", "json", "trace", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind      `json:"kind"`
		Data inspectpkg.TraceData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Kind != inspectpkg.KindTrace || len(envelope.Data.Entries) == 0 {
		t.Fatalf("trace envelope = %#v", envelope)
	}
}

func TestInspectReplayCommandReturnsIntegrityExitAfterDriftOutput(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	var events []team.RunEvent
	if err := team.StreamValidatedRunEvents(t.Context(), workspace, func(event team.RunEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint := team.ReduceToSessionData(events)
	checkpoint.Tasks[0].Status = team.TaskError
	if err := team.SaveSession(workspace, checkpoint); err != nil {
		t.Fatal(err)
	}
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--format", "json", "replay", runID})
	err := command.Execute()
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitIntegrity {
		t.Fatalf("replay drift error = %v", err)
	}
	var envelope struct {
		Data inspectpkg.ReplayData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode replay output: %v\n%s", err, stdout.String())
	}
	if envelope.Data.OverallStatus != "drift" {
		t.Fatalf("replay data = %#v", envelope.Data)
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

func TestInspectCommandHelpDescribesReadOnlyFacade(t *testing.T) {
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetArgs([]string{"--help"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"read-only facade", "run", "task", "evidence", "context", "trace", "replay"} {
		if !strings.Contains(stdout.String(), expected) {
			t.Fatalf("help does not contain %q:\n%s", expected, stdout.String())
		}
	}
}

func TestInspectCommandCompletesFormatWithoutFiles(t *testing.T) {
	command := newInspectCommand()
	complete, ok := command.GetFlagCompletionFunc("format")
	if !ok {
		t.Fatal("format completion is not registered")
	}
	values, directive := complete(command, nil, "j")
	if len(values) != 1 || values[0] != "json" {
		t.Fatalf("format completions = %v", values)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("format completion directive = %v", directive)
	}
	runCommand, _, err := command.Find([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	_, directive = runCommand.ValidArgsFunction(runCommand, nil, "run-")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("run ID completion directive = %v", directive)
	}
}

func TestInspectInvalidFormatFailsBeforeReadingWorkspace(t *testing.T) {
	workspace := t.TempDir()
	logsDir := filepath.Join(workspace, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "event_store.jsonl"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := newInspectCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--format", "yaml", "run", "run-1"})
	err := command.Execute()
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitUsage {
		t.Fatalf("error = %v, want usage exit code %d", err, inspectpkg.ExitUsage)
	}
	if !strings.Contains(err.Error(), "invalid format") {
		t.Fatalf("error = %v, want format validation before corrupt event-store read", err)
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
