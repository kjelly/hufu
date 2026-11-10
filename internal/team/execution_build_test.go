package team

import (
	"bytes"
	"encoding/json/v2"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/buildinfo"
)

func TestRunBuildIdentityPersistsThroughReplayAndReports(t *testing.T) {
	dir := t.TempDir()
	store, err := NewEventStore(dir, "old-run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	builds := []*buildinfo.Info{
		{Version: "1.0", Revision: "aaaa", Modified: new(false), GoVersion: "go1.27.2", Source: "build_info"},
		{Version: "dev", Revision: "bbbb", Modified: new(true), GoVersion: "go1.27.2", Source: "runtime_git"},
		nil,
	}
	runIDs := []string{"old-run", "resumed-run", "legacy-run"}
	var terminals []RunEvent
	for i, runID := range runIDs {
		appendReportEvent(t, store, RunEvent{Type: "run_started", RunID: runID}, LifecycleEventPayload{Team: "test", HufuBuild: builds[i]})
		// A terminal payload cannot replace the identity recorded at run start.
		terminals = append(terminals, appendReportEvent(t, store, RunEvent{Type: "run_finished", RunID: runID}, LifecycleEventPayload{Outcome: RunOutcomeCompleted, HufuBuild: &buildinfo.Info{Version: "incorrect"}}))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewEventStore(dir, "reader", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	events, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
	projector := NewExecutionEventProjector()
	for _, event := range events {
		projector.Add(event)
	}
	for _, row := range projector.Rows() {
		for i, runID := range runIDs {
			if row.RunID == runID && !reflect.DeepEqual(row.HufuBuild, builds[i]) {
				t.Fatalf("telemetry run %s identity = %+v", runID, row.HufuBuild)
			}
		}
	}
	for i, terminal := range terminals {
		var scoped []RunEvent
		for _, event := range events {
			if event.RunID == runIDs[i] {
				scoped = append(scoped, event)
			}
		}
		result := ReduceToSessionData(scoped).RunResult
		if result == nil || !reflect.DeepEqual(result.HufuBuild, builds[i]) {
			t.Fatalf("run %s replay identity = %+v", runIDs[i], result)
		}
		report, err := LoadRunReport(t.Context(), dir, terminal.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(report.HufuBuild, builds[i]) || !reflect.DeepEqual(report.Result.HufuBuild, builds[i]) {
			t.Fatalf("run %s report identity = %+v", runIDs[i], report)
		}
	}
}

func TestTaskJournalCompactionPreservesProducerBuild(t *testing.T) {
	workspace := t.TempDir()
	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	original := &buildinfo.Info{Version: "older", Revision: "original", Source: "build_info"}
	if err := journal.append(journalRecord{HufuBuild: original, Op: "put", Agent: "worker", Desc: "cached task", Output: "done", TS: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compactTaskJournalIfNeeded(workspace, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err := ExportTaskJournal(t.Context(), workspace, &exported); err != nil {
		t.Fatal(err)
	}
	var record journalRecord
	if err := json.Unmarshal(bytes.TrimSpace(exported.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.HufuBuild, original) {
		t.Fatalf("compaction changed producer version: %+v", record.HufuBuild)
	}
}

func TestInvocationRecordsCurrentBuild(t *testing.T) {
	c := newTestCoordinator(t)
	_, finish, err := c.beginPublicInvocationExecutionRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != string(EventRunStarted) {
			continue
		}
		var payload LifecycleEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.HufuBuild == nil || !reflect.DeepEqual(*payload.HufuBuild, buildinfo.Current()) {
			t.Fatalf("started identity = %+v", payload.HufuBuild)
		}
		result := c.FinalizeRun(t.Context(), &RunResult{Outcome: RunOutcomeCompleted}, nil)
		if result.HufuBuild == nil || !reflect.DeepEqual(*result.HufuBuild, buildinfo.Current()) {
			t.Fatalf("terminal identity = %+v", result.HufuBuild)
		}
		return
	}
	t.Fatal("missing run_started")
}
