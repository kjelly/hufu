package team

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func TestPlanFirstTaskCloningAndErrorPropagation(t *testing.T) {
	// Initialize coordinator with necessary structs
	c := &Coordinator{
		session: &TeamSession{
			Agents: map[string]*agent.AgentDef{
				"developer": {Name: "Developer", Role: "worker"},
			},
			Config: agent.TeamConfig{
				MaxRounds: 5,
			},
		},
		pendingPlans:    make(map[string]*PlanEntry),
		approvedOutputs: make(map[string]string),
		approvedErrors:  make(map[string]error),
	}

	task := TaskDef{
		Agent:        "developer",
		Goal:         "implement feature",
		Constraints:  "must be thread-safe",
		Verify:       "go test ./...",
		ContextFiles: []string{"a.go", "b.go"},
	}

	// 1. Verify cloneTaskDef works correctly
	cloned := cloneTaskDef(task)
	if cloned.Agent != task.Agent || cloned.Goal != task.Goal || cloned.Constraints != task.Constraints || cloned.Verify != task.Verify {
		t.Errorf("cloneTaskDef failed: fields mismatch. Cloned: %+v", cloned)
	}
	if len(cloned.ContextFiles) != len(task.ContextFiles) || cloned.ContextFiles[0] != task.ContextFiles[0] {
		t.Errorf("cloneTaskDef failed: ContextFiles mismatch. Cloned: %+v", cloned)
	}

	// 2. Test PlanEntry storing the TaskDef
	todoID := "task-123"
	entry := &PlanEntry{
		TodoID:      todoID,
		Agent:       "developer",
		Goal:        task.Goal,
		PlanText:    "1. write code\n2. run tests",
		Status:      "submitted",
		ReviewCount: 1,
		Task:        task,
	}
	c.pendingPlans[todoID] = entry

	// Assert entry fields
	if c.pendingPlans[todoID].Task.Verify != "go test ./..." {
		t.Errorf("expected Task.Verify to be 'go test ./...', got %q", c.pendingPlans[todoID].Task.Verify)
	}

	// Verify that cloneCoordinator copies Task and approvedErrors correctly
	clonedCoord := cloneCoordinator(c, c.session)
	if clonedCoord.pendingPlans[todoID] == nil {
		t.Fatal("cloned coordinator pendingPlans entry is nil")
	}
	if clonedCoord.pendingPlans[todoID].Task.Verify != "go test ./..." {
		t.Errorf("cloned coordinator pendingPlans Task.Verify mismatch, got %q", clonedCoord.pendingPlans[todoID].Task.Verify)
	}

	// Verify that cloning is deep and we can write to clone without affecting original
	clonedCoord.pendingPlans[todoID].Task.Verify = "modified verify"
	if c.pendingPlans[todoID].Task.Verify != "go test ./..." {
		t.Errorf("modification to cloned task affected original task")
	}
}

// TestPlanReviewerUsesConfiguredModel verifies getPlanReviewer dispatches to
// the coordinator's resolved planReviewerModel rather than the main model.
// Model resolution itself (team.yaml > hufu.yaml > main model precedence) is
// covered by config.TestResolvePlanReviewerModel; this test only guards the
// wiring between that resolved value and the agent actually created.
func TestPlanReviewerUsesConfiguredModel(t *testing.T) {
	pm, err := agent.NewProviderManager("http://localhost:11434", "", nil)
	if err != nil {
		t.Fatalf("NewProviderManager failed: %v", err)
	}
	c := &Coordinator{
		session: &TeamSession{
			Config: agent.TeamConfig{
				Generation: agent.GenerationParams{
					Model: "main-model",
				},
			},
		},
		providerManager:   pm,
		planReviewerModel: "custom-reviewer-model",
		pendingPlans:      make(map[string]*PlanEntry),
	}

	pr, err := c.getPlanReviewer(context.Background(), "todo-1")
	if err != nil {
		t.Fatalf("getPlanReviewer failed: %v", err)
	}
	if pr.modelID != "custom-reviewer-model" {
		t.Errorf("expected modelID to be custom-reviewer-model, got %q", pr.modelID)
	}
}

type planReviewerProfileIntrospector struct{}

func (planReviewerProfileIntrospector) InspectModel(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 4_096, MaxOutputTokens: 256}, nil
}

func TestPlanReviewerAuxiliaryCompilationUsesReviewerBoundProfile(t *testing.T) {
	const reviewerModel = "distinct-plan-reviewer-model"
	providerManager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatalf("NewProviderManager failed: %v", err)
	}
	profileRuntime := &ModelProfileRuntime{
		manager: providerManager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return planReviewerProfileIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	workspace := t.TempDir()
	compileErr := errors.New("stop before plan reviewer provider call")
	compiler := &mockContextCompiler{compileWorkerErr: compileErr}
	c := &Coordinator{
		session: &TeamSession{
			Dir:       workspace,
			Workspace: workspace,
			Agents:    map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}},
			Config:    agent.TeamConfig{Generation: agent.GenerationParams{MaxTokens: "2048"}},
		},
		providerManager:     providerManager,
		modelProfileRuntime: profileRuntime,
		planReviewerModel:   reviewerModel,
		pendingPlans:        map[string]*PlanEntry{"todo-1": {TodoID: "todo-1", Agent: "worker", Goal: "review this plan"}},
		taskTracker:         NewTaskTracker(),
		sessionData:         NewSession(),
		projectDir:          workspace,
		reportStatus:        func(StatusEvent) {},
		executionRunID:      "plan-reviewer-profile-test",
	}
	c.SetContextCompiler(compiler)

	reviewer, err := c.getPlanReviewer(t.Context(), "todo-1")
	if err != nil {
		t.Fatalf("getPlanReviewer failed: %v", err)
	}
	bound := reviewer.providerBoundInvocationContext
	if !bound.AdmissionContext.IsBound() || bound.ModelID != reviewerModel {
		t.Fatalf("reviewer invocation was not provider-bound: %#v", bound)
	}
	if bound.ModelContext.ContextWindow != 4_096 || bound.ModelContext.MaxOutputTokens != 2_048 {
		t.Fatalf("reviewer model context = %#v, want distinct runtime profile", bound.ModelContext)
	}

	_, _, execErr, reviewErr := reviewer.review(t.Context(), "1. inspect the change")
	if execErr != nil || !errors.Is(reviewErr, compileErr) {
		t.Fatalf("review errors = execution %v, review %v; want compiler error only", execErr, reviewErr)
	}
	if compiler.workerModelContext != bound.ModelContext {
		t.Fatalf("auxiliary compiler model context = %#v, want reviewer-bound %#v", compiler.workerModelContext, bound.ModelContext)
	}
	if compiler.workerModelContext == GlobalModelSpecRegistry().GetSpec("") {
		t.Fatalf("auxiliary compiler used the empty-model global profile: %#v", compiler.workerModelContext)
	}
}

