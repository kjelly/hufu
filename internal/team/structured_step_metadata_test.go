package team

import (
	"bytes"
	"encoding/json/v2"
	"slices"
	"testing"
)

func TestStructuredStepMetadataMatchesCompleteReceipt(t *testing.T) {
	t.Parallel()
	c, events, _ := stepGenerationEvidence(t, true)
	rows, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, events)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ProjectStructuredStepMetadata(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(metadata) != 1 {
		t.Fatalf("rows=%d metadata=%d", len(rows), len(metadata))
	}
	full, err := json.Marshal(rows[0].Metadata())
	if err != nil {
		t.Fatal(err)
	}
	projected, err := json.Marshal(metadata[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, projected) {
		t.Fatalf("metadata differs: complete=%s projected=%s", full, projected)
	}
	if bytes.Contains(projected, []byte(`"stdout"`)) || bytes.Contains(projected, []byte(`"stderr"`)) {
		t.Fatal("metadata contains raw diagnostics")
	}
	changed := slices.Clone(events)
	index := slices.IndexFunc(changed, func(event RunEvent) bool { return event.Type == string(EventStructuredStepReceiptRecorded) })
	payload, err := decodeStructuredStepReceiptEvent(changed[index])
	if err != nil {
		t.Fatal(err)
	}
	payload.Summary.ProposedModel = "substituted-proposal"
	changed[index].Payload, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadStructuredStepReceipts(t.Context(), c.session.Workspace, changed); err == nil {
		t.Fatal("summary substituted for immutable receipt")
	}
}

func TestStructuredStepProjectionRetainsOnlySelectedTaskOccurrence(t *testing.T) {
	t.Parallel()
	create := func(run string, retry bool) RunEvent {
		raw, err := json.Marshal(map[string]any{"id": "task", "reset_for_retry": retry})
		if err != nil {
			t.Fatal(err)
		}
		return RunEvent{ID: "create-" + run, RunID: run, TaskID: "task", Type: string(EventTaskCreated), Payload: raw}
	}
	old := RunEvent{ID: "old-receipt", RunID: "old", TaskID: "task", Type: string(EventStructuredStepReceiptRecorded)}
	current := RunEvent{ID: "current-receipt", RunID: "current", TaskID: "task", Type: string(EventStructuredStepReceiptRecorded)}
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh task", true: "retry"}[retry], func(t *testing.T) {
			events := []RunEvent{create("old", false), old, create("current", retry), current}
			selected, err := StructuredStepProjectionEvents(events, events)
			if err != nil {
				t.Fatal(err)
			}
			hasOld := slices.ContainsFunc(selected, func(event RunEvent) bool { return event.ID == old.ID })
			if hasOld != retry {
				t.Fatalf("old occurrence included=%t, retry=%t", hasOld, retry)
			}
		})
	}
}

func TestLegacyStructuredStepProjectionDoesNotInventExecution(t *testing.T) {
	t.Parallel()
	item := &TodoItem{ID: "old", Execution: ExecutionContract{Steps: []ExecutionStep{{ID: "declared", Tool: "inspector", Effect: ExecutionEffectRead}}},
		TypedResult: &TaskResult{Attempt: 1, ReceiptIDs: []string{"old-receipt"}}}
	before, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	rows := IncludeLegacyStructuredSteps(nil, []*TodoItem{item})
	if len(rows) != 1 || rows[0].ExecutionKind != "unknown" || rows[0].ExitCode != nil || len(rows[0].Invocations) != 0 || len(rows[0].ModelExecutionIDs) != 0 || rows[0].ProposedModel != "" {
		t.Fatalf("invented historical attribution: %+v", rows)
	}
	after, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("historical bytes changed during projection")
	}
}

func TestRunReportLoadsLegacySnapshotWithoutRewritingIt(t *testing.T) {
	t.Parallel()
	workspace, store := newReportStore(t)
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "legacy answer"})
	report, err := LoadRunReport(t.Context(), workspace, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	report.SchemaVersion, report.StructuredSteps = legacyRunReportVersion, nil
	body, err := marshalRunReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.store.DB().ExecContext(t.Context(), `UPDATE run_reports SET schema_version=?,snapshot=? WHERE source_event_id=?`, legacyRunReportVersion, body, finish.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRunReport(t.Context(), workspace, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SchemaVersion != legacyRunReportVersion || loaded.Result.Response != "legacy answer" {
		t.Fatalf("legacy snapshot=%+v", loaded)
	}
	var saved []byte
	if err := store.store.DB().QueryRowContext(t.Context(), `SELECT snapshot FROM run_reports WHERE source_event_id=?`, finish.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, saved) {
		t.Fatal("legacy snapshot was rewritten")
	}
}
