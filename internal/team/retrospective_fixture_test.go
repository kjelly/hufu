package team

import (
	"bytes"
	"testing"
)

// Fixed IDs and timestamps make this example reproducible in disposable SQLite
// stores. A complete historical snapshot is not proof of a worker's test claim.
func TestRetrospectiveSyntheticSnapshotProvenance(t *testing.T) {
	var first []byte
	for range 2 {
		workspace := t.TempDir()
		store, err := NewEventStore(workspace, "run-retro-fixture", "session-retro-fixture")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		store.SetInvocationID("inv-retro-fixture")
		records := []struct {
			id, kind, task string
			payload        any
		}{
			{"evt-retro-start", "run_started", "", map[string]any{"team": "demo"}},
			{"evt-retro-request", "user_message_added", "", SessionMessageEventPayload{Role: "user", Content: "Check the synthetic timeout fixture"}},
			{"evt-retro-task", "task_created", "1", map[string]any{"id": "1", "agent": "helper", "desc": "Observe the fixture", "status": "pending"}},
			{"evt-retro-done", "task_completed", "1", map[string]any{"id": "1", "agent": "helper", "status": "done", "typed_result": &TaskResult{TaskID: "1", Agent: "helper", Status: TaskResultStatusSuccess, Summary: "Worker claims all checks passed", Source: "submitted"}}},
			{"evt-retro-finish", "run_finished", "", map[string]any{"team": "demo", "outcome": "partial", "response": "Synthetic snapshot: verification receipt unavailable"}},
		}
		var terminal RunEvent
		for _, r := range records {
			terminal = appendReportEvent(t, store, RunEvent{ID: r.id, Type: r.kind, TaskID: r.task, Timestamp: "2026-10-16T00:00:00Z"}, r.payload)
		}
		report, err := LoadRunReport(t.Context(), workspace, terminal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if report.SourceEventID != "evt-retro-finish" || report.SourceHash != terminal.Hash || report.SourceHash != "fee2f15e0b0c7ea9d31cfb20e7845fa129cfea8299b3fbb616bbcd87521b89a9" || report.RunID != "run-retro-fixture" || report.BranchID != "main" || report.Team != "demo" || report.Result == nil || report.Result.Outcome != RunOutcomePartial || len(report.Tasks) != 1 {
			t.Fatalf("fixture provenance lost: %#v", report)
		}
		task := report.Tasks[0]
		if task.TypedResult == nil || task.TypedResult.Status != TaskResultStatusSuccess || task.ExecutionReceipt != nil || report.MetricsAvailable || report.StatsAvailable {
			t.Fatal("worker claim became verification or unavailable metrics became measured")
		}
		body, err := marshalRunReport(report)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = body
		} else if !bytes.Equal(first, body) {
			t.Fatal("fixed fixture changed across fresh workspaces")
		}
		t.Logf("source_event_id=%s source_hash=%s run=%s session=%s branch=%s task=%s", report.SourceEventID, report.SourceHash, report.RunID, report.SessionID, report.BranchID, task.ID)
	}
}
