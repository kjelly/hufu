package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestExecuteTaskFailsClosedWhenWorkerContextPreflightFails(t *testing.T) {
	workspace := t.TempDir()
	calls := 0
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "context-preflight", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:         time.Now(),
		taskTracker:         NewTaskTracker(),
		reportStatus:        func(StatusEvent) {},
		taskResultCache:     make(map[string][]cachedTaskEntry),
		workerAgentOverride: &countingEmptyAgent{calls: &calls},
	}
	c.SetContextCompiler(&mockContextCompiler{compileWorkerErr: errors.New("normative conflict")})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "must not dispatch"}})[0]
	_, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "must not dispatch"}, item.ID)
	if err == nil || !strings.Contains(err.Error(), "worker context preflight failed") {
		t.Fatalf("executeTask() error = %v, want context preflight failure", err)
	}
	if calls != 0 {
		t.Fatalf("worker calls = %d, want zero before failed context preflight", calls)
	}
}

func TestBuildSystemPromptFailsClosedWhenCoordinatorContextPreflightFails(t *testing.T) {
	orch := &agent.AgentDef{Name: "coordinator", Role: "orchestrator", System: "coordinate safely", Generation: agent.GenerationParams{Model: "test"}}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "coordinator-context"},
			Agents:    map[string]*agent.AgentDef{"coordinator": orch},
		},
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	c.SetContextCompiler(&mockContextCompiler{compileCoordinatorErr: errors.New("context budget exceeded")})
	_, err := c.buildSystemPrompt(context.Background(), orch, "do work", false)
	if err == nil || !strings.Contains(err.Error(), "coordinator context preflight failed") {
		t.Fatalf("buildSystemPrompt() error = %v, want coordinator context preflight failure", err)
	}
}

func TestContextCompiler_DeduplicateItems(t *testing.T) {
	now := time.Now()
	items := []ContextItem{
		{
			ID:         "stm-1",
			Kind:       "stm",
			Content:    "Shared build rule: use go 1.26",
			Priority:   PriorityRecentSTM, // 8
			Confidence: 0.9,
			Freshness:  now.Add(-10 * time.Minute),
		},
		{
			ID:         "ltm-1",
			Kind:       "ltm",
			Content:    "Shared build rule: use go 1.26",
			Priority:   PriorityRelevantLTM, // 9
			Confidence: 0.8,
			Freshness:  now.Add(-1 * time.Hour),
		},
		{
			ID:         "goal-1",
			Kind:       "user_goal",
			Content:    "Build feature X",
			Priority:   PriorityUserGoal, // 1
			Required:   true,
			Confidence: 1.0,
			Freshness:  now,
		},
	}

	deduped := DeduplicateContextItems(items)
	if len(deduped) != 2 {
		t.Fatalf("expected 2 items after deduplication, got %d", len(deduped))
	}

	var foundSTM, foundLTM bool
	for _, item := range deduped {
		if item.ID == "stm-1" {
			foundSTM = true
		}
		if item.ID == "ltm-1" {
			foundLTM = true
		}
	}
	if !foundSTM || foundLTM {
		t.Errorf("expected stm-1 to win over duplicate ltm-1, got foundSTM=%v, foundLTM=%v", foundSTM, foundLTM)
	}
}

func TestContextCompiler_RankItems(t *testing.T) {
	items := []ContextItem{
		{ID: "history", Kind: "history", Priority: PriorityGeneralHistory},
		{ID: "goal", Kind: "user_goal", Priority: PriorityUserGoal, Required: true},
		{ID: "plan", Kind: "approved_plan", Priority: PriorityApprovedPlan},
		{ID: "stm", Kind: "stm", Priority: PriorityRecentSTM},
	}

	ranked := RankContextItems(items)
	if len(ranked) != 4 {
		t.Fatalf("expected 4 items, got %d", len(ranked))
	}
	if ranked[0].ID != "goal" {
		t.Errorf("rank 1 expected 'goal', got %q", ranked[0].ID)
	}
	if ranked[1].ID != "plan" {
		t.Errorf("rank 2 expected 'plan', got %q", ranked[1].ID)
	}
	if ranked[2].ID != "stm" {
		t.Errorf("rank 3 expected 'stm', got %q", ranked[2].ID)
	}
	if ranked[3].ID != "history" {
		t.Errorf("rank 4 expected 'history', got %q", ranked[3].ID)
	}
}

