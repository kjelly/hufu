package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestDefaultContextCompilerInjectsLockedResourcesForCoordinator(t *testing.T) {
	c := &Coordinator{}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"coordinator"}}, Content: "coordinator rules content"},
	})
	cc := &defaultContextCompiler{c: c}

	compiled, err := cc.CompileCoordinatorContext(context.Background(), CoordinatorContextInput{})
	if err != nil {
		t.Fatalf("CompileCoordinatorContext() error = %v", err)
	}
	if !strings.Contains(compiled.Prompt, "coordinator rules content") {
		t.Fatalf("Prompt = %q, want it to include the locked resource content", compiled.Prompt)
	}
}

// TestDefaultContextCompilerInjectsLockedResourcesForNamedWorker also proves
// InjectInto matching is case-insensitive end-to-end through the wiring
// (the declared "Coder" matches the worker's actual name "coder").
func TestDefaultContextCompilerInjectsLockedResourcesForNamedWorker(t *testing.T) {
	c := &Coordinator{}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "coder-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"Coder"}}, Content: "coder-only content"},
	})
	cc := &defaultContextCompiler{c: c}

	compiled, err := cc.CompileWorkerContext(context.Background(), WorkerContextInput{AgentDef: &agent.AgentDef{Name: "coder"}})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v", err)
	}
	if !strings.Contains(compiled.Prompt, "coder-only content") {
		t.Fatalf("Prompt = %q, want it to include the locked resource content for coder", compiled.Prompt)
	}

	compiled, err = cc.CompileWorkerContext(context.Background(), WorkerContextInput{AgentDef: &agent.AgentDef{Name: "reviewer"}})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v", err)
	}
	if strings.Contains(compiled.Prompt, "coder-only content") {
		t.Fatalf("Prompt = %q, reviewer should not receive coder-only content", compiled.Prompt)
	}
}

func TestDefaultContextCompilerFallsBackToTaskDefAgentName(t *testing.T) {
	c := &Coordinator{}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "helper-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"helper"}}, Content: "helper content"},
	})
	cc := &defaultContextCompiler{c: c}

	compiled, err := cc.CompileWorkerContext(context.Background(), WorkerContextInput{TaskDef: TaskDef{Agent: "helper"}})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v", err)
	}
	if !strings.Contains(compiled.Prompt, "helper content") {
		t.Fatalf("Prompt = %q, want it to include the locked resource content via TaskDef.Agent", compiled.Prompt)
	}
}

// TestDefaultContextCompilerSkipsInjectionWithNoAgentIdentity mirrors the
// real auxiliary_context.go call site, which builds a WorkerContextInput
// with neither AgentDef nor TaskDef.Agent set (an isolated sidecar prompt).
func TestDefaultContextCompilerSkipsInjectionWithNoAgentIdentity(t *testing.T) {
	c := &Coordinator{}
	c.setLoadedRequiredResources([]*LoadedResource{
		{LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"coordinator", "coder"}}, Content: "should not appear"},
	})
	cc := &defaultContextCompiler{c: c}

	compiled, err := cc.CompileWorkerContext(context.Background(), WorkerContextInput{})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v", err)
	}
	if strings.Contains(compiled.Prompt, "should not appear") {
		t.Fatalf("Prompt = %q, want no injection without an agent identity", compiled.Prompt)
	}
}

func TestDefaultContextCompilerLegacyTeamUnaffected(t *testing.T) {
	c := &Coordinator{}
	cc := &defaultContextCompiler{c: c}

	coordCompiled, err := cc.CompileCoordinatorContext(context.Background(), CoordinatorContextInput{Goal: "do the thing"})
	if err != nil {
		t.Fatalf("CompileCoordinatorContext() error = %v", err)
	}
	if !strings.Contains(coordCompiled.Prompt, "do the thing") {
		t.Fatalf("Prompt = %q, want the ordinary goal content preserved", coordCompiled.Prompt)
	}

	workerCompiled, err := cc.CompileWorkerContext(context.Background(), WorkerContextInput{AgentDef: &agent.AgentDef{Name: "coder"}, Goal: "do the other thing"})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v", err)
	}
	if !strings.Contains(workerCompiled.Prompt, "do the other thing") {
		t.Fatalf("Prompt = %q, want the ordinary goal content preserved", workerCompiled.Prompt)
	}
}

// TestCompileCoordinatorContextRequiredLockedResourceFailsClosedOnBudget
// reuses the PR-3 proof (TestRequiredResourceCannotBeDroppedByTokenBudget)
// through the real CompileCoordinatorContext free function this time,
// confirming the pipeline still fails closed end-to-end, not just in
// isolation against BudgetContextItems directly.
func TestCompileCoordinatorContextRequiredLockedResourceFailsClosedOnBudget(t *testing.T) {
	input := CoordinatorContextInput{
		ModelContext: ModelContextSpec{ContextWindow: 10, MaxOutputTokens: 1, SafetyMarginTokens: 1},
		LockedResourceItems: LockedResourceContextItems([]*LoadedResource{
			{LockedResource: LockedResource{Name: "team-rules", Kind: agent.ResourceProjectRules, InjectInto: []string{"coordinator"}}, Content: strings.Repeat("word ", 1000)},
		}, "coordinator"),
	}

	_, err := CompileCoordinatorContext(context.Background(), input)
	if err == nil {
		t.Fatal("CompileCoordinatorContext() = nil error, want a fail-closed error for a required item that cannot fit")
	}
	if !strings.Contains(err.Error(), "team-rules") {
		t.Fatalf("error = %v, want it to name the required item", err)
	}
}
