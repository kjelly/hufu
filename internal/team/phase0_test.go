package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func TestPhase0EventStoreReplaysIdentityAndDedupState(t *testing.T) {
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

func TestPhase0OpenEventStoreInheritsRunAndSessionIdentity(t *testing.T) {
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

func TestPhase0ArtifactStoreAndManifestFailClosed(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "report.md")
	if err := os.WriteFile(source, []byte("report"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), PutArtifactRequest{Kind: "document", Path: "report.md", SourcePath: source, RunID: "run-1", TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := EvidenceManifest{RunID: "run-1", ArtifactRefs: []ArtifactRef{ref}, EvidenceResults: []EvidenceResult{{RequirementID: "report", Status: "passed"}}}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, logsDir, "artifacts", "data", ref.ID), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(context.Background(), store); err == nil {
		t.Fatal("tampered artifact unexpectedly verified")
	}
}

func TestPhase0BlockingAcceptanceModeParsing(t *testing.T) {
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

func TestPhase0ReplayRestoresRunAcceptanceAndArtifactState(t *testing.T) {
	artifact := ArtifactRef{ID: "sha256-report", Kind: "document", Path: "report.md", SHA256: "abc", ByteSize: 6}
	manifest := &EvidenceManifest{SchemaVersion: 1, RunID: "run-1", ArtifactRefs: []ArtifactRef{artifact}, Status: "accepted", ManifestHash: "manifest-hash"}
	acceptance := &AcceptanceResult{State: AcceptancePassed, Passed: true, Commands: []string{"test -f report.md"}}
	es := []RunEvent{
		{Type: "task_created", TaskID: "task-1", Payload: []byte(`{"id":"task-1","agent":"worker","desc":"write report","status":"pending","retries":1}`)},
		{Type: "artifact_created", TaskID: "task-1", Payload: mustJSON(t, map[string]interface{}{"artifact": artifact, "task_id": "task-1"})},
		{Type: "task_completed", TaskID: "task-1", Payload: mustJSON(t, map[string]interface{}{
			"id": "task-1", "status": "done", "retries": 1,
			"verify_result": &VerificationResult{ExitCode: 0},
			"typed_result":  &TaskResult{TaskID: "task-1", Artifacts: []ArtifactRef{artifact}},
		})},
		{Type: "run_finished", Payload: mustJSON(t, map[string]interface{}{
			"outcome": "completed", "goal_satisfied": true, "acceptance": acceptance,
			"evidence_manifest": manifest,
		})},
	}
	projected := ReduceToSessionData(es)
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

func TestPhase0FinalManifestPersistsForAdvisoryAndPartialRuns(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session:        &TeamSession{Workspace: dir},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-advisory",
	}
	done := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(done.ID, TaskDone, "done")
	done.VerifyResult = &VerificationResult{ExitCode: 0}
	manifest, err := c.buildEvidenceManifest(context.Background(), false)
	if err != nil || manifest.Status != "accepted" {
		t.Fatalf("advisory manifest = %#v, err=%v", manifest, err)
	}
	if _, err := os.Stat(filepath.Join(dir, logsDir, "evidence_manifest.json")); err != nil {
		t.Fatalf("final manifest was not persisted: %v", err)
	}

	failed := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed"}})[0]
	c.taskTracker.TodoList().UpdateStatus(failed.ID, TaskError, "failed")
	partial, err := c.buildEvidenceManifest(context.Background(), false)
	if err != nil || partial.Status != "failed" {
		t.Fatalf("partial manifest = %#v, err=%v", partial, err)
	}
}

func TestPhase0ManifestUsesSuccessfulExecutionTranscriptAsEvidence(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, logsDir, "task-output", "task-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"event":"tool_result","exit_code":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	exitCode := 0
	c := &Coordinator{
		session:        &TeamSession{Workspace: dir},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-transcript-evidence",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "transcript-backed task"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "success")
	item.TypedResult = &TaskResult{Status: "success", Summary: "completed"}
	item.ExecutionReceipt = &ExecutionReceipt{
		RunID: "run-transcript-evidence", TaskID: item.ID, Attempt: 1,
		ExitCode: &exitCode, TranscriptRef: transcript,
	}

	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatalf("transcript-backed task rejected: %v", err)
	}
	if manifest.Status != "accepted" || len(manifest.ArtifactRefs) != 1 {
		t.Fatalf("manifest = %#v, want accepted with one transcript artifact", manifest)
	}
	if len(manifest.EvidenceResults) != 1 || manifest.EvidenceResults[0].Status != "passed" {
		t.Fatalf("evidence results = %#v, want one passed task result", manifest.EvidenceResults)
	}
}

func TestPhase0ManifestBindsAcceptanceOutcome(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: dir, Config: agent.TeamConfig{Shell: "sh"}},
		taskTracker: NewTaskTracker(), executionRunID: "run-acceptance",
		acceptanceSpec: &AcceptanceSpec{Commands: []string{"false"}},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{ExitCode: 0}
	acceptance, err := c.runAcceptance(context.Background())
	if err == nil || acceptance.IsPassed() {
		t.Fatalf("expected failed acceptance, result=%#v err=%v", acceptance, err)
	}
	if err := c.finalizeEvidenceManifest(context.Background(), acceptance); err != nil {
		t.Fatal(err)
	}
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if manifest == nil || manifest.Status != "failed" {
		t.Fatalf("manifest status = %#v", manifest)
	}
	var found bool
	for _, result := range manifest.EvidenceResults {
		if result.RequirementID == "run:acceptance" {
			found = result.Status == "failed"
		}
	}
	if !found {
		t.Fatalf("manifest did not record failed acceptance: %#v", manifest.EvidenceResults)
	}
}

func TestPhase0NoProgressFinishPersistsFinalManifest(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: dir, Config: agent.TeamConfig{GoalMode: "exploratory"}},
		sessionData: NewSession(), taskTracker: NewTaskTracker(),
		lastRunResult: &RunResult{Outcome: RunOutcomePartial, GoalSatisfied: false},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{ExitCode: 0}
	c.metricsMu.Lock()
	c.noProgressStopTripped = true
	c.metricsMu.Unlock()
	tool := &finishTool{coordinator: c}
	if _, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"partial"}`}); err != nil {
		t.Fatal(err)
	}
	if c.LastRunResult().EvidenceManifest == nil {
		t.Fatal("no-progress finish omitted evidence manifest")
	}
	if _, err := os.Stat(filepath.Join(dir, logsDir, "evidence_manifest.json")); err != nil {
		t.Fatalf("no-progress final manifest missing: %v", err)
	}
}

func TestPhase0ArtifactSourceAndIDContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(inside, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"absolute outside":   outsideFile,
		"relative traversal": filepath.Join("..", filepath.Base(outside)+"", "outside.txt"),
	} {
		if _, err := store.Put(context.Background(), PutArtifactRequest{Path: name, SourcePath: source}); err == nil {
			t.Errorf("%s source unexpectedly accepted", name)
		}
	}
	symlink := filepath.Join(root, "link.txt")
	if err := os.Symlink(outsideFile, symlink); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}
	if _, err := store.Put(context.Background(), PutArtifactRequest{Path: "link.txt", SourcePath: symlink}); err == nil {
		t.Error("symlink escaping source root unexpectedly accepted")
	}
	if _, err := store.Put(context.Background(), PutArtifactRequest{ID: "../escape", Path: "inside.txt", SourcePath: inside}); err == nil {
		t.Error("path-traversal artifact id unexpectedly accepted")
	}
	if _, err := store.Open(context.Background(), "../escape"); err == nil {
		t.Error("path-traversal Open id unexpectedly accepted")
	}
}

func TestPhase0ArtifactEventAppendFailureIsRetryable(t *testing.T) {
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

func TestPhase0RunFinishedAlwaysCarriesManifestForTerminalResult(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: dir, Config: agent.TeamConfig{GoalMode: "exploratory"}},
		sessionData: NewSession(), taskTracker: NewTaskTracker(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{ExitCode: 0}
	end := c.beginExecutionRun()
	c.SetLastRunResult(&RunResult{Outcome: RunOutcomePartial, Stats: SummarizeRunStats(c.taskTracker.TodoList().Items())})
	end()
	es, err := OpenEventStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type != "run_finished" {
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		found = len(payload["evidence_manifest"]) > 0
	}
	if !found {
		t.Fatal("run_finished omitted evidence_manifest for terminal result")
	}
}

func TestPhase0NewRunCannotReusePreviousCompletedOutcome(t *testing.T) {
	dir := t.TempDir()
	old := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true}
	c := &Coordinator{
		session: &TeamSession{Workspace: dir}, sessionData: &SessionData{RunResult: old},
		lastRunResult: old, taskTracker: NewTaskTracker(),
	}
	end := c.beginExecutionRun()
	defer end()
	if c.LastRunResult() != nil || c.sessionData.RunResult != nil {
		t.Fatal("new execution reused previous completed result")
	}
}

func mustJSON(t *testing.T, value interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
