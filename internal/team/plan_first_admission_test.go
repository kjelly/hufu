package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func TestValidatePlanFirstExecutionTarget(t *testing.T) {
	providerManager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:     &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "plan-first"}},
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(), reportStatus: func(StatusEvent) {},
		providerManager: providerManager,
	}
	registry := NewSubagentRegistry(NewHufuLocalSubagentProvider(c))
	if err := registry.Register(&directParityProvider{name: "external"}); err != nil {
		t.Fatal(err)
	}
	c.SetSubagentRegistry(registry)
	external := execution.ExecutionTarget{Backend: "external", Model: "m"}
	local := execution.ExecutionTarget{Backend: execution.OllamaBackendName, Model: "m"}
	if backend, err := c.ExecutionRegistry().ResolveBackend(local.Backend); err != nil || backend.Kind() != execution.BackendKindLLM {
		t.Fatalf("local backend = %v, %v; want a registered LLM backend", backend, err)
	}
	tests := []struct {
		name    string
		task    TaskDef
		wantErr bool
	}{
		{name: "planning on an external backend", task: TaskDef{PlanFirst: true, ResolvedExecutionTarget: external}, wantErr: true},
		{name: "planning on a Hufu model", task: TaskDef{PlanFirst: true, ResolvedExecutionTarget: local}},
		{name: "planning whose route falls back to an external backend", task: TaskDef{
			PlanFirst: true, ResolvedExecutionTarget: local,
			ExecutionRoute: &ExecutionRouteBinding{Name: "review", Candidates: []execution.ExecutionTarget{local, external}},
		}, wantErr: true},
		{name: "planning with an extra-model leaf on an external backend", task: TaskDef{
			PlanFirst: true, ResolvedExecutionTarget: local, ExecutionTopology: []execution.ExecutionTarget{local, external},
		}, wantErr: true},
		{name: "an approved plan executing on an external backend", task: TaskDef{PlanFirst: true, PlanID: "1", ResolvedExecutionTarget: external}},
		{name: "a task without plan-first on an external backend", task: TaskDef{ResolvedExecutionTarget: external}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.task.Agent = "worker"
			err := c.validatePlanFirstExecutionTarget(tt.task)
			if tt.wantErr != (err != nil) || (err != nil && !strings.Contains(err.Error(), planFirstExternalBackendCode)) {
				t.Fatalf("validatePlanFirstExecutionTarget = %v, want error %v", err, tt.wantErr)
			}
		})
	}
}

// TestPlanFirstTaskIsNotDispatchedToCodex pins the failure this guard closes:
// under --plan a Codex worker never calls submit_plan, so its planning turn
// used to do the work and finish the task before any plan was reviewed.
func TestPlanFirstTaskIsNotDispatchedToCodex(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", SideEffect: "workspace_write",
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	c := newCodexE2ECoordinatorBase(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-plan", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	}, worker)
	// The direct-dispatch fixture leaves the coordinated batch state unset.
	c.delegatedTasks = map[string]int{}
	c.pendingPlans = map[string]*PlanEntry{}
	c.forcePlanFirst = true

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "change the code"}})
	if err == nil || !strings.Contains(err.Error(), planFirstExternalBackendCode) {
		t.Fatalf("ExecuteTasks error = %v, want %s", err, planFirstExternalBackendCode)
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 0 {
		t.Fatalf("todo items = %#v, want none: the planning task must be rejected before it is created", items)
	}
}
