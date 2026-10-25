package team

import (
	"os"
	"path/filepath"
	"testing"
)

// Run state is reconstructed from the event store: task transitions are
// deduplicated across coordinators, the store keeps its run identity across
// reopen, and a terminal run result replays with its acceptance and
// artifacts.

func TestEventStoreReplayDeduplicatesTaskTransitions(t *testing.T) {
	dir := t.TempDir()
	first := &Coordinator{session: &TeamSession{Workspace: dir}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	first.initEventStore()
	item := first.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "persisted"}})[0]
	first.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	first.saveCheckpoint()
	if err := first.eventStore.Close(); err != nil {
		t.Fatal(err)
	}

	second := &Coordinator{session: &TeamSession{Workspace: dir}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	second.initEventStore()
	defer second.eventStore.Close()
	item2 := second.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "persisted"}})[0]
	item2.ID = item.ID
	second.taskTracker.TodoList().UpdateStatus(item2.ID, TaskDone, "done")
	second.saveCheckpoint()
	events, err := second.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == "task_completed" && event.TaskID == item.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("replayed completed transition count = %d, want 1", count)
	}
}

func TestOpenEventStoreInheritsRunAndSessionIdentity(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-inherited", "session-inherited")
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Append(RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	_ = es.Close()
	opened, err := OpenEventStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := opened.Append(RunEvent{Type: "task_created", Actor: "coordinator", Payload: []byte(`{"task":"t"}`)}); err != nil {
		t.Fatal(err)
	}
	events, err := opened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1]; got.RunID != "run-inherited" || got.SessionID != "session-inherited" {
		t.Fatalf("inherited identity = %s/%s", got.RunID, got.SessionID)
	}
}

func TestReplayRestoresRunAcceptanceAndArtifactState(t *testing.T) {
	artifact := ArtifactRef{ID: "sha256-report", Kind: "document", Path: "report.md", SHA256: "abc", ByteSize: 6}
	manifest := &EvidenceManifest{SchemaVersion: 1, RunID: "run-1", ArtifactRefs: []ArtifactRef{artifact}, Status: "accepted", ManifestHash: "manifest-hash"}
	acceptance := &AcceptanceResult{State: AcceptancePassed, Passed: true, Commands: []string{"test -f report.md"}}
	events := []RunEvent{
		{Type: "task_created", TaskID: "task-1", Payload: []byte(`{"id":"task-1","agent":"worker","desc":"write report","status":"pending","retries":1}`)},
		{Type: "artifact_created", TaskID: "task-1", Payload: mustJSON(t, map[string]any{"artifact": artifact, "task_id": "task-1"})},
		{Type: "task_completed", TaskID: "task-1", Payload: mustJSON(t, map[string]any{
			"id": "task-1", "status": "done", "retries": 1,
			"verify_result": &VerificationResult{ExitCode: 0},
			"typed_result":  &TaskResult{TaskID: "task-1", Artifacts: []ArtifactRef{artifact}},
		})},
		{Type: "run_finished", Payload: mustJSON(t, map[string]any{
			"outcome": "completed", "goal_satisfied": true, "acceptance": acceptance,
			"evidence_manifest": manifest,
		})},
	}
	projected := ReduceToSessionData(events)
	if projected.RunResult == nil || projected.RunResult.Acceptance == nil || !projected.RunResult.Acceptance.IsPassed() {
		t.Fatalf("replayed run result acceptance = %#v", projected.RunResult)
	}
	if projected.RunResult.EvidenceManifest == nil || projected.RunResult.EvidenceManifest.ManifestHash != "manifest-hash" {
		t.Fatalf("replayed evidence manifest = %#v", projected.RunResult.EvidenceManifest)
	}
	if len(projected.Tasks) != 1 || projected.Tasks[0].Retries != 1 || projected.Tasks[0].VerifyResult == nil || projected.Tasks[0].TypedResult == nil || len(projected.Tasks[0].TypedResult.Artifacts) != 1 {
		t.Fatalf("replayed task state = %#v", projected.Tasks)
	}
}

func TestArtifactEventAppendFailureIsRetryable(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	item := &TodoItem{ID: "task-1", Agent: "worker", Status: TaskPending, TypedResult: &TaskResult{Artifacts: []ArtifactRef{{Path: "report.md"}}}}
	key := taskTransitionEventKey(item)
	c := &Coordinator{eventStore: es, emittedTaskTransitions: map[string]bool{key: true}}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}
	c.emitTaskEventsFromCheckpoint([]*TodoItem{item})
	if c.emittedTaskTransitions["artifact:task-1:report.md"] {
		t.Fatal("failed artifact append was marked emitted")
	}
	reopened, err := OpenEventStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = reopened
	defer reopened.Close()
	c.emitTaskEventsFromCheckpoint([]*TodoItem{item})
	events, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == "artifact_created" {
			count++
		}
	}
	if count != 1 || !c.emittedTaskTransitions["artifact:task-1:report.md"] {
		t.Fatalf("retry append count=%d emitted=%v", count, c.emittedTaskTransitions["artifact:task-1:report.md"])
	}
}

func TestNewRunCannotReusePreviousCompletedOutcome(t *testing.T) {
	old := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true}
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir()}, sessionData: &SessionData{RunResult: old},
		lastRunResult: old, taskTracker: NewTaskTracker(),
	}
	end := c.beginExecutionRun()
	defer end()
	if c.LastRunResult() != nil || c.sessionData.RunResult != nil {
		t.Fatal("new execution reused previous completed result")
	}
}

func TestBlockingAcceptanceModeParsing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: strict\nacceptance:\n  mode: blocking\n  commands: [true]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AcceptanceMode != string(AcceptanceBlocking) {
		t.Fatalf("acceptance mode = %q", cfg.AcceptanceMode)
	}
	if cfg.GoalMode != "" {
		t.Fatalf("blocking acceptance changed goal mode to %q", cfg.GoalMode)
	}
}
