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
	operatorpkg "github.com/kjelly/hufu/internal/operator"
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

func TestInspectCommandOverviewJSONUsesQuerySuccessContract(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--workspace", workspace, "--output", "json", "overview", "--run", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind         `json:"kind"`
		Data inspectpkg.OverviewData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode overview: %v\n%s", err, stdout.String())
	}
	if envelope.Kind != inspectpkg.KindOverview || envelope.Data.Operation.Status != "succeeded" || envelope.Data.Snapshot == nil || envelope.Data.Error != nil {
		t.Fatalf("overview envelope = %#v", envelope)
	}
	if envelope.Data.Snapshot.Outcome.RunOutcome != string(team.RunOutcomePartial) || envelope.Data.Snapshot.Scope.RunID != runID {
		t.Fatalf("overview snapshot = %#v", envelope.Data.Snapshot)
	}
	if envelope.Data.Snapshot.PrimaryAction == nil || envelope.Data.Snapshot.PrimaryAction.ID != operatorpkg.ActionReviewResult {
		t.Fatalf("overview action = %#v", envelope.Data.Snapshot.PrimaryAction)
	}
	if stderr.Len() != 0 {
		t.Fatalf("overview JSON wrote stderr: %q", stderr.String())
	}
}

func TestInspectOverviewTextUsesUnifiedOperatorSummary(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "overview", "--run", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"State:", "What:", "Next:", "Data:"} {
		if strings.Count(stdout.String(), label) != 1 {
			t.Fatalf("overview summary label %q missing or duplicated:\n%s", label, stdout.String())
		}
	}
}

func TestInspectOverviewSummaryDoesNotEmitPersistedTerminalControls(t *testing.T) {
	snapshot := operatorpkg.OperatorSnapshot{
		Scope:         operatorpkg.ResolvedScope{WorkspaceExact: "/work/\x1b]8;;https://evil.example\x07link\x1b]8;;\x07", TeamName: "team\x1b[31mred", RunID: "run\nnext", BranchID: "main"},
		Activity:      operatorpkg.ActivityView{State: operatorpkg.ActivityBlocked},
		Outcome:       operatorpkg.OutcomeView{RunOutcome: "blocked"},
		Integrity:     operatorpkg.IntegrityView{Status: "valid"},
		Freshness:     operatorpkg.FreshnessView{EventID: "event", LiveState: "not_applicable"},
		LatestChanges: []operatorpkg.ChangeView{{EventOrdinal: 1, Kind: "task_failed\x1b[2J", Refs: []string{"ref\x07"}}},
	}
	action, err := operatorpkg.BuildAction(operatorpkg.ActionInspectTaskRecovery, operatorpkg.ActionBuildInput{
		Target:     operatorpkg.ActionTarget{Workspace: snapshot.Scope.WorkspaceExact, RunID: snapshot.Scope.RunID, TaskID: "task"},
		ReasonCode: "external_effect_unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.PrimaryAction = &action
	var output bytes.Buffer
	if err := renderInspectText(&output, &inspectpkg.Envelope{Kind: inspectpkg.KindOverview, Data: inspectpkg.OverviewData{Snapshot: &snapshot}}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(output.String(), "\x1b\x07") || strings.Contains(output.String(), "https://evil.example") {
		t.Fatalf("overview emitted terminal controls: %q", output.String())
	}
}

func TestInspectOverviewJSONFailureIsSingleDocumentAndSilentOnStderr(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	command := newInspectCommand()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--workspace", workspace, "--output", "json", "overview"})
	err := command.Execute()
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitUsage {
		t.Fatalf("error = %v, want usage exit %d", err, inspectpkg.ExitUsage)
	}
	var envelope struct {
		Kind inspectpkg.Kind         `json:"kind"`
		Data inspectpkg.OverviewData `json:"data"`
	}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("failure output is not one JSON document: %v\n%s", decodeErr, stdout.String())
	}
	if envelope.Kind != inspectpkg.KindOverview || envelope.Data.Operation.Status != "failed" || envelope.Data.Snapshot != nil || envelope.Data.Error == nil || envelope.Data.Error.Code != "target_not_found" {
		t.Fatalf("failure envelope = %#v", envelope)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON failure duplicated stderr: %q", stderr.String())
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Fatalf("overview created missing workspace: %v", statErr)
	}
}

func TestInspectOverviewTextFailureUsesOneStderrTemplate(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	command := newInspectCommand()
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"--workspace", workspace, "overview"})
	if err := command.Execute(); err == nil {
		t.Fatal("missing overview unexpectedly succeeded")
	}
	if stdout.Len() != 0 {
		t.Fatalf("text failure wrote stdout: %q", stdout.String())
	}
	if strings.Count(stderr.String(), "Error [target_not_found]") != 1 || !strings.Contains(stderr.String(), "no data was modified") {
		t.Fatalf("text failure stderr = %q", stderr.String())
	}
}

func TestInspectOutputAliasConflictFailsBeforeReadingWorkspace(t *testing.T) {
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
	command.SetArgs([]string{"--workspace", workspace, "--format", "text", "--output", "json", "overview"})
	err := command.Execute()
	if !strings.Contains(fmt.Sprint(err), "conflicts") || strings.Contains(fmt.Sprint(err), "event") {
		t.Fatalf("conflict validation did not precede workspace read: %v", err)
	}
}

func TestInspectEquivalentFormatAndOutputAliasesAreAccepted(t *testing.T) {
	workspace, runID, _ := buildInspectCommandFixture(t)
	command := newInspectCommand()
	var stdout bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "--format", "JSON", "--output", "json", "run", runID})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("equivalent aliases did not produce JSON: %q", stdout.String())
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

func TestRootCommandIncludesInspectOverview(t *testing.T) {
	command, _, err := newRootCommand().Find([]string{"inspect", "overview"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "overview" || command.Parent().Name() != "inspect" {
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
	for _, expected := range []string{"read-only facade", "overview", "run", "task", "evidence", "context", "trace", "replay", "--output"} {
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
	complete, ok = command.GetFlagCompletionFunc("output")
	if !ok {
		t.Fatal("output completion is not registered")
	}
	values, directive = complete(command, nil, "t")
	if len(values) != 1 || values[0] != "text" || directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("output completions = %v directive=%v", values, directive)
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
