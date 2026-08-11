package team

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenArtifactRefAuthorizesDeclaredDependencyAndRejectsTypo(t *testing.T) {
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", Desc: "produce transcript"}})[0]
	consumer := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "auditor", Desc: "audit transcript"}})[0]
	consumer.DependsOn = []string{producer.ID}

	source := filepath.Join(workspace, "transcript.jsonl")
	if err := os.WriteFile(source, []byte("authoritative evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), PutArtifactRequest{
		Kind: "task_transcript", Path: source, SourcePath: source, TaskID: producer.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{TaskID: producer.ID, Status: TaskResultStatusSuccess, Summary: "done", RawOutputRef: &ref, Source: "runtime", Confidence: 1}
	if err := tracker.TodoList().SetTypedResult(producer.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(producer.ID, TaskDone, "done", "done"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: tracker}

	ctx := context.WithValue(context.Background(), todoIDKey{}, consumer.ID)
	reader, err := c.openArtifactRef(ctx, ref.ID)
	if err != nil {
		t.Fatalf("declared dependency ref rejected: %v", err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(data) != "authoritative evidence\n" {
		t.Fatalf("resolved data=%q err=%v", data, err)
	}

	_, err = c.openArtifactRef(ctx, ref.ID+"-typo")
	if err == nil || !strings.Contains(err.Error(), "unknown or not authorized") {
		t.Fatalf("mistyped ref error=%v", err)
	}

	result.RawOutputRef.SHA256 = "tampered"
	if _, err := c.openArtifactRef(ctx, ref.ID); err == nil || !strings.Contains(err.Error(), "integrity verification") {
		t.Fatalf("tampered ref error=%v", err)
	}
}

func TestOpenArtifactRefRejectsUndeclaredProducer(t *testing.T) {
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", Desc: "produce"}})[0]
	consumer := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "auditor", Desc: "audit without dependency"}})[0]
	source := filepath.Join(workspace, "result.txt")
	if err := os.WriteFile(source, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), PutArtifactRequest{Kind: "artifact", Path: source, SourcePath: source, TaskID: producer.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().SetTypedResult(producer.ID, &TaskResult{TaskID: producer.ID, Status: TaskResultStatusSuccess, Summary: "done", Artifacts: []ArtifactRef{ref}}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(producer.ID, TaskDone, "done", "done"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: tracker}
	ctx := context.WithValue(context.Background(), todoIDKey{}, consumer.ID)
	if _, err := c.openArtifactRef(ctx, ref.ID); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("undeclared producer ref error=%v", err)
	}
}
