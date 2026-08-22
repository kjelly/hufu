package team

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

type bindingTestStore struct{}

func (bindingTestStore) Put(context.Context, PutArtifactRequest) (ArtifactPutResult, error) {
	return ArtifactPutResult{}, nil
}
func (bindingTestStore) Verify(context.Context, ArtifactRef) error                 { return nil }
func (bindingTestStore) Open(context.Context, string) (io.ReadCloser, error)       { return nil, nil }
func (bindingTestStore) ListByTask(context.Context, string) ([]ArtifactRef, error) { return nil, nil }

func TestEvidenceManifestRejectsConflictingTaskAttemptAndTranscriptBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*EvidenceBinding)
	}{
		{name: "task", mutate: func(b *EvidenceBinding) { b.TaskID = "other" }},
		{name: "attempt", mutate: func(b *EvidenceBinding) { b.Attempt = 2 }},
		{name: "transcript", mutate: func(b *EvidenceBinding) { b.TranscriptRef = "sha256-other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := ArtifactRef{ID: "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RunID: "run-1", TaskID: "task-1", Attempt: 1}
			manifest := &EvidenceManifest{RunID: "run-1", Status: "accepted", ArtifactRefs: []ArtifactRef{artifact}, EvidenceResults: []EvidenceResult{{
				RequirementID: "task:task-1", Status: "passed", ArtifactRefs: []ArtifactRef{artifact}, Binding: &EvidenceBinding{
					RunID: "run-1", TaskID: "task-1", Attempt: 1, ModelExecutionID: "exec-1", ProducerID: "worker", TranscriptRef: artifact.ID, ArtifactIDs: []string{artifact.ID},
				},
			}}}
			tc.mutate(manifest.EvidenceResults[0].Binding)
			if err := manifest.Seal(); err != nil {
				t.Fatal(err)
			}
			if err := manifest.Verify(context.Background(), bindingTestStore{}); err == nil {
				t.Fatal("conflicting binding was accepted")
			}
		})
	}
}

func TestEvidenceManifestVerifyCommandWithoutTranscriptIsNotVerified(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}, taskTracker: NewTaskTracker(), executionRunID: "run-verify"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify-only"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{Command: "true", ExitCode: 0}
	_, err := c.buildEvidenceManifest(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "execution binding") {
		t.Fatalf("verify-only task without transcript err = %v, want binding failure", err)
	}
}

