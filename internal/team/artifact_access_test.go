package team

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
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

func TestBuildArtifactAccessScopeBindsManagedSkillSnapshot(t *testing.T) {
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "unrelated gardening", Goal: "runtime review assigned source"}})[0]
	skillPath := filepath.Join(t.TempDir(), "hufu-runtime-code-review", "SKILL.md")
	managedSkill := &skill.SkillDef{
		Name: "runtime-review", Description: "runtime review", Path: skillPath,
		Content: "immutable runtime review instructions",
	}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Agents: map[string]*agent.AgentDef{
				"worker": &agent.AgentDef{Name: "worker", Role: "runtime reviewer"},
			},
		},
		projectDir:       workspace,
		taskTracker:      tracker,
		skills:           []*skill.SkillDef{managedSkill},
		autoLoadedSkills: []*skill.SkillDef{managedSkill},
		executionRunID:   "run-1",
	}

	scope, err := c.buildArtifactAccessScope(item.ID, 1)
	if err != nil {
		t.Fatalf("buildArtifactAccessScope: %v", err)
	}
	if len(scope.ManagedSkillRefs) != 1 {
		t.Fatalf("managed skill refs = %#v, want one", scope.ManagedSkillRefs)
	}
	ref := scope.ManagedSkillRefs[0]
	if ref.Kind != "skill" || ref.Role != "instruction" || ref.Path != skillPath || ref.SHA256 == "" {
		t.Fatalf("managed skill ref = %#v, want immutable skill metadata", ref)
	}
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(t.Context(), ref); err != nil {
		t.Fatalf("managed skill snapshot failed verification: %v", err)
	}
}

func TestBuildArtifactAccessScopeUsesLivePromptGoalForManagedSkills(t *testing.T) {
	workspace := t.TempDir()
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "durable description", Goal: "alpha durable goal",
	}})[0]
	alpha := &skill.SkillDef{
		Name: "alpha-review", Description: "alpha review", Path: filepath.Join(t.TempDir(), "alpha", "SKILL.md"), Content: "alpha instructions",
	}
	beta := &skill.SkillDef{
		Name: "beta-review", Description: "beta review", Path: filepath.Join(t.TempDir(), "beta", "SKILL.md"), Content: "beta instructions",
	}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "alpha beta reviewer"},
			},
		},
		projectDir:       workspace,
		taskTracker:      tracker,
		skills:           []*skill.SkillDef{alpha, beta},
		autoLoadedSkills: []*skill.SkillDef{alpha, beta},
		executionRunID:   "run-live-goal",
	}

	scope, err := c.buildArtifactAccessScope(item.ID, 1, "beta live goal")
	if err != nil {
		t.Fatalf("buildArtifactAccessScope: %v", err)
	}
	if len(scope.ManagedSkillRefs) != 1 || scope.ManagedSkillRefs[0].Description != beta.Name {
		t.Fatalf("managed skill refs = %#v, want only live-goal skill %q", scope.ManagedSkillRefs, beta.Name)
	}
}

func TestCloneCoordinatorUsesParentArtifactStoreRootForScopedEvidence(t *testing.T) {
	parentWorkspace := t.TempDir()
	childWorkspace := t.TempDir()
	store, err := NewFileArtifactStore(parentWorkspace, parentWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Put(t.Context(), PutArtifactRequest{
		ID: "custom-parent-ref", Kind: "task_output", Role: "evidence", Path: "parent.txt", Content: []byte("parent evidence"),
	})
	if err != nil {
		t.Fatal(err)
	}
	orig := &Coordinator{
		session:        &TeamSession{Workspace: parentWorkspace},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-1",
	}
	clone := cloneCoordinator(orig, &TeamSession{Workspace: childWorkspace})
	if got := clone.artifactStoreRootPath(); got != parentWorkspace {
		t.Fatalf("clone artifact store root = %q, want parent workspace %q", got, parentWorkspace)
	}
	scope := &ArtifactAccessScope{
		RunID: "run-1", TaskID: "consumer", Attempt: 1,
		AuthorizedRefs: []ArtifactRef{stored.ArtifactRef},
	}
	ctx := context.WithValue(t.Context(), todoIDKey{}, "consumer")
	ctx = context.WithValue(ctx, executionAttemptKey{}, 1)
	ctx = context.WithValue(ctx, artifactAccessScopeKey, scope)
	reader, err := clone.openArtifactRef(ctx, stored.ID)
	if err != nil {
		t.Fatalf("clone failed to open parent-root artifact: %v", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil || string(data) != "parent evidence" {
		t.Fatalf("clone parent artifact data = %q, err=%v", data, err)
	}
}
