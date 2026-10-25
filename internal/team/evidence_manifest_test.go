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

// The evidence manifest binds every terminal run result to the artifacts
// and transcripts that justify it; it is sealed, persisted and verified
// against the content-addressed artifact store.

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeTaskTranscript stores a transcript under the workspace task-output
// log directory and returns its path.
func writeTaskTranscript(t *testing.T, workspace, name, body string) string {
	t.Helper()
	transcript := filepath.Join(workspace, logsDir, "task-output", name)
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return transcript
}

// evidenceCoordinator builds a coordinator for the given run with one
// completed, verified task that carries a successful execution receipt.
func evidenceCoordinator(t *testing.T, runID string) *Coordinator {
	t.Helper()
	c := newTestCoordinator(t)
	c.executionRunID = runID
	return c
}

func TestArtifactStoreAndManifestFailClosedOnTamper(t *testing.T) {
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
	manifest := EvidenceManifest{RunID: "run-1", ArtifactRefs: []ArtifactRef{ref.ArtifactRef}, EvidenceResults: []EvidenceResult{{RequirementID: "report", Status: "passed"}}}
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

func TestArtifactStoreRejectsSourcesAndIDsOutsideRoot(t *testing.T) {
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
		"relative traversal": filepath.Join("..", filepath.Base(outside), "outside.txt"),
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

func TestEvidenceManifestFromExecutionReceipts(t *testing.T) {
	const runID = "run-evidence"
	tests := []struct {
		name string
		// receipt returns the receipt for the task, given its ID and the
		// transcript path written under the workspace.
		receipt func(taskID, transcript string) *ExecutionReceipt
		status  TaskStatus
		strict  bool
		wantErr bool
		// wantStatus is the manifest status and wantResult the status of
		// its single evidence result.
		wantStatus string
		wantResult string
	}{
		{
			name: "successful transcript backs an accepted manifest",
			receipt: func(taskID, transcript string) *ExecutionReceipt {
				exit := 0
				return &ExecutionReceipt{RunID: runID, TaskID: taskID, Attempt: 1, ExitCode: &exit, TranscriptRef: transcript, ProducerID: "worker", ModelExecutionID: "model-execution-1"}
			},
			status: TaskDone, strict: true, wantStatus: "accepted", wantResult: "passed",
		},
		{
			name: "failed task keeps its transcript as failed evidence",
			receipt: func(taskID, transcript string) *ExecutionReceipt {
				exit := 1
				return &ExecutionReceipt{RunID: runID, TaskID: taskID, Attempt: 1, ExitCode: &exit, TranscriptRef: transcript}
			},
			status: TaskError, wantStatus: "failed", wantResult: "failed",
		},
		{
			name: "receipt from another run is rejected",
			receipt: func(taskID, transcript string) *ExecutionReceipt {
				exit := 0
				return &ExecutionReceipt{RunID: "run-old", TaskID: taskID, Attempt: 1, ExitCode: &exit, TranscriptRef: transcript}
			},
			status: TaskDone, strict: true, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := evidenceCoordinator(t, runID)
			transcript := writeTaskTranscript(t, c.session.Workspace, "task-1.jsonl", `{"event":"tool_result","exit_code":0}`)
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "transcript-backed task"}})[0]
			c.taskTracker.TodoList().UpdateStatus(item.ID, tt.status, string(tt.status))
			if tt.status == TaskDone {
				item.TypedResult = &TaskResult{Status: "success", Summary: "completed"}
			}
			item.ExecutionReceipt = tt.receipt(item.ID, transcript)
			item.ExecutionReceipts = []ExecutionReceipt{*item.ExecutionReceipt}

			manifest, err := c.buildEvidenceManifest(context.Background(), tt.strict)
			if tt.wantErr {
				if err == nil {
					t.Fatal("manifest accepted the receipt")
				}
				return
			}
			if err != nil {
				t.Fatalf("buildEvidenceManifest: %v", err)
			}
			if manifest.Status != tt.wantStatus || len(manifest.ArtifactRefs) != 1 || len(manifest.EvidenceResults) != 1 {
				t.Fatalf("manifest = %#v, want %s with one artifact and one result", manifest, tt.wantStatus)
			}
			result := manifest.EvidenceResults[0]
			if result.Status != tt.wantResult || len(result.ArtifactRefs) != 1 || result.ArtifactRefs[0].TaskID != item.ID {
				t.Fatalf("evidence result = %#v, want %s bound to task %s", result, tt.wantResult, item.ID)
			}
		})
	}
}

