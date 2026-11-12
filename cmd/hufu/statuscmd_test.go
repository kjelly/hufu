package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func TestSummarizeWorkspaceSession(t *testing.T) {
	status := summarizeWorkspaceSession("/tmp/workspace", &team.SessionData{
		Rounds: 2,
		Tasks: []*team.TodoItem{
			{Status: team.TaskDone}, {Status: team.TaskError}, {Status: team.TaskBlocked},
			{Status: team.TaskSkipped}, {Status: team.TaskPending},
		},
	})
	if status.Total != 5 || status.Done != 1 || status.Error != 2 || status.Skipped != 1 || status.Pending != 1 {
		t.Errorf("unexpected status: %#v", status)
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
