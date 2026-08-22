package team

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	runtimeTools "github.com/kjelly/hufu/internal/tools"
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
		Kind: "task_transcript", Path: source, SourcePath: source, RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{TaskID: producer.ID, Attempt: 1, Agent: "producer", Status: TaskResultStatusSuccess, Summary: "done", RawOutputRef: &ref.ArtifactRef, Source: "runtime", Confidence: 1}
	if err := tracker.TodoList().SetTypedResult(producer.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(producer.ID, TaskDone, "done", "done"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, executionRunID: "run-1", taskTracker: tracker}

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

	tampered := cloneTaskResult(result)
	tampered.RawOutputRef.SHA256 = "tampered"
	if err := tracker.TodoList().SetTypedResult(producer.ID, tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := c.openArtifactRef(ctx, ref.ID); err == nil || !strings.Contains(err.Error(), "integrity verification") {
		t.Fatalf("tampered ref error=%v", err)
	}
}

func TestScopedArtifactViewOpensCurrentUnboundDependencyArtifactSources(t *testing.T) {
	for _, source := range unboundArtifactSources() {
		t.Run(source.name, func(t *testing.T) {
			c, producer, consumer, ref := newUnboundArtifactAccessFixture(t)
			source.set(producer.TypedResult, ref)

			scope, err := c.buildArtifactAccessScope(consumer.ID, 1)
			if err != nil {
				t.Fatalf("buildArtifactAccessScope: %v", err)
			}
			ctx := context.WithValue(context.Background(), todoIDKey{}, consumer.ID)
			ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
			ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(scope))
			ctx = runtimeTools.SetToolsAllowed(ctx, []string{"view"})
			view := runtimeTools.NewViewTool(runtimeTools.WithArtifactOpener(c.openArtifactRef))
			response, err := view.Run(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"` + ref.ID + `"}`})
			if err != nil || response.IsError || !strings.Contains(response.Content, "current unbound output") {
				t.Fatalf("scoped view response=%#v err=%v", response, err)
			}
		})
	}
}

func TestScopedArtifactViewRequiresCurrentUnboundDependencyOccurrenceFromEverySource(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*ArtifactRef)
	}{
		{name: "run", mutate: func(ref *ArtifactRef) { ref.RunID = "run-stale" }},
		{name: "task", mutate: func(ref *ArtifactRef) { ref.TaskID = "other-task" }},
		{name: "attempt", mutate: func(ref *ArtifactRef) { ref.Attempt = 2 }},
		{name: "agent", mutate: func(ref *ArtifactRef) { ref.Agent = "other-agent" }},
	}

	for _, source := range unboundArtifactSources() {
		t.Run(source.name, func(t *testing.T) {
			for _, mutation := range mutations {
				t.Run(mutation.name, func(t *testing.T) {
					c, producer, consumer, ref := newUnboundArtifactAccessFixture(t)
					mutation.mutate(&ref)
					source.set(producer.TypedResult, ref)

					if _, err := c.buildArtifactAccessScope(consumer.ID, 1); err == nil {
						t.Fatal("buildArtifactAccessScope succeeded for invalid producer occurrence")
					}

					ctx := context.WithValue(context.Background(), todoIDKey{}, consumer.ID)
					if _, err := c.openArtifactRef(ctx, ref.ID); err == nil {
						t.Fatal("unscoped ordinary-dependency fallback authorized invalid producer occurrence")
					}
				})
			}
		})
	}
}

type unboundArtifactSource struct {
	name string
	set  func(*TaskResult, ArtifactRef)
}

func unboundArtifactSources() []unboundArtifactSource {
	return []unboundArtifactSource{
		{name: "artifacts", set: func(result *TaskResult, ref ArtifactRef) {
			result.Artifacts = []ArtifactRef{ref}
		}},
		{name: "raw_output_ref", set: func(result *TaskResult, ref ArtifactRef) {
			result.RawOutputRef = &ref
		}},
		{name: "outputs_artifact", set: func(result *TaskResult, ref ArtifactRef) {
			result.Outputs = map[string]StructuredOutputValue{
				"artifact": {Kind: ExecutionOutputArtifact, Artifact: &ref},
			}
		}},
	}
}

func newUnboundArtifactAccessFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, ArtifactRef) {
	t.Helper()
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", Desc: "produce"}})[0]
	consumer := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "auditor", Desc: "audit"}})[0]
	consumer.DependsOn = []string{producer.ID}
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), PutArtifactRequest{
		Content: []byte("current unbound output"), Kind: "task_output", Path: "output.txt",
		RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: producer.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{
		TaskID: producer.ID, Attempt: 1, Agent: producer.Agent, Status: TaskResultStatusSuccess,
		Summary: "done", Source: "runtime", Confidence: 1,
	}
	if err := tracker.TodoList().SetTypedResult(producer.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(producer.ID, TaskDone, "done", "done"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace}, executionRunID: "run-1",
		taskTracker: tracker, taskResults: map[string]*TaskResult{producer.ID: result},
	}
	return c, producer, consumer, ref.ArtifactRef
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
	if err := tracker.TodoList().SetTypedResult(producer.ID, &TaskResult{TaskID: producer.ID, Status: TaskResultStatusSuccess, Summary: "done", Artifacts: []ArtifactRef{ref.ArtifactRef}}); err != nil {
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
