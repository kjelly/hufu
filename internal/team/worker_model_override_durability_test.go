package team

import (
	"context"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

// These tests pin the durable side of the CLI --worker-model override
// (cmd/hufu applyCLIGenerationOverridesToAgents), which changes only a loaded
// worker's Generation.Model. Once an occurrence is admitted its frozen
// ExecutionTarget stays authoritative: a changed override must never retarget
// a retried, checkpoint-restored, or resumed task.

var frozenWorkerTargetA = execution.ExecutionTarget{Backend: "codex", Model: "gpt-a"}

func TestWorkerModelChangeDoesNotRetargetRetriedOccurrence(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-worker-model", "session-worker-model")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	coder := &agent.AgentDef{Name: "coder", Role: "worker", Generation: agent.GenerationParams{Model: "codex/gpt-a"}}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Agents: map[string]*agent.AgentDef{"coder": coder}},
		taskTracker:    NewTaskTracker(),
		executionRunID: "run-worker-model",
		reportStatus:   func(StatusEvent) {},
		eventStore:     store,
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "coder", Desc: "implement feature X", Model: "codex/gpt-a",
		ExecutionTarget: frozenWorkerTargetA, ExecutionTopology: []execution.ExecutionTarget{frozenWorkerTargetA},
		SubagentProvider: "codex",
	}})[0]

	// The operator now runs with --worker-model coder=codex/gpt-b.
	coder.Generation.Model = "codex/gpt-b"
	if err := c.CommitTaskResetForRetry(context.Background(), item.ID, "retry after worker-model change"); err != nil {
		t.Fatal(err)
	}
	live := c.taskTracker.TodoList().Items()[0]
	if live.ExecutionTarget != frozenWorkerTargetA || !reflect.DeepEqual(live.ExecutionTopology, []execution.ExecutionTarget{frozenWorkerTargetA}) {
		t.Fatalf("retried occurrence target = %#v topology %#v, want frozen %#v", live.ExecutionTarget, live.ExecutionTopology, frozenWorkerTargetA)
	}
	model, err := c.resolveTaskExecutionModel(coder, taskDefFromTodoItem(live), live.ID)
	if err != nil {
		t.Fatalf("resolveTaskExecutionModel: %v", err)
	}
	if !strings.HasSuffix(model, "gpt-a") {
		t.Fatalf("retried occurrence resolved model %q, want frozen gpt-a", model)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatalf("ReplayTodoList: %v", err)
	}
	if len(replayed) != 1 || replayed[0].ExecutionTarget != frozenWorkerTargetA {
		t.Fatalf("replayed retry target = %#v, want frozen %#v", replayed, frozenWorkerTargetA)
	}
}

func TestWorkerModelChangeDoesNotRetargetCheckpointRestoredOccurrence(t *testing.T) {
	runtime := &taskOccurrenceModelRuntime{model: "live-model"}
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir()},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		modelRuntime: runtime,
		reportStatus: func(StatusEvent) {},
	}
	c.SetSessionData(&SessionData{Tasks: []*TodoItem{{
		ID: "3", Agent: "coder", Goal: "resume the admitted work", Status: TaskInProgress,
		Model: "gpt-a", ExecutionTarget: frozenWorkerTargetA, SubagentProvider: "codex",
	}}})
	c.SetEventJournal(nil)

	// The restarted process loaded the team with --worker-model coder=codex/gpt-b.
	coder := &agent.AgentDef{Name: "coder", Role: "worker", Generation: agent.GenerationParams{Model: "codex/gpt-b"}}
	restored := c.taskTracker.TodoList().Items()
	if len(restored) != 1 || restored[0].ExecutionTarget != frozenWorkerTargetA {
		t.Fatalf("restored occurrences = %#v, want frozen target %#v", restored, frozenWorkerTargetA)
	}
	model, err := c.resolveTaskExecutionModel(coder, taskDefFromTodoItem(restored[0]), restored[0].ID)
	if err != nil {
		t.Fatalf("resolveTaskExecutionModel: %v", err)
	}
	if !strings.HasSuffix(model, "gpt-a") {
		t.Fatalf("restored occurrence resolved model %q, want frozen gpt-a", model)
	}
	if runtime.calls != 0 {
		t.Fatalf("live model runtime calls = %d, want 0 for a durable occurrence", runtime.calls)
	}
}

func newWorkerModelResumeCoordinator(t *testing.T, workspace, coderModel string) *Coordinator {
	t.Helper()
	session := &TeamSession{
		Dir:       workspace,
		Workspace: workspace,
		Config:    agent.TeamConfig{Name: "worker-model-resume", DefaultLLMBackend: "ollama"},
		Agents: map[string]*agent.AgentDef{
			"coder":    {Name: "coder", Role: "worker", Generation: agent.GenerationParams{Model: coderModel}},
			"reviewer": {Name: "reviewer", Role: "worker", Generation: agent.GenerationParams{Model: "ollama/reviewer"}},
		},
	}
	subjectRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SetCompatibilityWorkspaceScope(subjectRoot); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c
}