func TestContextCompiler_BudgetItems(t *testing.T) {
	items := []ContextItem{
		{ID: "goal", Kind: "user_goal", Content: "Required user goal", Priority: PriorityUserGoal, Required: true, TokenCount: 100},
		{ID: "policy", Kind: "hard_constraints", Content: "Hard policy constraints", Priority: PriorityHardConstraints, Required: true, TokenCount: 100},
		{ID: "stm", Kind: "stm", Content: "Recent short term memory findings", Priority: PriorityRecentSTM, TokenCount: 500},
		{ID: "history", Kind: "history", Content: "General conversation history", Priority: PriorityGeneralHistory, TokenCount: 1000},
	}

	budget := ContextBudget{
		Available: 300,
	}

	selected, overBudget, err := BudgetContextItems(items, budget)
	if err != nil {
		t.Fatalf("BudgetContextItems error: %v", err)
	}
	if !overBudget {
		t.Error("expected overBudget to be true when total tokens exceed budget")
	}

	if len(selected) != 2 {
		t.Fatalf("expected 2 required items preserved, got %d", len(selected))
	}
	if selected[0].ID != "goal" || selected[1].ID != "policy" {
		t.Errorf("expected required items [goal, policy], got [%s, %s]", selected[0].ID, selected[1].ID)
	}
}

func TestContextCompiler_EmptyRequiredItemFails(t *testing.T) {
	items := []ContextItem{
		{ID: "empty_req", Kind: "user_goal", Content: "   ", Required: true},
	}
	budget := ContextBudget{Available: 1000}
	ctx := context.Background()

	_, _, err := AssembleContextItemsPipeline(ctx, items, budget)
	if err == nil {
		t.Fatal("expected error for empty required context item, got nil")
	}
	if !strings.Contains(err.Error(), "missing or empty") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestContextCompiler_RejectsNormativeConflictAndStaleFragments(t *testing.T) {
	now := time.Now().UTC()
	conflicting := []ContextItem{
		{ID: "policy-a", Kind: "constraints", Content: "mode=allow", Authority: ContextAuthorityNormative, ConflictKey: "mode"},
		{ID: "policy-b", Kind: "constraints", Content: "mode=deny", Authority: ContextAuthorityNormative, ConflictKey: "mode"},
	}
	if err := ValidateContextItems(conflicting, now); err == nil || !strings.Contains(err.Error(), "normative context conflict") {
		t.Fatalf("normative conflict error = %v", err)
	}
	stale := []ContextItem{{ID: "old-fact", Kind: "stm", Content: "old", ExpiresAt: now.Add(-time.Second)}}
	if err := ValidateContextItems(stale, now); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale context error = %v", err)
	}
}

func TestContextCompiler_RendersAuthorityLabels(t *testing.T) {
	items := []ContextItem{
		{ID: "goal", Kind: "current_task", Content: "do current work", Priority: PriorityUserGoal, Required: true},
		{ID: "history", Kind: "stm", Content: "prior evidence", Priority: PriorityRecentSTM},
	}
	text, _, err := AssembleContextItemsPipeline(context.Background(), items, ContextBudget{Available: 100})
	if err != nil {
		t.Fatalf("AssembleContextItemsPipeline() error = %v", err)
	}
	for _, want := range []string{"authority=normative id=goal", "authority=historical id=history"} {
		if !strings.Contains(text, want) {
			t.Fatalf("compiled context missing %q:\n%s", want, text)
		}
	}
}

func TestCompileWorkerContextOmitsOptionalContextWhenItExceedsBudget(t *testing.T) {
	input := WorkerContextInput{
		Goal:   "required goal",
		RawSTM: "# 發現\n- " + strings.Repeat("historical evidence ", 2000),
		ModelContext: ModelContextSpec{
			ModelID: "test", ContextWindow: 500, MaxOutputTokens: 100, SafetyMarginTokens: 100,
		},
	}
	compiled, err := CompileWorkerContext(context.Background(), input)
	if err != nil || !compiled.OverBudget || len(compiled.OmittedItems) == 0 {
		t.Fatalf("CompileWorkerContext() compiled=%#v error=%v, want successful optional omission", compiled, err)
	}
}

func TestCompileCoordinatorContextOmitsOptionalContextWhenItExceedsBudget(t *testing.T) {
	input := CoordinatorContextInput{
		CorePrompt: "required coordinator contract",
		Goal:       "required goal",
		RawSTM:     "# historical\n- " + strings.Repeat("historical evidence ", 2000),
		ModelContext: ModelContextSpec{
			ModelID: "test", ContextWindow: 500, MaxOutputTokens: 100, SafetyMarginTokens: 100,
		},
	}
	compiled, err := CompileCoordinatorContext(context.Background(), input)
	if err != nil || !compiled.OverBudget {
		t.Fatalf("CompileCoordinatorContext() compiled=%#v error=%v, want successful optional compression/omission", compiled, err)
	}
}