func TestEvidenceManifestStrictFalseBindingCorruptionIsTerminalFailure(t *testing.T) {
	workspace := t.TempDir()
	transcript := workspace + "/transcript.md"
	if err := os.WriteFile(transcript, []byte("worker transcript\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "run-binding"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "binding"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{Command: "true", ExitCode: 0}
	item.ExecutionReceipts = []ExecutionReceipt{{RunID: "run-binding", TaskID: item.ID, Attempt: 1, ModelExecutionID: "exec-1", TranscriptRef: transcript, ExitCode: evidenceIntPtr(0)}}
	item.ExecutionReceipts[0].ProducerID = ""

	manifest, err := c.buildEvidenceManifest(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "failed" || len(manifest.EvidenceResults) != 1 || manifest.EvidenceResults[0].Status != "failed" {
		t.Fatalf("binding corruption manifest = %#v, want terminal failed evidence", manifest)
	}
	if manifest.EvidenceResults[0].Status == "unverified" {
		t.Fatal("binding corruption was masked as unverified")
	}
}

func TestEvidenceManifestIncludesVerifiedTranscriptInTaskArtifactRefs(t *testing.T) {
	workspace := t.TempDir()
	transcript := workspace + "/transcript.md"
	if err := os.WriteFile(transcript, []byte("worker transcript\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "run-transcript"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verified"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{Command: "true", ExitCode: 0}
	item.ExecutionReceipts = []ExecutionReceipt{{RunID: "run-transcript", TaskID: item.ID, Attempt: 1, ModelExecutionID: "exec-1", ProducerID: "worker", TranscriptRef: transcript, ExitCode: evidenceIntPtr(0)}}

	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != "accepted" || len(manifest.EvidenceResults) != 1 {
		t.Fatalf("manifest = %#v, want accepted single task evidence", manifest)
	}
	result := manifest.EvidenceResults[0]
	if len(result.ArtifactRefs) != 1 || result.Binding == nil || len(result.Binding.ArtifactIDs) != 1 || result.Binding.ArtifactIDs[0] != result.ArtifactRefs[0].ID {
		t.Fatalf("task transcript membership = %#v, binding=%#v", result.ArtifactRefs, result.Binding)
	}
	if err := manifest.Verify(context.Background(), mustArtifactStore(t, workspace)); err != nil {
		t.Fatalf("manifest verification failed: %v", err)
	}
}

func TestEvidenceManifestProjectsDeduplicatedTranscriptPerCurrentOccurrence(t *testing.T) {
	workspace := t.TempDir()
	store := mustArtifactStore(t, workspace)
	immutable, err := store.Put(context.Background(), PutArtifactRequest{
		Kind: "task_transcript", Path: "run-a.jsonl", Content: []byte("same transcript"),
		RunID: "run-a", TaskID: "task-a", Attempt: 1, Agent: "worker-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(context.Background(), immutable.ID)
	if err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "run-b"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker-b", Desc: "reused transcript"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	item.VerifyResult = &VerificationResult{ExitCode: 0}
	item.ExecutionReceipts = []ExecutionReceipt{{
		RunID: "run-b", TaskID: item.ID, Attempt: 2, ModelExecutionID: "exec-b", ProducerID: "worker-b",
		TranscriptRef: immutable.ID, ExitCode: evidenceIntPtr(0),
	}}

	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.ArtifactRefs) != 1 {
		t.Fatalf("manifest artifact refs = %#v, want one occurrence", manifest.ArtifactRefs)
	}
	occurrence := manifest.ArtifactRefs[0]
	if occurrence.ID != immutable.ID || occurrence.SHA256 != immutable.SHA256 || occurrence.ByteSize != immutable.ByteSize {
		t.Fatalf("occurrence immutable fields = %#v, want ID/digest/size from store %#v", occurrence, immutable)
	}
	if occurrence.RunID != "run-b" || occurrence.TaskID != item.ID || occurrence.Attempt != 2 || occurrence.Agent != "worker-b" {
		t.Fatalf("occurrence binding = %#v, want current run/task/attempt/agent", occurrence)
	}
	after, err := store.Get(context.Background(), immutable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("immutable store metadata changed: before=%#v after=%#v", before, after)
	}
}

func TestEvidenceManifestVerifiesTwoCurrentOccurrencesWithOneArtifactID(t *testing.T) {
	workspace := t.TempDir()
	store := mustArtifactStore(t, workspace)
	artifact, err := store.Put(context.Background(), PutArtifactRequest{
		Kind: "opaque", Path: "first", Content: []byte("shared"), RunID: "old-run", TaskID: "old-task", Attempt: 1, Agent: "old-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: NewTaskTracker(), executionRunID: "current-run"}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker-a", Desc: "first occurrence"},
		{Agent: "worker-b", Desc: "second occurrence"},
	})
	for i, item := range items {
		c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
		item.VerifyResult = &VerificationResult{ExitCode: 0}
		item.ExecutionReceipts = []ExecutionReceipt{{
			RunID: "current-run", TaskID: item.ID, Attempt: 1, ModelExecutionID: "exec-" + item.ID,
			ProducerID: item.Agent, TranscriptRef: artifact.ID, ExitCode: evidenceIntPtr(0),
		}}
		if i == 0 {
			item.TypedResult = &TaskResult{Status: "success", Artifacts: []ArtifactRef{artifact.ArtifactRef}}
		}
	}

	manifest, err := c.buildEvidenceManifest(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(context.Background(), store); err != nil {
		t.Fatalf("manifest with duplicate content occurrences failed verification: %v", err)
	}
	if len(manifest.ArtifactRefs) != 2 || manifest.ArtifactRefs[0].ID != artifact.ID || manifest.ArtifactRefs[1].ID != artifact.ID {
		t.Fatalf("manifest occurrences = %#v, want two refs with ID %q", manifest.ArtifactRefs, artifact.ID)
	}
	for i, item := range items {
		ref := manifest.ArtifactRefs[i]
		if ref.RunID != "current-run" || ref.TaskID != item.ID || ref.Agent != item.Agent {
			t.Fatalf("occurrence %d = %#v, want task=%q agent=%q", i, ref, item.ID, item.Agent)
		}
	}
}

func evidenceIntPtr(v int) *int { return &v }

func mustArtifactStore(t *testing.T, workspace string) *FileArtifactStore {
	t.Helper()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestEvidenceManifestRejectsPassedTaskWithoutBinding(t *testing.T) {
	manifest := &EvidenceManifest{RunID: "run-1", Status: "accepted", EvidenceResults: []EvidenceResult{{
		RequirementID: "task:task-1", Status: "passed",
	}}}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(context.Background(), bindingTestStore{}); err == nil {
		t.Fatal("passed task without sealed binding unexpectedly verified")
	}
}