func TestCloneCoordinatorCopiesCompactionState(t *testing.T) {
	orig := &Coordinator{
		initialPrompt: "implement feature x",
		lastCompactionSummary: &StructuredSummary{
			Goal:                "implement feature x",
			CompletedTasks:      []string{"task-a"},
			FilesModified:       []string{"a.go"},
			VerificationResults: []string{"FAIL: go test"},
		},
		session: &TeamSession{},
	}

	cloned := cloneCoordinator(orig, orig.session)
	if cloned.initialPrompt != orig.initialPrompt {
		t.Fatalf("initialPrompt not preserved: got %q want %q", cloned.initialPrompt, orig.initialPrompt)
	}

	if cloned.lastCompactionSummary == nil {
		t.Fatal("cloned lastCompactionSummary is nil")
	}
	if cloned.lastCompactionSummary.Goal != orig.lastCompactionSummary.Goal {
		t.Errorf("compaction goal mismatch: got %q want %q", cloned.lastCompactionSummary.Goal, orig.lastCompactionSummary.Goal)
	}
	if cloned.lastCompactionSummary == orig.lastCompactionSummary {
		t.Fatal("cloned lastCompactionSummary should be a deep copy, but pointer is shared")
	}

	cloned.lastCompactionSummary.FilesModified[0] = "b.go"
	if orig.lastCompactionSummary.FilesModified[0] != "a.go" {
		t.Fatalf("modifying cloned summary affected original: got %q", orig.lastCompactionSummary.FilesModified[0])
	}

	cloned.lastCompactionSummary.CompletedTasks = append(cloned.lastCompactionSummary.CompletedTasks, "task-b")
	if len(orig.lastCompactionSummary.CompletedTasks) != 1 {
		t.Fatalf("modifying cloned summary mutated original completed tasks: %#v", orig.lastCompactionSummary.CompletedTasks)
	}
}

func TestCloneCoordinatorKeepsNilCompactionSummary(t *testing.T) {
	orig := &Coordinator{
		initialPrompt:         "initial goal",
		lastCompactionSummary: nil,
		session:               &TeamSession{},
	}

	cloned := cloneCoordinator(orig, orig.session)
	if cloned.initialPrompt != "initial goal" {
		t.Fatalf("initialPrompt not preserved when lastCompactionSummary is nil: got %q", cloned.initialPrompt)
	}
	if cloned.lastCompactionSummary != nil {
		t.Fatalf("expected cloned lastCompactionSummary to be nil, got %#v", cloned.lastCompactionSummary)
	}
}

func TestCloneCoordinatorCopiesInitialPromptEvenWhenLastSummaryNil(t *testing.T) {
	orig := &Coordinator{
		initialPrompt:         "initial prompt should survive",
		lastCompactionSummary: nil,
		session:               &TeamSession{},
	}

	cloned := cloneCoordinator(orig, orig.session)
	if cloned.initialPrompt != orig.initialPrompt {
		t.Fatalf("initialPrompt not preserved: got %q want %q", cloned.initialPrompt, orig.initialPrompt)
	}
	if cloned.lastCompactionSummary != nil {
		t.Fatalf("expected nil lastCompactionSummary after clone, got %#v", cloned.lastCompactionSummary)
	}
}

func TestCloneCoordinatorPreservesContextRoutingRuntimeState(t *testing.T) {
	orig := &Coordinator{
		session: &TeamSession{}, executionRunID: "run-extra-model", executionTeamRevision: "team-rev",
		memoryRankingPolicy: MemoryRuntimeRankingPolicy{CandidateTopK: 20, InjectTopK: 4, MinimumRelevance: .2},
		executionProfile:    ExecutionProfile{Name: ProfileFreshVerification, DisableHistoricalMemory: true},
		goalMode:            GoalModeOutcome,
	}
	cloned := cloneCoordinator(orig, orig.session)
	if cloned.executionRunID != orig.executionRunID || cloned.executionTeamRevision != orig.executionTeamRevision {
		t.Fatalf("extra-model clone lost context run identity: %#v", cloned)
	}
	if cloned.memoryRankingPolicy != orig.memoryRankingPolicy || cloned.executionProfile != orig.executionProfile || cloned.goalMode != orig.goalMode {
		t.Fatalf("extra-model clone lost routing policy/profile: policy=%#v profile=%#v goal=%q", cloned.memoryRankingPolicy, cloned.executionProfile, cloned.goalMode)
	}
}
