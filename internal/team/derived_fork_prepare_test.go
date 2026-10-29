package team

import (
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func derivedForkPreparationFixture(t *testing.T) (*versionFixture, *WorkspaceSessionOps) {
	t.Helper()
	f := newVersionFixture(t)
	f.version.ProjectID, f.version.TeamName = "project", "review"
	f.events.SetBranchID("main")
	if err := SaveSessionTree(f.control, f.tree); err != nil {
		t.Fatal(err)
	}
	for _, event := range []RunEvent{
		{Type: string(EventRunStarted), RunID: "source-run", Actor: "coordinator", Payload: []byte(`{"team":"review"}`)},
		{Type: string(EventWorkflowStateChanged), RunID: "source-run", Actor: "runtime", Payload: []byte(`{"state":"DONE","phase_results":{"VERIFY":{"status":"SUCCESS"}}}`)},
	} {
		if _, err := f.events.AppendPersistedContext(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	f.write("source.txt", "verified source\n")
	ops := f.ops()
	if _, _, err := ops.SnapshotNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f, ops
}

func TestDerivedForkPreparationIsReadOnly(t *testing.T) {
	f, ops := derivedForkPreparationFixture(t)
	before := mustReadEvents(t, f.events)
	prepared, err := ops.PrepareDerivedFork(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	head := f.head("main")
	if prepared.intent.SourceSnapshotID != string(head.ID) || prepared.intent.CaptureOrigin != "known" ||
		prepared.headID != before[len(before)-1].ID || prepared.intent.SourceSnapshotEventID != head.CommitEventID ||
		prepared.intent.BranchID != "" || len(f.tree.Branches) != 1 {
		t.Fatal("read-only source preparation lost native identities or created a child")
	}
	if after := mustReadEvents(t, f.events); !reflect.DeepEqual(before, after) {
		t.Fatal("source preparation appended events")
	}
	operations, err := f.store.IncompleteOperations(t.Context())
	if err != nil || len(operations) != 0 {
		t.Fatalf("source preparation started an operation: %v", err)
	}
}

func TestDerivedForkPreparationRefusesWithoutRecovery(t *testing.T) {
	for _, failure := range []string{"observe", "unmanaged", "no_project_lock", "workspace_recovery", "deferred_checkpoint", "wrong_owner", "incomplete_operation", "terminal_commit", "unknown_effect", "wrong_team", "missing_cutoff"} {
		t.Run(failure, func(t *testing.T) {
			f, ops := derivedForkPreparationFixture(t)
			target := "main"
			switch failure {
			case "observe":
				ops.Version.Mode = versionstore.ModeObserve
			case "unmanaged":
				ops.Version.ProjectID = ""
			case "no_project_lock":
				ops.Version.HoldsProjectLock = false
			case "workspace_recovery", "deferred_checkpoint", "wrong_owner":
				if err := f.store.UpdateSubjectState(t.Context(), f.subject, func(state *versionstore.SubjectState) {
					switch failure {
					case "workspace_recovery":
						state.RecoveryRequired = true
					case "deferred_checkpoint":
						state.CheckpointDeferred = true
					case "wrong_owner":
						state.MaterializedWorkspaceID = "another-workspace"
					}
				}); err != nil {
					t.Fatal(err)
				}
			case "incomplete_operation":
				if _, err := f.store.CreateOperation(t.Context(), versionstore.Operation{WorkspaceID: f.version.WorkspaceID,
					BranchID: "main", TargetBranchID: "main", Kind: versionstore.OperationCheckout,
					FromSnapshotID: f.head("main").ID, ToSnapshotID: f.head("main").ID}); err != nil {
					t.Fatal(err)
				}
			case "terminal_commit":
				session := ReduceToSessionData(mustReadEvents(t, f.events))
				session.PendingTerminalCommit = &PendingTerminalCommit{RunID: "source-run", BranchID: "main", IdempotencyKey: "source-terminal"}
				if err := SaveSession(f.control, session); err != nil {
					t.Fatal(err)
				}
			case "unknown_effect":
				if _, err := f.events.AppendPersistedContext(t.Context(), RunEvent{Type: string(EventTaskCreated), RunID: "source-run", Actor: "coordinator", TaskID: "1",
					Payload: []byte(`{"id":"1","agent":"worker","status":"error","side_effect":"unknown"}`)}); err != nil {
					t.Fatal(err)
				}
			case "wrong_team":
				ops.Version.TeamName = "other"
			case "missing_cutoff":
				target = "missing-event"
			}
			before := mustReadEvents(t, f.events)
			stateBefore, _, err := f.store.GetSubjectState(t.Context(), f.subject)
			if err != nil {
				t.Fatal(err)
			}
			operationsBefore, err := f.store.IncompleteOperations(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ops.PrepareDerivedFork(t.Context(), target); err == nil {
				t.Fatal("unsafe or unsupported source was accepted")
			}
			stateAfter, _, err := f.store.GetSubjectState(t.Context(), f.subject)
			if err != nil || !reflect.DeepEqual(stateBefore, stateAfter) {
				t.Fatalf("refusal performed subject recovery: %v", err)
			}
			operationsAfter, err := f.store.IncompleteOperations(t.Context())
			if err != nil || !reflect.DeepEqual(operationsBefore, operationsAfter) {
				t.Fatalf("refusal completed a pending operation: %v", err)
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) || len(f.tree.Branches) != 1 || f.read("source.txt") != "verified source\n" {
				t.Fatal("refusal changed source events, branches, or files")
			}
		})
	}
}

func TestDerivedForkTaskSafetyUsesTypedEffectState(t *testing.T) {
	for _, test := range []struct {
		name  string
		item  TodoItem
		allow bool
	}{
		{"read_only_interrupted", TodoItem{Status: TaskInProgress, SideEffect: SideEffectNone}, true},
		{"workspace_interrupted", TodoItem{Status: TaskInProgress, SideEffect: SideEffectWorkspaceWrite}, true},
		{"external_unknown", TodoItem{Status: TaskError, SideEffect: SideEffectExternalWrite, RecoveryState: RecoveryStateUnknown}, false},
		{"external_partial", TodoItem{Status: TaskError, SideEffect: SideEffectExternalWrite, RecoveryState: RecoveryStatePartial}, false},
		{"external_complete", TodoItem{Status: TaskError, SideEffect: SideEffectExternalWrite, RecoveryState: RecoveryStateComplete}, true},
		{"external_not_started", TodoItem{Status: TaskError, SideEffect: SideEffectExternalWrite, RecoveryState: RecoveryStateNotStarted}, true},
		{"unknown_completed", TodoItem{Status: TaskDone, SideEffect: SideEffectUnknown}, false},
		{"missing_metadata", TodoItem{Status: TaskInProgress}, false},
		{"never_started", TodoItem{Status: TaskPending}, true},
		{"paused", TodoItem{Status: TaskInProgress, SideEffect: SideEffectNone, CheckpointPause: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateDerivedForkTaskSafety(&test.item); (err == nil) != test.allow {
				t.Fatalf("typed safety admission = %v, want allowed=%t", err, test.allow)
			}
		})
	}
}

func TestDerivedForkWorldSafetyRequiresCompletionAfterApply(t *testing.T) {
	prepared := RunEvent{Type: string(EventAttemptWorkspacePrepared), BranchID: "main", TaskID: "1", RunID: "original-run", Payload: []byte(`{"world_id":"world","attempt":1,"occurrence_attempt":1}`)}
	applied := RunEvent{Type: string(EventAttemptWorkspaceApplied), BranchID: "main", TaskID: "1", Payload: []byte(`{"world_id":"world"}`)}
	completed := RunEvent{Type: string(EventTaskCompleted), BranchID: "main", TaskID: "1", Payload: []byte(`{"status":"done","execution_receipt":{"execution_world_id":"world"}}`)}
	for _, test := range []struct {
		name   string
		events []RunEvent
		allow  bool
	}{
		{"prepared_only", []RunEvent{prepared}, true},
		{"applied_unfinished", []RunEvent{prepared, applied}, false},
		{"old_completion", []RunEvent{prepared, completed, applied}, false},
		{"settled", []RunEvent{prepared, applied, completed}, true},
		{"wrong_branch", []RunEvent{prepared, applied, func() RunEvent { e := completed; e.BranchID = "sibling"; return e }()}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateDerivedForkWorldSafety(test.events); (err == nil) != test.allow {
				data, _ := json.Marshal(test.events)
				t.Fatalf("world safety = %v, want allowed=%t; events=%s", err, test.allow, data)
			}
		})
	}
}
