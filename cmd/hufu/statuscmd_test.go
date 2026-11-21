package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func TestStatusTaskCountsBySavedState(t *testing.T) {
	workspace := t.TempDir()
	session := &team.SessionData{
		Rounds: 2,
		Tasks: []*team.TodoItem{
			{ID: "1", Status: team.TaskDone}, {ID: "2", Status: team.TaskError}, {ID: "3", Status: team.TaskBlocked},
			{ID: "4", Status: team.TaskSkipped}, {ID: "5", Status: team.TaskPending}, {ID: "6", Status: team.TaskInProgress},
			{ID: "7", Status: team.TaskVerifying}, {ID: "8", Status: team.TaskPlanned}, {ID: "9", Status: team.TaskPaused},
			{ID: "10", Status: team.TaskProtocolIncomplete}, {ID: "11", Status: "future_status"},
		},
	}
	if err := team.SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	binary := buildTestBinary(t)
	for _, flags := range [][]string{{}, {"--output", "json"}, {"--json"}, {"--json", "--output", "json"}} {
		args := append([]string{"status", "--workspace", workspace}, flags...)
		command := exec.CommandContext(t.Context(), binary, args...)
		command.Dir = workspace
		out, err := command.Output()
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if len(flags) == 0 {
			for _, count := range []string{"1 pending", "1 in progress", "1 verifying", "1 blocked", "1 error", "1 paused", "1 planned", "1 protocol incomplete", "1 unknown", "(11 total)"} {
				if !strings.Contains(string(out), count) {
					t.Fatalf("missing %q in %s", count, out)
				}
			}
			continue
		}
		var counts map[string]json.RawMessage
		if err := json.Unmarshal(out, &counts); err != nil {
			t.Fatalf("invalid JSON: %v, stdout=%s", err, out)
		}
		for _, key := range []string{"done", "error", "blocked", "skipped", "pending", "in_progress", "verifying", "planned", "paused", "protocol_incomplete", "unknown"} {
			if string(counts[key]) != "1" {
				t.Errorf("%v: count %s = %s, want 1", flags, key, counts[key])
			}
		}
		if string(counts["total"]) != "11" {
			t.Errorf("%v: total = %s, want 11", flags, counts["total"])
		}
	}
}