func TestCompileWorkerContextUsesBoundEstimatorForRequiredContent(t *testing.T) {
	const modelID = "context-compiler-bound-worker"
	registerContextCompilerEstimatorTestModel(t, modelID)

	goal := strings.Repeat("bound worker required content ", 500)
	spec, globalTokens, boundTokens := boundContextCompilerSpec(t, modelID, "## Goal\n\n"+goal)
	compiled, err := CompileWorkerContext(t.Context(), WorkerContextInput{Goal: goal, ModelContext: spec})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v, want required content to fit with bound estimator", err)
	}
	if globalTokens <= boundTokens {
		t.Fatalf("global estimator tokens = %d, bound qwen tokens = %d; test is vacuous", globalTokens, boundTokens)
	}

	for _, item := range compiled.IncludedItems {
		if item.ID == "current_task" {
			if item.TokenCount != boundTokens {
				t.Fatalf("worker required content token count = %d, want bound qwen count %d", item.TokenCount, boundTokens)
			}
			if !strings.Contains(compiled.Prompt, strings.TrimSpace(goal)) {
				t.Fatalf("worker prompt omitted required goal")
			}
			return
		}
	}
	t.Fatalf("worker compiled context omitted required current_task: %#v", compiled)
}

func TestCompileCoordinatorContextUsesBoundEstimatorForRequiredContent(t *testing.T) {
	const modelID = "context-compiler-bound-coordinator"
	registerContextCompilerEstimatorTestModel(t, modelID)

	goal := strings.Repeat("bound coordinator required content ", 500)
	spec, globalTokens, boundTokens := boundContextCompilerSpec(t, modelID, "## Current Task\n\n"+goal)
	compiled, err := CompileCoordinatorContext(t.Context(), CoordinatorContextInput{Goal: goal, ModelContext: spec})
	if err != nil {
		t.Fatalf("CompileCoordinatorContext() error = %v, want required content to fit with bound estimator", err)
	}
	if globalTokens <= boundTokens {
		t.Fatalf("global estimator tokens = %d, bound qwen tokens = %d; test is vacuous", globalTokens, boundTokens)
	}

	for _, item := range compiled.IncludedItems {
		if item.ID == "current_task" {
			if item.TokenCount != boundTokens {
				t.Fatalf("coordinator required content token count = %d, want bound qwen count %d", item.TokenCount, boundTokens)
			}
			if !strings.Contains(compiled.Prompt, strings.TrimSpace(goal)) {
				t.Fatalf("coordinator prompt omitted required goal")
			}
			return
		}
	}
	t.Fatalf("coordinator compiled context omitted required current_task: %#v", compiled)
}

func TestCompileWorkerContextUnboundUsesGlobalEstimator(t *testing.T) {
	const modelID = "context-compiler-unbound"
	registerContextCompilerEstimatorTestModel(t, modelID)

	goal := strings.Repeat("unbound global estimator content ", 500)
	content := "## Goal\n\n" + goal
	globalSpec := GlobalModelSpecRegistry().GetSpec(modelID)
	globalTokens, err := defaultCounter.CountText(t.Context(), modelID, content)
	if err != nil {
		t.Fatalf("CountText() error = %v", err)
	}
	globalSpec.ContextWindow = globalTokens + globalSpec.MaxOutputTokens + globalSpec.SafetyMarginTokens
	compiled, err := CompileWorkerContext(t.Context(), WorkerContextInput{
		Goal: goal,
		ModelContext: ModelContextSpec{
			ModelID:            modelID,
			ContextWindow:      globalSpec.ContextWindow,
			MaxOutputTokens:    globalSpec.MaxOutputTokens,
			SafetyMarginTokens: globalSpec.SafetyMarginTokens,
		},
	})
	if err != nil {
		t.Fatalf("CompileWorkerContext() error = %v, want unbound global estimator compatibility", err)
	}

	for _, item := range compiled.IncludedItems {
		if item.ID == "current_task" {
			if item.TokenCount != globalTokens {
				t.Fatalf("unbound required content token count = %d, want global count %d", item.TokenCount, globalTokens)
			}
			return
		}
	}
	t.Fatalf("unbound compiled context omitted required current_task: %#v", compiled)
}

func registerContextCompilerEstimatorTestModel(t *testing.T, modelID string) {
	t.Helper()
	registry := GlobalModelSpecRegistry()
	previous := registry.GetSpec(modelID)
	t.Cleanup(func() { registry.RegisterSpec(previous) })
	registry.RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      128_000,
		MaxOutputTokens:    64,
		SafetyMarginTokens: 16,
		Estimator:          conservativeTokenEstimator,
	})
}

