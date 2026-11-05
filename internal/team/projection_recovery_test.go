package team

import (
	"encoding/json/v2"
	"testing"
)

func TestInitEventStoreClearsOnlyDurablyResolvedProjectionMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, reason                string
		recordMismatch, corruptTask bool
		wantRecovery                bool
	}{
		{"matched recorded mismatch", "event-store projection mismatch: old reducer", true, false, false},
		{"unrecorded mismatch", "event-store projection mismatch: old reducer", false, false, true},
		{"unrelated recovery", "cost ledger replay failed", true, false, true},
		{"actual mismatch", "event-store projection mismatch: old reducer", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.initEventStore()
			item := &TodoItem{ID: "1", Agent: "worker", Desc: "completed work", Status: TaskDone}
			payload, err := taskTransitionPayloadJSON(item)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.eventStore.AppendPersisted(RunEvent{Type: "task_completed", Actor: "worker", TaskID: item.ID, Payload: payload}); err != nil {
				t.Fatal(err)
			}
			if tc.recordMismatch {
				body, err := json.Marshal(map[string]string{"reason": tc.reason})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.eventStore.AppendPersisted(RunEvent{Type: "projection_mismatch", Actor: "coordinator", Payload: body}); err != nil {
					t.Fatal(err)
				}
			}
			sd := NewSession()
			sd.Tasks = []*TodoItem{cloneTodoItem(item)}
			if tc.corruptTask {
				sd.Tasks[0].Desc = "different work"
			}
			sd.RecoveryRequired, sd.RecoveryReason = true, tc.reason
			if err := SaveSession(c.session.Workspace, sd); err != nil {
				t.Fatal(err)
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := newTestCoordinator(t)
			reopened.session.Workspace = c.session.Workspace
			reopened.sessionData = LoadSession(c.session.Workspace)
			reopened.initEventStore()
			if reopened.sessionData.RecoveryRequired != tc.wantRecovery {
				t.Fatalf("recovery=%v reason=%s", reopened.sessionData.RecoveryRequired, reopened.sessionData.RecoveryReason)
			}
			if saved := LoadSession(c.session.Workspace); saved == nil || saved.RecoveryRequired != tc.wantRecovery {
				t.Fatal("reconciled recovery state was not persisted")
			}
		})
	}
}
