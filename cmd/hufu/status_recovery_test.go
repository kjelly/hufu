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
	for _, tc := range []struct {
		name     string
		tasks    []string
		external bool
	}{
		{"single collection", []string{"11"}, false},
		{"multiple collections behind unknown external effect", []string{"11", "12", "13", "14"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHELL", "/bin/sh")
			workspace := t.TempDir()
			store, err := team.NewEventStore(workspace, "run-recovery", "session-recovery")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			appendEvent := func(kind team.EventType, taskID string, payload any) {
				t.Helper()
				if _, err := store.AppendPersisted(team.RunEvent{Type: string(kind), TaskID: taskID, Actor: "coordinator", Payload: inspectCommandJSON(t, payload)}); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent(team.EventRunStarted, "", map[string]any{"scope": map[string]string{"team_id": "review"}})
			for _, id := range tc.tasks {
				appendEvent(team.EventTaskCreated, id, map[string]any{"id": id, "agent": "reviewer", "status": team.TaskPending})
				appendEvent(team.EventTaskStarted, id, map[string]any{"id": id, "status": team.TaskInProgress})
				appendEvent(team.EventTaskBlocked, id, map[string]any{
					"id": id, "status": team.TaskBlocked, "side_effect": team.SideEffectNone,
					"failure_event":     &team.FailureEventPayload{FailureClass: team.FailureProtocol, FailureType: team.FailureTypeResultCollection, RetryDisposition: team.NeedsHuman},
					"execution_receipt": &team.ExecutionReceipt{RunID: "run-recovery", TaskID: id, ResultCollection: &team.ResultCollectionCheckpoint{State: team.ResultCollectionBlocked}},
				})
			}
			taskCount := len(tc.tasks)
			if tc.external {
				taskCount++
				appendEvent(team.EventTaskCreated, "external", map[string]any{"id": "external", "agent": "writer", "status": team.TaskPending})
				appendEvent(team.EventTaskBlocked, "external", map[string]any{
					"id": "external", "status": team.TaskBlocked, "side_effect": team.SideEffectExternalWrite, "recovery_state": team.RecoveryStateUnknown,
					"failure_event": &team.FailureEventPayload{FailureClass: team.FailureExecution, RetryDisposition: team.ReconcileOnly},
				})
			}
			appendEvent(team.EventRunFinished, "", team.RunResult{RunID: "run-recovery", Outcome: team.RunOutcomeBlocked, StopReason: team.StopReasonPolicyViolation, Stats: team.RunStats{TasksTotal: taskCount, TasksUnresolved: taskCount}})
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			// Deliberately stale: all hints must come from the event lineage.
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
					for _, want := range []string{"assessment required", "--dry-run", "Read-only preview"} {
						if !strings.Contains(output.String(), want) {
							t.Fatalf("missing %q in %s", want, output.String())
						}
					}
					for _, id := range tc.tasks {
						if !strings.Contains(output.String(), "Task "+id+":") || !strings.Contains(output.String(), "--repair-result "+id+" ") {
							t.Fatalf("missing assessment for %s: %s", id, output.String())
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
				if decoded.Recovery == nil || decoded.Recovery.State != "assessment_required" || len(decoded.Recovery.Blockers) != taskCount {
					t.Fatalf("status=%+v", decoded)
				}
				if tc.external && (len(decoded.Recovery.Actions) == 0 || decoded.Recovery.Actions[0].Target.TaskID != "external" || decoded.Recovery.Actions[0].ReasonCode != "external_effect_unknown") {
					t.Fatalf("lost primary safety priority: %+v", decoded.Recovery.Actions)
				}
				seen := make(map[string]bool)
				for _, action := range decoded.Recovery.Actions {
					if action.ID != operatorpkg.ActionAssessResultRepair {
						continue
					}
					if seen[action.Target.TaskID] || action.Target.RunID != decoded.RunID || action.Target.BranchID != decoded.BranchID || action.Kind != operatorpkg.ActionKindInspect || action.Availability != operatorpkg.ActionAvailabilityAvailable {
						t.Fatalf("wrong recovery scope: %+v", action)
					}
					seen[action.Target.TaskID] = true
				}
				if len(seen) != len(tc.tasks) {
					t.Fatalf("assessments=%v; want %v", seen, tc.tasks)
				}
			}
			after, err := os.ReadFile(filepath.Join(workspace, "runtime.sqlite"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("status modified durable evidence: %v", err)
			}
		})
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