func boundContextCompilerSpec(t *testing.T, modelID, content string) (ModelContextSpec, int, int) {
	t.Helper()
	bound := agent.ProviderAdmissionContext{
		ModelID:            modelID,
		Bound:              true,
		ContextWindow:      1,
		MaxOutputTokens:    64,
		SafetyMarginTokens: 16,
		Estimator:          "qwen",
	}
	boundSpec := modelContextSpecForProviderRequest(agent.ProviderRequest{ModelID: modelID, AdmissionContext: bound})
	globalTokens, err := defaultCounter.CountText(t.Context(), modelID, content)
	if err != nil {
		t.Fatalf("global CountText() error = %v", err)
	}
	boundTokens := defaultCounter.countTextWithEstimator(content, boundSpec.Estimator)
	bound.ContextWindow = boundTokens + bound.MaxOutputTokens + bound.SafetyMarginTokens
	boundSpec = modelContextSpecForProviderRequest(agent.ProviderRequest{ModelID: modelID, AdmissionContext: bound})
	return boundSpec, globalTokens, boundTokens
}

func TestCompileWorkerContextBuildsTypedNormativeFragments(t *testing.T) {
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Goal: "ship router", Constraints: "do not leak secrets", ApprovedPlan: "1. implement", AgentInstructions: "use submit_result",
		VerificationCriteria: "go test ./...", RuntimeContext: "phase=VERIFY", SkillContext: []ContextItem{{ID: "skill:test", Kind: "skill", Content: "skill summary", Priority: PriorityAgentCoreInstructions, Required: true, Authority: ContextAuthorityNormative}},
		ModelContext: ModelContextSpec{ContextWindow: 4096, MaxOutputTokens: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"current_task": PriorityUserGoal, "task_constraints": PriorityHardConstraints, "approved_plan": PriorityApprovedPlan, "agent_instructions": PriorityAgentCoreInstructions, "verification_criteria": PriorityVerificationCriteria, "runtime_context": PriorityVerificationCriteria, "skill:test": PriorityAgentCoreInstructions}
	for _, item := range compiled.IncludedItems {
		if priority, ok := want[item.ID]; ok {
			if item.Priority != priority || !item.Required || normalizedContextAuthority(item) != ContextAuthorityNormative {
				t.Fatalf("typed item %s = %#v", item.ID, item)
			}
			delete(want, item.ID)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing typed fragments: %v", want)
	}
}

func TestCompileWorkerContextIncludesRetryFailureAsRequiredTypedFragment(t *testing.T) {
	request := validTestContextRequest()
	request.Attempt = 2
	request.Trigger = ContextTriggerRetry
	request.Failure = &ContextFailure{Class: FailureExecution, ErrorClass: string(FailureExecution)}
	request.AssignRequestID()
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Request: request, Goal: "repair the task", AgentInstructions: "execute safely",
		FailureContext: "## Retry Context\n\n**Failure class:** execution",
		ModelContext:   ModelContextSpec{ContextWindow: 4096, MaxOutputTokens: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range compiled.IncludedItems {
		if item.ID == "retry_failure_context" {
			found = true
			if !item.Required || item.Authority != ContextAuthorityNormative {
				t.Fatalf("retry fragment contract = %#v", item)
			}
		}
	}
	if !found || !strings.Contains(compiled.Prompt, "Failure class") {
		t.Fatalf("compiled retry context missing: %#v", compiled.IncludedItems)
	}
}

func TestContextCompiler_RequiredItemOverflow(t *testing.T) {
	items := []ContextItem{
		{ID: "req_huge", Kind: "user_goal", Content: strings.Repeat("A", 10000), Required: true, TokenCount: 2500},
	}
	budget := ContextBudget{Available: 100}

	_, overBudget, err := BudgetContextItems(items, budget)
	if err == nil {
		t.Fatal("expected budget overflow error when non-compressible required item exceeds budget, got nil")
	}
	if !overBudget {
		t.Error("expected overBudget to be true on overflow")
	}
}

func TestContextCompiler_Pipeline(t *testing.T) {
	items := []ContextItem{
		{
			ID:         "goal",
			Kind:       "user_goal",
			Content:    "## Goal\nImplement feature Y",
			Priority:   PriorityUserGoal,
			Required:   true,
			TokenCount: 10,
		},
		{
			ID:         "stm",
			Kind:       "stm",
			Content:    "## STM Findings\nFound bug in module Z",
			Priority:   PriorityRecentSTM,
			TokenCount: 15,
		},
		{
			ID:         "stm-dup",
			Kind:       "stm",
			Content:    "## STM Findings\nFound bug in module Z",
			Priority:   PriorityRecentSTM,
			TokenCount: 15,
		},
	}

	budget := ContextBudget{Available: 1000}
	ctx := context.Background()

	result, _, err := AssembleContextItemsPipeline(ctx, items, budget)
	if err != nil {
		t.Fatalf("AssembleContextItemsPipeline error: %v", err)
	}

	if !strings.Contains(result, "## Goal") || !strings.Contains(result, "Found bug in module Z") {
		t.Errorf("assembled context missing expected content: %q", result)
	}

	if count := strings.Count(result, "Found bug in module Z"); count != 1 {
		t.Errorf("expected duplicate string to appear exactly once, got %d times in:\n%s", count, result)
	}
}

func TestContextCompiler_FormatDependencyResults(t *testing.T) {
	results := []TaskResult{
		{
			TaskID:  "task-1",
			Agent:   "researcher",
			Status:  "done",
			Summary: "Researched API design",
			Details: "The typed deliverable is ready.",
			Artifacts: []ArtifactRef{
				{ID: "artifact-api-spec", SHA256: "digest-api-spec", Bytes: 42, Kind: "document", Type: "spec", Description: "OpenAPI specification"},
			},
			FilesModified: []FileRef{{Path: "api/router.go", Purpose: "implemented routing"}},
			Commands:      []CommandResult{{Command: "go test ./...", ExitCode: 0, Output: "RAW SHELL TRANSCRIPT"}},
			Verification:  []VerificationResult{{Command: "go test ./...", ExitCode: 0, Stdout: "RAW VERIFY STDOUT", Fingerprint: "verify-sha"}},
			ReceiptIDs:    []string{"receipt-1"},
			RawOutputRef:  &ArtifactRef{ID: "artifact-transcript", SHA256: "digest-transcript", Bytes: 99},
			OpenQuestions: OpenQuestions{"confirm rollout"},
			Decisions: []Decision{
				{Topic: "Protocol", Choice: "JSON-RPC", Reason: "Transport safety"},
			},
		},
	}

	formatted := FormatDependencyResults(results)
	if !strings.Contains(formatted, "## Task Dependency Results") {
		t.Errorf("formatted dependency results missing header: %q", formatted)
	}
	if !strings.Contains(formatted, "Task [task-1]") || !strings.Contains(formatted, "typed deliverable") {
		t.Errorf("formatted output missing typed task identity/details: %q", formatted)
	}
	for _, expected := range []string{"JSON-RPC", "api/router.go", "verify-sha", "receipt-1", "artifact-api-spec", "artifact-transcript", "confirm rollout"} {
		if !strings.Contains(formatted, expected) {
			t.Errorf("formatted output missing %q: %q", expected, formatted)
		}
	}
	for _, forbidden := range []string{"workspace/api_spec.json", "artifacts/transcript.cast"} {
		if strings.Contains(formatted, forbidden) {
			t.Errorf("formatted output leaked artifact path %q: %q", forbidden, formatted)
		}
	}
	for _, forbidden := range []string{"RAW SHELL TRANSCRIPT", "RAW VERIFY STDOUT"} {
		if strings.Contains(formatted, forbidden) {
			t.Errorf("formatted output leaked raw output %q: %q", forbidden, formatted)
		}
	}
}

func TestContextCompiler_CompileCoordinatorContext_Deduplication(t *testing.T) {
	ctx := context.Background()
	input := CoordinatorContextInput{
		Goal:         "Refactor compiler pipeline",
		RawSTM:       "# 發現\n- Shared rule: use go 1.26\n",
		RawLTM:       "# 發現\n- Shared rule: use go 1.26\n",
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		Role:         "coordinator",
	}

	compiled, err := CompileCoordinatorContext(ctx, input)
	if err != nil {
		t.Fatalf("CompileCoordinatorContext error: %v", err)
	}

	if count := strings.Count(compiled.Prompt, "Shared rule: use go 1.26"); count != 1 {
		t.Errorf("expected duplicated text across STM and LTM to appear exactly once, got %d in:\n%s", count, compiled.Prompt)
	}
}

func TestContextCompiler_CompileCoordinatorContext_MemoryRetention(t *testing.T) {
	ctx := context.Background()
	input := CoordinatorContextInput{
		Goal:         "Orchestrate batch execution",
		RawSTM:       "# 發現\n- Worker 1 initialized database schema\n",
		RawLTM:       "# 決策\n- Architecture: microservices architecture\n",
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		Role:         "coordinator",
	}

	compiled, err := CompileCoordinatorContext(ctx, input)
	if err != nil {
		t.Fatalf("CompileCoordinatorContext error: %v", err)
	}

	if !strings.Contains(compiled.Prompt, "Worker 1 initialized database schema") {
		t.Errorf("expected compiled coordinator context to retain STM, got:\n%s", compiled.Prompt)
	}
	if !strings.Contains(compiled.Prompt, "Architecture: microservices architecture") {
		t.Errorf("expected compiled coordinator context to retain LTM, got:\n%s", compiled.Prompt)
	}
}

func TestContextCompiler_CompileCoordinatorContext_RoleFiltering(t *testing.T) {
	ctx := context.Background()
	rawSTM := `# 發現
- Shared finding: database connection string postgres://localhost:5432

# 進度
- Completed step 1

# 決策
- Settled decision: use gRPC protocol
`

	// 1. Coordinator role sees all sections (Findings, Progress, Decisions)
	coordInput := CoordinatorContextInput{
		Goal:         "Orchestrate execution",
		RawSTM:       rawSTM,
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		Role:         "coordinator",
	}

	compiledCoord, err := CompileCoordinatorContext(ctx, coordInput)
	if err != nil {
		t.Fatalf("CompileCoordinatorContext error for coordinator: %v", err)
	}

	if !strings.Contains(compiledCoord.Prompt, "database connection string") {
		t.Errorf("expected coordinator context to include Findings, got:\n%s", compiledCoord.Prompt)
	}
	if !strings.Contains(compiledCoord.Prompt, "Completed step 1") {
		t.Errorf("expected coordinator context to include Progress, got:\n%s", compiledCoord.Prompt)
	}
	if !strings.Contains(compiledCoord.Prompt, "Settled decision: use gRPC protocol") {
		t.Errorf("expected coordinator context to include Decisions, got:\n%s", compiledCoord.Prompt)
	}

	// 2. Non-coordinator role (researcher) compiled via CompileCoordinatorContext
	// must filter out Progress section while retaining Findings and Decisions
	researcherCoordInput := CoordinatorContextInput{
		Goal:         "Orchestrate execution for researcher",
		RawSTM:       rawSTM,
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		Role:         "researcher",
	}

	compiledResearcherCoord, err := CompileCoordinatorContext(ctx, researcherCoordInput)
	if err != nil {
		t.Fatalf("CompileCoordinatorContext error for researcher role: %v", err)
	}

	if !strings.Contains(compiledResearcherCoord.Prompt, "database connection string") {
		t.Errorf("expected researcher coordinator context to include Findings, got:\n%s", compiledResearcherCoord.Prompt)
	}
	if !strings.Contains(compiledResearcherCoord.Prompt, "Settled decision: use gRPC protocol") {
		t.Errorf("expected researcher coordinator context to include Decisions, got:\n%s", compiledResearcherCoord.Prompt)
	}
	if strings.Contains(compiledResearcherCoord.Prompt, "Completed step 1") {
		t.Errorf("expected CompileCoordinatorContext with Role: researcher to filter out Progress section, got:\n%s", compiledResearcherCoord.Prompt)
	}

	// 3. CompileWorkerContext with researcher role
	workerInput := WorkerContextInput{
		Goal:         "Research task",
		TaskDef:      TaskDef{Agent: "researcher"},
		AgentDef:     &agent.AgentDef{Name: "researcher", Role: "researcher"},
		RawSTM:       rawSTM,
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		MaxAuxChars:  5000,
	}

	compiledWorker, err := CompileWorkerContext(ctx, workerInput)
	if err != nil {
		t.Fatalf("CompileWorkerContext error: %v", err)
	}

	if !strings.Contains(compiledWorker.Prompt, "database connection string") {
		t.Errorf("expected researcher worker context to include Findings, got:\n%s", compiledWorker.Prompt)
	}
	if strings.Contains(compiledWorker.Prompt, "Completed step 1") {
		t.Errorf("expected researcher worker context to filter out Progress section, got:\n%s", compiledWorker.Prompt)
	}
}

func TestContextCompiler_CompileWorkerContext_Deduplication(t *testing.T) {
	ctx := context.Background()
	input := WorkerContextInput{
		Goal:         "Implement worker context pipeline",
		TaskDef:      TaskDef{Agent: "worker"},
		AgentDef:     &agent.AgentDef{Name: "worker"},
		RawSTM:       "# 發現\n- Shared convention: use atomic file writes\n",
		RawLTM:       "# 發現\n- Shared convention: use atomic file writes\n",
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		MaxAuxChars:  2000,
	}

	compiled, err := CompileWorkerContext(ctx, input)
	if err != nil {
		t.Fatalf("CompileWorkerContext error: %v", err)
	}

	if count := strings.Count(compiled.Prompt, "Shared convention: use atomic file writes"); count != 1 {
		t.Errorf("expected duplicated text across STM and LTM to appear exactly once, got %d in:\n%s", count, compiled.Prompt)
	}
}

func TestContextCompiler_LongBaseWorkerPrompt_PreservesAuxiliaryContext(t *testing.T) {
	ctx := context.Background()
	longBasePrompt := strings.Repeat("Detailed task instructions and skill context block. ", 200) // ~10,000 chars

	input := WorkerContextInput{
		Goal:         longBasePrompt,
		TaskDef:      TaskDef{Agent: "worker"},
		AgentDef:     &agent.AgentDef{Name: "worker"},
		RawSTM:       "# 發現\n- Critical finding: database connection string is postgres://localhost:5432\n",
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		MaxAuxChars:  5000,
	}

	compiled, err := CompileWorkerContext(ctx, input)
	if err != nil {
		t.Fatalf("CompileWorkerContext error for long base prompt: %v", err)
	}

	if !strings.Contains(compiled.Prompt, "Critical finding: database connection string") {
		t.Errorf("expected long base worker prompt to preserve auxiliary STM context, but got:\n%s", compiled.Prompt)
	}
}

func TestContextCompiler_DependencyFiltering_ResolvedTodoIDs(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	// Add batch 1: Task 1 (independent completed)
	item1 := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "Task 1: Setup DB"}})[0]
	_ = c.taskTracker.TodoList().SetTypedResult(item1.ID, &TaskResult{TaskID: item1.ID, Summary: "DB Setup Complete"})
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(item1.ID, TaskDone, "", "")

	// Add batch 2: Task 2 (build API) and Task 3 (depends on Task 2, NOT Task 1)
	itemsBatch2 := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "Task 2: Build API"},
		{Agent: "worker", Desc: "Task 3: Run integration test"},
	})
	item2 := itemsBatch2[0]
	item3 := itemsBatch2[1]

	// Set item3's resolved DependsOn to item2.ID ("2"), NOT item1.ID ("1")
	item3.DependsOn = []string{item2.ID}

	// Complete Task 2
	_ = c.taskTracker.TodoList().SetTypedResult(item2.ID, &TaskResult{TaskID: item2.ID, Summary: "API Built Successfully"})
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(item2.ID, TaskDone, "", "")

	// Execute task dependency resolution for item3
	var depResults []TaskResult
	var currentTodo *TodoItem
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ID == item3.ID {
			currentTodo = item
			break
		}
	}
	if currentTodo != nil && len(currentTodo.DependsOn) > 0 {
		depSet := make(map[string]bool, len(currentTodo.DependsOn))
		for _, depID := range currentTodo.DependsOn {
			depSet[depID] = true
		}
		for _, item := range c.taskTracker.TodoList().Items() {
			if depSet[item.ID] && item.Status == TaskDone {
				if res := c.GetTaskResult(item.ID); res != nil {
					depResults = append(depResults, *res)
				}
			}
		}
	}

	if len(depResults) != 1 {
		t.Fatalf("expected exactly 1 dependency result for Task 3, got %d", len(depResults))
	}
	if depResults[0].Summary != "API Built Successfully" {
		t.Errorf("expected Task 2 result ('API Built Successfully'), got %q", depResults[0].Summary)
	}
}

