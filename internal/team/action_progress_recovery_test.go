package team

import (
	"encoding/json/v2"
	"strconv"
	"testing"
)

func TestActionProgressReplaysNewActionAfterAttemptCounterReset(t *testing.T) {
	for _, nextAttempt := range []int{1, 3} {
		t.Run(strconv.Itoa(nextAttempt), func(t *testing.T) {
			c := newTestCoordinator(t)
			c.session = workflowTestSession(t)
			c.projectDir = c.session.Workspace
			c.phaseWorkflow = executePhaseWorkflow(t, c.session)
			c.initEventStore()
			task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "executor", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
			start := func(actionID string, attempt int) {
				t.Helper()
				if err := c.emitEvent("action_started", "runtime", item.ID, map[string]string{"action_id": actionID}); err != nil {
					t.Fatal(err)
				}
				if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "executing", "", attemptStartMetadata(attempt)); err != nil {
					t.Fatal(err)
				}
				c.setCurrentTaskAttempt(item.ID, attempt)
			}
			start("old-action", 2)
			c.recordActionProgress(task, item.ID, "old-action", 2, ActionProgress{Version: 1, Kind: "stage", Stage: "old"})
			if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskError, "failed", "", nil); err != nil {
				t.Fatal(err)
			}
			if err := c.CommitTaskResetForRetry(t.Context(), item.ID, "recover"); err != nil {
				t.Fatal(err)
			}
			start("new-action", nextAttempt)
			c.recordActionProgress(task, item.ID, "new-action", nextAttempt, ActionProgress{Version: 1, Kind: "stage", Stage: "new"})
			assertProgressReplayParity(t, c)

			// A higher counter from an old action cannot steal the new action's
			// progress. Nor can duplicate or out-of-order frames replace it.
			for _, stale := range []*ActionProgressObservation{
				{ActionProgress: ActionProgress{Version: 1, Kind: "stage", Stage: "stale"}, ActionID: "old-action", Attempt: 99, Sequence: 99},
				{ActionProgress: ActionProgress{Version: 1, Kind: "stage", Stage: "duplicate"}, ActionID: "new-action", Attempt: nextAttempt, Sequence: 1},
			} {
				if err := c.emitEvent("action_progress", "runtime", item.ID, map[string]any{"action_progress": stale}); err != nil {
					t.Fatal(err)
				}
				assertProgressReplayParity(t, c)
			}
			c.recordActionProgress(task, item.ID, "new-action", nextAttempt, ActionProgress{Version: 1, Kind: "stage", Stage: "finished"})
			outputs, hash, err := CanonicalizeRuntimeOutputs(map[string]any{"verified": true})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.taskTracker.TodoList().SetTypedResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: item.Agent, Attempt: nextAttempt, Source: "runtime", Status: TaskResultStatusSuccess,
				Summary: "accepted native work", RuntimeOutputs: outputs, RuntimeOutputsHash: hash,
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &ExecutionReceipt{
				RunID: c.executionRunID, TaskID: item.ID, Attempt: nextAttempt, ProducerID: item.Agent,
				ActionInvocationID: "new-action", RuntimeOutputsHash: hash, ExitCode: new(0),
			}); err != nil {
				t.Fatal(err)
			}
			if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskDone, "complete", "saved output", nil); err != nil {
				t.Fatal(err)
			}
			c.recordActionProgress(task, item.ID, "old-action", 2, ActionProgress{Version: 1, Kind: "stage", Stage: "late"})
			if err := c.emitEvent("action_progress", "runtime", item.ID, map[string]any{
				"action_progress": &ActionProgressObservation{ActionProgress: ActionProgress{Version: 1, Kind: "stage", Stage: "late"}, ActionID: "new-action", Attempt: nextAttempt, Sequence: 99},
			}); err != nil {
				t.Fatal(err)
			}
			assertProgressReplayParity(t, c)

			stored, err := c.eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			var snapshots []RunEvent
			for _, event := range stored {
				if event.Type != "action_progress" {
					snapshots = append(snapshots, event)
				}
			}
			if err := compareTaskProjection(c.taskTracker.TodoList().Items(), ReduceToTodoList(snapshots)); err != nil {
				t.Fatalf("lifecycle-only progress replay: %v", err)
			}
			session := ReduceToSessionData(stored)
			session.Tasks = c.taskTracker.TodoList().Items()
			const reason = "event-store projection mismatch: old progress reducer"
			if err := c.emitEvent("projection_mismatch", "coordinator", "", map[string]string{"reason": reason}); err != nil {
				t.Fatal(err)
			}
			session.RecoveryRequired, session.RecoveryReason = true, reason
			if err := SaveSession(c.session.Workspace, session); err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(session.Tasks)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			fresh := newTestCoordinator(t)
			fresh.session = c.session
			fresh.sessionData = LoadSession(c.session.Workspace)
			fresh.initEventStore()
			if fresh.sessionData.RecoveryRequired {
				t.Fatalf("fresh startup refused corrected progress: %s", fresh.sessionData.RecoveryReason)
			}
			after, err := json.Marshal(fresh.sessionData.Tasks)
			if err != nil || string(before) != string(after) {
				t.Fatalf("startup changed completed work: %v", err)
			}
		})
	}
}

func TestActionProgressReplayOrdering(t *testing.T) {
	for _, tc := range []struct {
		name, active, observedID string
		attempt, sequence        int
		wantNew                  bool
	}{
		{"bound_new_action_resets_counter", "new", "new", 1, 1, true},
		{"bound_old_action_higher_counter", "new", "old", 99, 99, false},
		{"same_action_next_sequence", "old", "old", 2, 3, true},
		{"same_action_duplicate", "old", "old", 2, 2, false},
		{"same_action_out_of_order", "old", "old", 2, 1, false},
		{"legacy_same_action_next_sequence", "", "old", 2, 3, true},
		{"legacy_new_action_higher_counter", "", "new", 3, 1, true},
		{"legacy_unbound_reset_refused", "", "new", 1, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prior := &ActionProgressObservation{ActionProgress: ActionProgress{Stage: "prior"}, ActionID: "old", Attempt: 2, Sequence: 2}
			observed := &ActionProgressObservation{ActionProgress: ActionProgress{Stage: "observed"}, ActionID: tc.observedID, Attempt: tc.attempt, Sequence: tc.sequence}
			result := replayActionProgress(prior, observed, tc.active)
			if (result.Stage == "observed") != tc.wantNew {
				t.Fatalf("progress=%+v wantNew=%v", result, tc.wantNew)
			}
			observed.Stage = "mutated"
			if result.Stage == "mutated" || prior.Stage != "prior" {
				t.Fatal("replay aliases or mutates its input")
			}
		})
	}
}

func assertProgressReplayParity(t *testing.T, c *Coordinator) {
	t.Helper()
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if err := compareTaskProjection(c.taskTracker.TodoList().Items(), ReduceToTodoList(events)); err != nil {
		t.Fatal(err)
	}
}
