package team

import (
	"encoding/json/v2"
	"testing"
)

func TestAbortedPreflightTerminalRecovery(t *testing.T) {
	for _, mode := range []string{"aborted", "missing_abort", "model_activity", "tool_activity", "unknown_event", "started_turn", "wrong_key", "wrong_branch"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.sessionData = NewSession()
			store, err := NewEventStore(c.session.Workspace, c.executionRunID, "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			c.eventStore = store
			store.SetBranchID("main")
			started, err := store.AppendPersisted(RunEvent{Type: "run_started", RunID: c.executionRunID, Actor: "coordinator", Payload: []byte(`{"team":"test"}`)})
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := ContinuationCheckpoint{Status: "aborted"}
			if mode == "started_turn" {
				checkpoint.TurnCount = 1
			}
			payload, _ := json.Marshal(checkpoint)
			aborted, err := store.AppendPersisted(RunEvent{Type: "coordinator_continuation_checkpoint", RunID: c.executionRunID, Actor: "coordinator", Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			lineage := []RunEvent{started, aborted}
			pending := &PendingTerminalCommit{RunID: c.executionRunID, IdempotencyKey: terminalFinishedIdempotencyKey(c.executionRunID), BranchID: "main"}
			switch mode {
			case "missing_abort":
				lineage = lineage[:1]
			case "model_activity":
				lineage = append(lineage, RunEvent{RunID: c.executionRunID, BranchID: "main", Type: "context_window_admission"})
			case "tool_activity":
				lineage = append(lineage, RunEvent{RunID: c.executionRunID, BranchID: "main", Type: "tool_observation_started"})
			case "unknown_event":
				lineage = append(lineage, RunEvent{RunID: c.executionRunID, BranchID: "main", Type: "unknown"})
			case "wrong_key":
				pending.IdempotencyKey = "other"
			case "wrong_branch":
				pending.BranchID = "other"
			}
			if mode != "aborted" {
				_, recovered, err := c.recoverAbortedPreflightTerminal(lineage, pending)
				if err != nil || recovered {
					t.Fatalf("unsafe recovery=%t error=%v", recovered, err)
				}
				return
			}
			// Restart reconciliation must consume the exact canonical event,
			// clear only its pending marker, and keep existing worker tasks.
			c.sessionData.PendingTerminalCommit = pending
			c.sessionData.RecoveryRequired = true
			c.sessionData.Tasks = []*TodoItem{{ID: "existing", Status: TaskDone}}
			present, reconciled := c.reconcilePendingTerminalCommit(NewSessionTree(), "main")
			if !present || !reconciled || c.sessionData.PendingTerminalCommit != nil || c.sessionData.RecoveryRequired || len(c.sessionData.Tasks) != 1 {
				t.Fatal("terminal reconciliation discarded the existing session")
			}
			events, err := store.ReadEvents()
			if err != nil || len(events) != 3 {
				t.Fatalf("terminal recovery duplicated an event: count=%d error=%v", len(events), err)
			}
			var result RunFinishedEventPayload
			if err := json.Unmarshal(events[2].Payload, &result); err != nil || result.Outcome != RunOutcomeFailed || result.GoalSatisfied || result.RunInputs != nil {
				t.Fatalf("preflight recovery claimed completed work: %#v error=%v", result, err)
			}
			c.sessionData.PendingTerminalCommit = pending
			c.sessionData.RecoveryRequired = true
			if _, reconciled := c.reconcilePendingTerminalCommit(NewSessionTree(), "main"); !reconciled {
				t.Fatal("could not reconcile the already committed terminal event")
			}
			events, err = store.ReadEvents()
			if err != nil || len(events) != 3 {
				t.Fatal("restart appended the terminal event twice")
			}
		})
	}
}