func TestWorker_MemoryInjectedExactlyOnce(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	// Write an STM file
	stmContent := "# 發現\n- Shared rule: DB host is db.local\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "stm.md"), []byte(stmContent), 0644); err != nil {
		t.Fatalf("failed to write stm.md: %v", err)
	}

	workerDef := &agent.AgentDef{
		Name:   "developer",
		Role:   "worker",
		System: "You are a software developer.",
	}

	// 1. Verify injectWorkerContext system prompt DOES NOT contain STM memory
	injectedDef := c.injectWorkerContext(context.Background(), workerDef)
	if strings.Contains(injectedDef.System, "DB host is db.local") {
		t.Errorf("injectWorkerContext system prompt should NOT contain STM memory, but got:\n%s", injectedDef.System)
	}

	// 2. Verify CompileWorkerContext contains STM memory in task prompt
	workerInput := WorkerContextInput{
		Goal:         "Implement feature",
		TaskDef:      TaskDef{Agent: "developer"},
		AgentDef:     workerDef,
		RawSTM:       stmContent,
		ModelContext: ModelContextSpec{ContextWindow: 16384},
		MaxAuxChars:  5000,
	}
	compiled, err := CompileWorkerContext(context.Background(), workerInput)
	if err != nil {
		t.Fatalf("CompileWorkerContext failed: %v", err)
	}

	fullPrompt := injectedDef.System + "\n\n" + compiled.Prompt
	if count := strings.Count(fullPrompt, "DB host is db.local"); count != 1 {
		t.Errorf("expected memory fact 'DB host is db.local' to appear exactly once across system+task prompt, got %d in:\n%s", count, fullPrompt)
	}
}

