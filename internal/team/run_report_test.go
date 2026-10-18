package team

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendReportEvent(t *testing.T, store *EventStore, event RunEvent, payload any) RunEvent {
	t.Helper()
	var err error
	event.Payload, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if event.Timestamp == "" {
		event.Timestamp = "2026-10-15T01:00:00Z"
	}
	if event.Actor == "" {
		event.Actor = "coordinator"
	}
	durable, err := store.AppendPersistedContext(t.Context(), event)
	if err != nil {
		t.Fatal(err)
	}
	return durable
}

func newReportStore(t *testing.T) (string, *EventStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewEventStore(dir, "run-a", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetInvocationID("inv-a")
	appendReportEvent(t, store, RunEvent{Type: "run_started"}, map[string]any{"team": "demo"})
	return dir, store
}

func TestRunReportUsesFollowUpCycleRequestAndFreshTasks(t *testing.T) {
	dir, store := newReportStore(t)
	appendReportEvent(t, store, RunEvent{Type: "task_completed", TaskID: "1"}, map[string]any{"id": "1", "status": "done", "output": "previous result"})
	appendReportEvent(t, store, RunEvent{Type: string(EventWorkflowStateChanged)}, WorkflowStateChangedPayload{State: PhaseDone, PhaseResults: map[Phase]PhaseResult{PhaseVerify: {Status: PhaseStatusSuccess}}})
	appendReportEvent(t, store, RunEvent{Type: "run_started", RunID: "run-followup"}, map[string]any{"team": "demo"})
	appendReportEvent(t, store, RunEvent{Type: string(EventWorkflowCycleStarted), RunID: "run-followup"}, WorkflowCycleStartedPayload{TaskIDFloor: 1, Request: "follow-up investigation"})
	appendReportEvent(t, store, RunEvent{Type: "task_created", RunID: "run-followup", TaskID: "2"}, map[string]any{"id": "2", "status": "pending", "desc": "fresh task"})
	appendReportEvent(t, store, RunEvent{Type: "task_completed", RunID: "run-followup", TaskID: "2"}, map[string]any{"id": "2", "status": "done", "output": "fresh result"})
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished", RunID: "run-followup"}, map[string]any{"outcome": "completed", "response": "follow-up answer"})
	report, err := LoadRunReport(t.Context(), dir, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.OriginalPrompt != "follow-up investigation" || len(report.Tasks) != 1 || report.Tasks[0].ID != "2" || report.Tasks[0].Output != "fresh result" {
		t.Fatalf("report mixed cycle evidence: %#v", report)
	}
}

func TestRunReportCapturedWithoutExportAndRemainsRunBound(t *testing.T) {
	dir, store := newReportStore(t)
	appendReportEvent(t, store, RunEvent{Type: "user_message_added"}, SessionMessageEventPayload{Role: "user", Content: "調查本次任務"})
	appendReportEvent(t, store, RunEvent{Type: "task_created", TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "agent": "helper", "desc": "原任務"})
	appendReportEvent(t, store, RunEvent{Type: "task_completed", TaskID: "1"}, map[string]any{"id": "1", "status": "done", "output": "原結果"})
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished", IdempotencyKey: "finish-a", Timestamp: "2026-10-15T01:00:05Z"}, map[string]any{"outcome": "completed", "response": "本次回覆"})
	report, err := LoadRunReport(t.Context(), dir, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Team != "demo" || report.RunID != "run-a" || report.InvocationID != "inv-a" || report.SourceHash != finish.Hash || report.OriginalPrompt != "調查本次任務" || len(report.Tasks) != 1 || report.Tasks[0].Output != "原結果" || report.CompletedAt.Sub(report.StartedAt) != 5*time.Second {
		t.Fatalf("incorrect snapshot: %+v", report)
	}
	original, err := marshalRunReport(report)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse a task ID on a later run and write contradictory loose projections.
	appendReportEvent(t, store, RunEvent{Type: "run_started", RunID: "run-b"}, map[string]any{"team": "other"})
	appendReportEvent(t, store, RunEvent{Type: "task_created", RunID: "run-b", TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "desc": "OTHER_RUN_SENTINEL"})
	appendReportEvent(t, store, RunEvent{Type: "run_finished", RunID: "run-b"}, map[string]any{"outcome": "completed", "response": "OTHER_RUN_SENTINEL"})
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("OTHER_RUN_SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	retry, err := store.AppendPersistedContext(t.Context(), RunEvent{Type: "run_finished", IdempotencyKey: "finish-a", Payload: []byte(`{"outcome":"completed","response":"replacement"}`)})
	if err != nil || retry.ID != finish.ID {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	report, err = LoadRunReport(t.Context(), dir, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := marshalRunReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(actual) || strings.Contains(string(actual), "OTHER_RUN_SENTINEL") {
		t.Fatalf("historical report changed: %s", actual)
	}
	var count int
	if err := store.store.DB().QueryRow(`SELECT count(*) FROM run_reports WHERE source_event_id=?`, finish.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate capture: %d, %v", count, err)
	}
}

func TestRunReportResumeIncludesOnlyTouchedInheritedTasks(t *testing.T) {
	dir, store := newReportStore(t)
	for _, id := range []string{"1", "2"} {
		appendReportEvent(t, store, RunEvent{Type: "task_created", TaskID: id}, map[string]any{"id": id, "status": "pending", "desc": "contract " + id})
	}
	appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "partial", "response": "interrupted"})
	appendReportEvent(t, store, RunEvent{Type: "run_started", RunID: "run-resume"}, map[string]any{"team": "demo"})
	appendReportEvent(t, store, RunEvent{Type: "task_completed", RunID: "run-resume", TaskID: "1"}, map[string]any{"id": "1", "status": "done", "output": "resumed"})
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished", RunID: "run-resume"}, map[string]any{"outcome": "completed", "response": "resumed final"})
	report, err := LoadRunReport(t.Context(), dir, finish.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Tasks) != 1 || report.Tasks[0].Desc != "contract 1" || report.Tasks[0].Output != "resumed" {
		t.Fatalf("wrong resume tasks: %+v", report.Tasks)
	}
}

func TestRunReportForkVisibilityAndSourceRemainStable(t *testing.T) {
	dir, store := newReportStore(t)
	before := appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "parent final"})
	tree := NewSessionTree()
	child, err := tree.CreateBranch("child", before.ID, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(dir, tree); err != nil {
		t.Fatal(err)
	}
	appendReportEvent(t, store, RunEvent{Type: "run_started", BranchID: child.ID, RunID: "run-child"}, map[string]any{"team": "demo"})
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished", BranchID: child.ID, RunID: "run-child"}, map[string]any{"outcome": "completed", "response": "child final"})
	tree.ActiveBranch = "main"
	if err := SaveSessionTree(dir, tree); err != nil {
		t.Fatal(err)
	}
	for _, event := range []RunEvent{before, finish} {
		report, err := LoadRunReport(t.Context(), dir, event.ID)
		if err != nil {
			t.Fatal(err)
		}
		if report.SourceEventID != event.ID || report.BranchID != effectiveBranchID(event.BranchID) {
			t.Fatalf("wrong source: %+v", report)
		}
	}
}

func TestRunReportAtomicFailureAndUnknownCommit(t *testing.T) {
	dir, store := newReportStore(t)
	if _, err := store.store.DB().Exec(`CREATE TRIGGER reject_report BEFORE INSERT ON run_reports BEGIN SELECT RAISE(ABORT, 'injected'); END;`); err != nil {
		t.Fatal(err)
	}
	_, err := store.AppendPersistedContext(t.Context(), RunEvent{Type: "run_finished", Actor: "coordinator", IdempotencyKey: "finish", Payload: []byte(`{"outcome":"completed","response":"final"}`)})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("expected report storage failure, got %v", err)
	}
	var count int
	if err := store.store.DB().QueryRow(`SELECT count(*) FROM events WHERE type='run_finished'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("terminal event committed without report: %d %v", count, err)
	}
	if _, err := store.store.DB().Exec(`DROP TRIGGER reject_report`); err != nil {
		t.Fatal(err)
	}
	// Reload after the failed transaction, then inject an unknown acknowledgement.
	if err := store.reload(); err != nil {
		t.Fatal(err)
	}
	store.commitHook = func() error { return errors.New("lost acknowledgement") }
	_, err = store.AppendPersistedContext(t.Context(), RunEvent{Type: "run_finished", Actor: "coordinator", IdempotencyKey: "finish", Payload: []byte(`{"outcome":"completed","response":"final"}`)})
	if err == nil || !strings.Contains(err.Error(), "lost acknowledgement") {
		t.Fatalf("expected unknown commit, got %v", err)
	}
	finish, err := store.AppendPersistedContext(t.Context(), RunEvent{Type: "run_finished", Actor: "coordinator", IdempotencyKey: "finish", Payload: []byte(`{"outcome":"completed","response":"final"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunReport(t.Context(), dir, finish.ID); err != nil {
		t.Fatalf("committed report lost: %v", err)
	}
}

func TestRunReportReadOnlyMissingAndTampered(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := LoadRunReport(t.Context(), dir, "evt-missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("created missing workspace: %v", err)
	}
	dir, store := newReportStore(t)
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "final"})
	if _, err := store.store.DB().Exec(`UPDATE run_reports SET snapshot=replace(CAST(snapshot AS TEXT),'final','forged') WHERE source_event_id=?`, finish.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunReport(t.Context(), dir, finish.ID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("accepted altered report: %v", err)
	}
	if _, err := store.store.DB().Exec(`DELETE FROM run_reports WHERE source_event_id=?`, finish.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunReport(t.Context(), dir, finish.ID); !errors.Is(err, ErrRunReportUnavailable) {
		t.Fatalf("silently rebuilt missing report: %v", err)
	}
	if _, err := store.store.DB().Exec(`DROP TABLE run_reports`); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunReport(t.Context(), dir, finish.ID); !errors.Is(err, ErrRunReportUnavailable) {
		t.Fatalf("old schema: %v", err)
	}
}

func TestRunReportPreCaptureSchemaKeepsHistoryReadable(t *testing.T) {
	dir, store := newReportStore(t)
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "historical response"})
	// Reproduce a workspace created before report capture, including its
	// actual migration registry rather than only removing the optional table.
	if _, err := store.store.DB().Exec(`DROP TABLE run_reports; DELETE FROM schema_migrations WHERE version=4`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runtime.sqlite")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenEventStoreReadOnly(dir)
	if err != nil {
		t.Fatalf("pre-capture history must remain readable: %v", err)
	}
	events, err := reader.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if result := ReduceToSessionData(events).RunResult; result == nil || result.Response != "historical response" {
		t.Fatalf("lost historical response: %+v", result)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunReport(t.Context(), dir, finish.ID); !errors.Is(err, ErrRunReportUnavailable) {
		t.Fatalf("pre-capture report must be explicitly unavailable: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("historical reads migrated or changed the source database")
	}
}

func TestRunReportCapturesOnlyCanonicalRedactedBytes(t *testing.T) {
	_, store := newReportStore(t)
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "api_key=report-super-secret-token"})
	var body []byte
	if err := store.store.DB().QueryRow(`SELECT snapshot FROM run_reports WHERE source_event_id=?`, finish.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "report-super-secret-token") {
		t.Fatal("unredacted persisted snapshot")
	}
}

func TestRunReportFreshTaskContractsDoNotInheritReusedIDs(t *testing.T) {
	dir, store := newReportStore(t)
	appendReportEvent(t, store, RunEvent{Type: "task_created", TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "agent": "old-helper", "desc": "OLD_CONTRACT_SENTINEL"})
	appendReportEvent(t, store, RunEvent{Type: "task_completed", TaskID: "1"}, map[string]any{"id": "1", "status": "done", "output": "OLD_OUTPUT_SENTINEL"})
	appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": "old final"})
	for _, teamName := range []string{"demo", "other"} {
		run := "run-new-" + teamName
		appendReportEvent(t, store, RunEvent{Type: "run_started", RunID: run}, map[string]any{"team": teamName})
		appendReportEvent(t, store, RunEvent{Type: "task_created", RunID: run, TaskID: "1"}, map[string]any{"id": "1", "status": "pending", "agent": "new-helper", "desc": "NEW_CONTRACT"})
		appendReportEvent(t, store, RunEvent{Type: "task_completed", RunID: run, TaskID: "1"}, map[string]any{"id": "1", "status": "done", "output": "NEW_OUTPUT"})
		finish := appendReportEvent(t, store, RunEvent{Type: "run_finished", RunID: run}, map[string]any{"outcome": "completed", "response": "new final"})
		report, err := LoadRunReport(t.Context(), dir, finish.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Tasks) != 1 || report.Tasks[0].Desc != "NEW_CONTRACT" || report.Tasks[0].Agent != "new-helper" || report.Tasks[0].Output != "NEW_OUTPUT" {
			t.Fatalf("inherited old contract: %+v", report.Tasks)
		}
	}
}

func TestRunReportOversizeIsExplicitUnavailableWithoutChangingOutcome(t *testing.T) {
	dir, store := newReportStore(t)
	finish := appendReportEvent(t, store, RunEvent{Type: "run_finished"}, map[string]any{"outcome": "completed", "response": strings.Repeat("x", maxRunReportBytes)})
	if _, err := LoadRunReport(t.Context(), dir, finish.ID); !errors.Is(err, ErrRunReportUnavailable) {
		t.Fatalf("oversize report should be unavailable: %v", err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	result := ReduceToSessionData(events).RunResult
	if result == nil || result.Outcome != RunOutcomeCompleted {
		t.Fatal("report projection changed canonical outcome")
	}
}