func TestStatusWorkspaceReadErrors(t *testing.T) {
	dir := t.TempDir()
	empty, corrupt, missing := filepath.Join(dir, "empty"), filepath.Join(dir, "corrupt"), filepath.Join(dir, "missing")
	for _, workspace := range []string{empty, corrupt} {
		if err := os.Mkdir(workspace, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	database := runtimestore.Path(corrupt)
	original := []byte("corrupt fixture")
	if err := os.WriteFile(database, original, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := statusWorkspace
	t.Cleanup(func() { statusWorkspace = previous })
	for _, test := range []struct {
		workspace, problem, next string
	}{
		{missing, "does not exist", "hufu workspace status"},
		{empty, "no saved session", "locate recorded runs"},
		{corrupt, "cannot read the session", "PRAGMA quick_check"},
	} {
		statusWorkspace = test.workspace
		command := &cobra.Command{}
		var stdout bytes.Buffer
		command.SetOut(&stdout)
		err := runStatus(command, nil)
		if err == nil || !strings.Contains(err.Error(), test.problem) || !strings.Contains(err.Error(), test.next) ||
			strings.Contains(err.Error(), "--workspace") || stdout.Len() != 0 {
			t.Fatalf("status error = %v, stdout=%q", err, stdout.String())
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("missing workspace was created: %v", err)
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Fatalf("empty workspace changed: %v, %v", entries, err)
	}
	if contents, err := os.ReadFile(database); err != nil || !bytes.Equal(contents, original) {
		t.Fatalf("corrupt database changed: %q, %v", contents, err)
	}
}

func TestRunStatusDisplaysCanonicalInvocationIdentity(t *testing.T) {
	workspace := t.TempDir()
	const (
		runID        = "run-status"
		invocationID = "inv-status"
		sessionID    = "session-status"
	)
	store, err := team.NewEventStore(workspace, runID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	store.SetInvocationID(invocationID)
	for _, event := range []team.RunEvent{
		{Type: string(team.EventRunStarted), Actor: "coordinator", Payload: []byte(`{"scope":{"project_id":"project","team_id":"review"}}`)},
		{Type: string(team.EventRunFinished), Actor: "coordinator", Payload: []byte(`{"run_id":"run-status","outcome":"completed","goal_satisfied":true,"stop_reason":"completed"}`)},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSession(workspace, &team.SessionData{
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		RunResult: &team.RunResult{RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true, StopReason: team.StopReasonCompleted},
	}); err != nil {
		t.Fatal(err)
	}

	previousWorkspace, previousJSON := statusWorkspace, statusJSON
	t.Cleanup(func() { statusWorkspace, statusJSON = previousWorkspace, previousJSON })
	statusWorkspace = workspace
	statusJSON = false
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Team:       review", "Run:        " + runID, "Invocation: " + invocationID, "Branch:     main"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("status output missing %q:\n%s", expected, output.String())
		}
	}

	statusJSON = true
	output.Reset()
	if err := runStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	var decoded workspaceStatus
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, output.String())
	}
	if decoded.Team != "review" || decoded.RunID != runID || decoded.InvocationID != invocationID || decoded.BranchID != "main" {
		t.Fatalf("status identity = %#v", decoded)
	}
}

func TestApplyWorkspaceStatusScopeKeepsLegacyInvocationUnavailable(t *testing.T) {
	status := workspaceStatus{}
	applyWorkspaceStatusScope(&status, operatorScopeWithoutInvocation())
	if status.InvocationID != "" || status.RunID != "legacy-run" {
		t.Fatalf("legacy status identity = %#v", status)
	}
}

func operatorScopeWithoutInvocation() operatorpkg.ResolvedScope {
	return operatorpkg.ResolvedScope{RunID: "legacy-run", BranchID: "main"}
}

func TestStatusPresentsProviderDiagnosisInTextAndJSON(t *testing.T) {
	workspace := t.TempDir()
	diagnosis := &team.ActionFailureDiagnosis{Version: 1, Code: "capacity_exceeded", Summary: "Evidence exceeds capacity", CauseStatus: "confirmed", Cause: "Fixed metadata exceeds the budget", Evidence: []team.ActionDiagnosticFact{{Name: "observed_bytes", Value: "411383"}}}
	failure := &team.FailureEventPayload{TaskID: "1", FailureClass: team.FailureExecution, RetryDisposition: team.RetryNone, Summary: "action failed", Diagnosis: diagnosis}
	if err := team.SaveSession(workspace, &team.SessionData{Tasks: []*team.TodoItem{{ID: "1", Status: team.TaskError, FailureEvent: failure}}}); err != nil {
		t.Fatal(err)
	}
	previousWorkspace, previousJSON, previousWorkers := statusWorkspace, statusJSON, statusWorkers
	t.Cleanup(func() { statusWorkspace, statusJSON, statusWorkers = previousWorkspace, previousJSON, previousWorkers })
	statusWorkspace, statusWorkers = workspace, false
	for _, asJSON := range []bool{false, true} {
		statusJSON = asJSON
		command := &cobra.Command{}
		var output bytes.Buffer
		command.SetOut(&output)
		if err := runStatus(command, nil); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "capacity_exceeded") || !strings.Contains(output.String(), "411383") {
			t.Fatalf("status diagnosis missing: %s", output.String())
		}
		if asJSON {
			var decoded workspaceStatus
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || len(decoded.Failures) != 1 || decoded.Failures[0].Diagnosis == nil {
				t.Fatalf("JSON diagnosis: %s err=%v", output.String(), err)
			}
		}
	}
}