func TestContextCompiler_CoordinatorIntegration(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config:    agent.TeamConfig{Name: "test-team", GoalMode: "exploratory"},
	}

	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	cc := c.ContextCompiler()
	if cc == nil {
		t.Fatal("expected non-nil ContextCompiler from Coordinator")
	}

	budget := cc.CalculateBudget(ModelContextSpec{ContextWindow: 16384}, 500, 200)
	if budget.Available <= 0 {
		t.Errorf("CalculateBudget returned invalid budget: %+v", budget)
	}

	suffix := cc.BuildMemorySuffix("worker")
	if suffix != "" {
		t.Errorf("BuildMemorySuffix on empty workspace = %q, want empty", suffix)
	}

	formattedDeps := cc.FormatDependencyResults(nil)
	if formattedDeps != "" {
		t.Errorf("FormatDependencyResults(nil) = %q, want empty", formattedDeps)
	}
}

func TestCompileWorkerContextUsesCanonicalBundleInsteadOfMarkdown(t *testing.T) {
	canonical := &CanonicalContextBundle{SharedSession: []contextstore.ContextItem{{
		ID: "ctx-verified", Kind: contextstore.ContextObservation, Content: "canonical-only finding",
		ContentHash: "canonical-hash", Lifecycle: contextstore.LifecycleConfirmed, Confidence: .9,
	}}}
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Goal: "use available context", RawSTM: "# 發現\n- stale markdown-only finding", CanonicalMemory: canonical,
		ModelContext: ModelContextSpec{ModelID: "test", ContextWindow: 4096, MaxOutputTokens: 512},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(compiled.Prompt, "canonical-only finding") || strings.Contains(compiled.Prompt, "stale markdown-only finding") {
		t.Fatalf("canonical bundle did not replace Markdown source: %s", compiled.Prompt)
	}
	found := false
	for _, item := range compiled.IncludedItems {
		if item.ID == "context:ctx-verified" && item.Revision == "ctx-verified" && len(item.Provenance) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("compiled context lost canonical identity: %#v", compiled.IncludedItems)
	}
}