func closeWorkerModelResumeStore(t *testing.T, c *Coordinator) {
	t.Helper()
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	c.eventStore = nil
}

// TestWorkerModelChangeOnResumeFailsClosedOnPolicyDrift pins the production
// resume boundary: every worker's resolved target is part of the frozen
// execution-policy snapshot, so resuming a workspace with a different
// --worker-model is rejected before admission instead of silently mixing
// targets. The same run resumes normally when the override is unchanged.
func TestWorkerModelChangeOnResumeFailsClosedOnPolicyDrift(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())

	first := newWorkerModelResumeCoordinator(t, workspace, "codex/gpt-a")
	first.initEventStore()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	closeWorkerModelResumeStore(t, first)
	checkpoint := LoadSession(workspace)
	if checkpoint == nil || checkpoint.ExecutionPolicySnapshot == nil {
		t.Fatal("first admission did not persist an execution policy snapshot")
	}

	changed := newWorkerModelResumeCoordinator(t, workspace, "codex/gpt-b")
	changed.SetSessionData(LoadSession(workspace))
	changed.initEventStore()
	if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("resume with changed worker model error = %v, want policy snapshot drift", err)
	}
	events, err := changed.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	policyEvents := 0
	for _, event := range events {
		if event.Type == string(EventExecutionPolicySnapshot) {
			policyEvents++
		}
	}
	if policyEvents != 1 {
		t.Fatalf("policy events after rejected resume = %d, want 1", policyEvents)
	}
	closeWorkerModelResumeStore(t, changed)

	unchanged := newWorkerModelResumeCoordinator(t, workspace, "codex/gpt-a")
	unchanged.SetSessionData(LoadSession(workspace))
	unchanged.initEventStore()
	if err := unchanged.checkRunAdmission(); err != nil {
		t.Fatalf("resume with unchanged worker model: %v", err)
	}
}

// recordingCapabilityIntrospector reports tool support for every model except
// those listed as weak, and records every model it was asked to profile.
type recordingCapabilityIntrospector struct {
	mu     sync.Mutex
	weak   string
	models []string
}

func (r *recordingCapabilityIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, modelID string) (providerintrospection.RuntimeModelInfo, error) {
	r.mu.Lock()
	r.models = append(r.models, modelID)
	r.mu.Unlock()
	state := providerintrospection.CapabilityYes
	if strings.Contains(modelID, r.weak) {
		state = providerintrospection.CapabilityNo
	}
	return providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": state}}, nil
}

func (r *recordingCapabilityIntrospector) takeModels() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	models := r.models
	r.models = nil
	return models
}

// TestCapabilityValidationChecksOverriddenWorkerTarget proves an override
// cannot bypass model requirements: validation and profiling follow the
// effective overridden target, never the stale Markdown model.
func TestCapabilityValidationChecksOverriddenWorkerTarget(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	introspector := &recordingCapabilityIntrospector{weak: "weak-override"}
	runtime := &ModelProfileRuntime{
		manager: manager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return introspector
		}, modelprofile.ProfileCacheOptions{}),
	}
	coder := &agent.AgentDef{
		Name: "coder", Role: "worker",
		Generation:   agent.GenerationParams{Model: "ollama/strong-markdown"},
		Requirements: agent.ContractRequirements{Model: agent.ModelRequirements{Tools: true}},
	}
	c := &Coordinator{
		modelProfileRuntime: runtime,
		session:             &TeamSession{Agents: map[string]*agent.AgentDef{"coder": coder}},
	}
	if err := c.ValidateModelCapabilities(t.Context()).Err(); err != nil {
		t.Fatalf("Markdown target validation: %v", err)
	}
	introspector.takeModels()

	// --worker-model coder=ollama/weak-override
	coder.Generation.Model = "ollama/weak-override"
	validation := c.ValidateModelCapabilities(t.Context())
	if err := validation.Err(); err == nil || !strings.Contains(err.Error(), "weak-override") {
		t.Fatalf("overridden target validation error = %v, want tools capability failure for the override", err)
	}
	models := introspector.takeModels()
	if len(models) == 0 {
		t.Fatal("capability validation did not profile the overridden target")
	}
	for _, model := range models {
		if strings.Contains(model, "strong-markdown") {
			t.Fatalf("capability validation profiled stale Markdown target %q (all: %q)", model, models)
		}
	}
}
