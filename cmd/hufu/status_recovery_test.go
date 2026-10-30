package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestStatusShowsCanonicalRecoveryWithoutWorkersOrMutatingEvidence(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-recovery", "session-recovery")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []team.RunEvent{
		{Type: string(team.EventRunStarted), Payload: inspectCommandJSON(t, map[string]any{"scope": map[string]string{"team_id": "review"}})},
		{Type: string(team.EventTaskCreated), TaskID: "11", Payload: inspectCommandJSON(t, map[string]any{"id": "11", "agent": "reviewer", "status": team.TaskPending})},
		{Type: string(team.EventTaskStarted), TaskID: "11", Payload: inspectCommandJSON(t, map[string]any{"id": "11", "status": team.TaskInProgress})},
		{Type: string(team.EventTaskBlocked), TaskID: "11", Payload: inspectCommandJSON(t, map[string]any{
			"id": "11", "status": team.TaskBlocked,
			"failure_event":     &team.FailureEventPayload{FailureClass: team.FailureProtocol, FailureType: "collection", RetryDisposition: team.NeedsHuman},
			"execution_receipt": &team.ExecutionReceipt{RunID: "run-recovery", TaskID: "11", ResultCollection: &team.ResultCollectionCheckpoint{State: team.ResultCollectionBlocked}},
		})},
		{Type: string(team.EventRunFinished), Payload: inspectCommandJSON(t, team.RunResult{RunID: "run-recovery", Outcome: team.RunOutcomeBlocked, StopReason: team.StopReasonPolicyViolation, Stats: team.RunStats{TasksTotal: 1, TasksUnresolved: 1}})},
	} {
		event.Actor = "coordinator"
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Deliberately stale checkpoint: the recovery hint must come from events.
	if err := team.SaveSession(workspace, &team.SessionData{Tasks: []*team.TodoItem{{ID: "wrong-task", Status: team.TaskDone}}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(workspace, "runtime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	previousWorkspace, previousJSON, previousWorkers := statusWorkspace, statusJSON, statusWorkers
	t.Cleanup(func() { statusWorkspace, statusJSON, statusWorkers = previousWorkspace, previousJSON, previousWorkers })
	statusWorkspace, statusWorkers = workspace, false
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	for _, asJSON := range []bool{false, true} {
		statusJSON = asJSON
		output.Reset()
		if err := runStatus(command, nil); err != nil {
			t.Fatal(err)
		}
		if !asJSON {
			for _, want := range []string{"Task 11:", "assessment required", "--repair-result 11", "--dry-run", "Read-only preview"} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("missing %q in %s", want, output.String())
				}
			}
			if strings.Contains(output.String(), "Task wrong-task:") {
				t.Fatal("checkpoint task leaked into canonical recovery")
			}
			continue
		}
		var decoded workspaceStatus
		if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Recovery == nil || decoded.Recovery.State != "assessment_required" || len(decoded.Recovery.Blockers) != 1 {
			t.Fatalf("status=%+v", decoded)
		}
		found := false
		for _, action := range decoded.Recovery.Actions {
			if action.ID == operatorpkg.ActionAssessResultRepair {
				found = true
				if action.Target.TaskID != "11" || action.Target.RunID != decoded.RunID || action.Target.BranchID != decoded.BranchID || action.Kind != "inspect" {
					t.Fatalf("wrong recovery scope: %+v", action)
				}
			}
		}
		if !found {
			t.Fatal("JSON omitted repair assessment")
		}
	}
	after, err := os.ReadFile(filepath.Join(workspace, "runtime.sqlite"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status modified durable evidence: %v", err)
	}
}

func TestWorkspaceRecoveryOmitsHealthyStateAndNeverOffersMutation(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	if workspaceRecoveryFromSnapshot(operatorpkg.OperatorSnapshot{Activity: operatorpkg.ActivityView{State: operatorpkg.ActivityFinished}}) != nil {
		t.Fatal("healthy status claims recovery required")
	}
	for _, availability := range []string{"available", "blocked"} {
		snapshot := operatorpkg.OperatorSnapshot{
			Blockers:         []operatorpkg.DiagnosticView{{Code: "task_blocked", Ref: "11"}},
			PrimaryAction:    &operatorpkg.ActionSuggestion{ID: "inspect", Kind: "inspect", Availability: availability, Argv: []string{"hufu", "inspect", "task", "a'b $(touch forbidden)"}},
			SecondaryActions: []operatorpkg.ActionSuggestion{{Kind: "mutate", Availability: "available", Argv: []string{"hufu", "session", "retry"}}},
		}
		before := *snapshot.PrimaryAction
		recovery := workspaceRecoveryFromSnapshot(snapshot)
		if recovery == nil || len(recovery.Actions) != 1 || !reflect.DeepEqual(*snapshot.PrimaryAction, before) {
			t.Fatalf("recovery=%+v", recovery)
		}
		var output bytes.Buffer
		writeWorkspaceRecovery(&output, recovery)
		if availability == "blocked" && strings.Contains(output.String(), "touch forbidden") {
			t.Fatal("unavailable command printed")
		}
		if availability == "available" && !strings.Contains(output.String(), `'a'"'"'b $(touch forbidden)'`) {
			t.Fatal("unsafe command quoting")
		}
	}
}