func TestCompileWorkerContextVerifyOmitsRawAndGenericHistory(t *testing.T) {
	request := validTestContextRequest()
	request.Phase = PhaseVerify
	request.AssignRequestID()
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		Request: request, Goal: "verify artifact", VerificationCriteria: "go test ./...", RuntimeContext: "phase=VERIFY",
		RawSTM: "# Findings\nraw execution chatter\n", RawLTM: "# Long term\nexecute-only memory\n",
		ConcurrentTasks: "other worker transcript summary", WorkerMemory: &WorkerMemoryBundle{Items: []WorkerMemoryItem{{ContextItem: contextstore.ContextItem{Content: "private execute memory"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"raw execution chatter", "execute-only memory", "other worker transcript summary", "private execute memory"} {
		if strings.Contains(compiled.Prompt, forbidden) {
			t.Fatalf("VERIFY prompt leaked generic history %q: %s", forbidden, compiled.Prompt)
		}
	}
	for _, required := range []string{"verify artifact", "go test ./...", "phase=VERIFY"} {
		if !strings.Contains(compiled.Prompt, required) {
			t.Fatalf("VERIFY prompt missing required typed evidence %q: %s", required, compiled.Prompt)
		}
	}
}

func TestCanonicalMustKeepMapsToRequiredWithoutAuthorityEscalation(t *testing.T) {
	items := canonicalCompilerItems([]contextstore.ContextItem{{ID: "must", Content: "historical requirement", ContentHash: "hash", Lifecycle: contextstore.LifecycleConfirmed, MustKeep: true}}, PriorityRelevantLTM, "shared_persistent", false, false)
	if len(items) != 1 || !items[0].Required || items[0].Authority != ContextAuthorityHistorical {
		t.Fatalf("canonical must-keep mapping = %#v", items)
	}
}