func TestEvidenceManifestResolvesOpaqueTranscriptArtifact(t *testing.T) {
	const runID = "run-opaque-transcript"
	c := evidenceCoordinator(t, runID)
	dir := c.session.Workspace
	transcript := writeTaskTranscript(t, dir, "task-1.jsonl", `{"event":"tool_result","exit_code":0}`)
	store, err := NewFileArtifactStore(dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(context.Background(), PutArtifactRequest{
		Kind: "task_transcript", Path: transcript, SourcePath: transcript,
		RunID: runID, TaskID: "1", Attempt: 1, Agent: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "opaque transcript task"}})[0]
	if item.ID != "1" {
		t.Fatalf("task ID = %q, want 1", item.ID)
	}
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "success")
	item.TypedResult = &TaskResult{Status: "success", Summary: "completed"}
	exitCode := 0
	item.ExecutionReceipt = &ExecutionReceipt{RunID: runID, TaskID: item.ID, Attempt: 1, ExitCode: &exitCode, TranscriptRef: ref.ID, ProducerID: "worker", ModelExecutionID: "model-execution-1"}

	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatalf("opaque transcript-backed task rejected: %v", err)
	}
	if manifest.Status != "accepted" || len(manifest.ArtifactRefs) != 1 || manifest.ArtifactRefs[0].ID != ref.ID {
		t.Fatalf("manifest = %#v, want existing opaque transcript artifact", manifest)
	}
	got := manifest.ArtifactRefs[0]
	if got.RunID != ref.RunID || got.TaskID != ref.TaskID || got.Attempt != ref.Attempt || got.Agent != ref.Agent {
		t.Fatalf("opaque artifact metadata changed: got=%#v want=%#v", got, ref)
	}
}

func TestFinalEvidenceManifestPersistsForAdvisoryAndPartialRuns(t *testing.T) {
	c := evidenceCoordinator(t, "run-advisory")
	dir := c.session.Workspace
	done := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(done.ID, TaskDone, "done")
	done.VerifyResult = &VerificationResult{ExitCode: 0}
	transcript := writeTaskTranscript(t, dir, "verified-task.jsonl", `{"event":"tool_result","exit_code":0}`)
	exitCode := 0
	done.ExecutionReceipt = &ExecutionReceipt{RunID: c.executionRunID, TaskID: done.ID, Attempt: 1, ExitCode: &exitCode, ProducerID: "worker", ModelExecutionID: "model-execution-1", TranscriptRef: transcript}
	done.ExecutionReceipts = []ExecutionReceipt{*done.ExecutionReceipt}
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

func TestEvidenceManifestBindsAcceptanceOutcome(t *testing.T) {
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) { cfg.Shell = "sh" }))
	c.executionRunID = "run-acceptance"
	c.acceptanceSpec = &AcceptanceSpec{Commands: []string{"false"}}
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

func TestNoProgressFinishPersistsFinalManifest(t *testing.T) {
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) { cfg.GoalMode = "exploratory" }))
	dir := c.session.Workspace
	c.sessionData = NewSession()
	c.lastRunResult = &RunResult{Outcome: RunOutcomePartial, GoalSatisfied: false}
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

func TestRunFinishedAlwaysCarriesManifestForTerminalResult(t *testing.T) {
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) { cfg.GoalMode = "exploratory" }))
	dir := c.session.Workspace
	c.sessionData = NewSession()
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
