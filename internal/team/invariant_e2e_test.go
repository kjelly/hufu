package team

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/tools"
)

const (
	invariantE2EModelID   = "invariant-e2e-model"
	invariantE2EID        = "sqlite-canonical-memory"
	invariantE2EStatement = "context.sqlite is canonical; context-stm.md and context-ltm.md are disposable projections and never runtime inputs"
	invariantE2EViolation = `{"status":"success","summary":"review completed with an invariant finding","files_read":[{"path":"internal/team/shared_memory.go","purpose":"reviewed changed canonical-memory path"}],"findings":[{"summary":"Markdown projection used as runtime input","detail":"The changed path reads context-stm.md instead of context.sqlite.","severity":"error"}],"invariant_assessments":[{"invariant_id":"sqlite-canonical-memory","status":"violated","summary":"context-stm.md is used as a runtime input","finding_index":0}]}`
	invariantE2EPreserved = `{"status":"success","summary":"canonical memory invariant preserved","files_read":[{"path":"internal/team/shared_memory.go","purpose":"verified canonical-memory repair"}],"invariant_assessments":[{"invariant_id":"sqlite-canonical-memory","status":"preserved","summary":"runtime reads context.sqlite only"}]}`
)

type invariantE2EOutcome struct {
	coordinator *Coordinator
	todo        *TodoItem
	err         error
	requests    []string
}

func TestReportModeInvariantViolationCompletesReviewRunE2E(t *testing.T) {
	outcome := runInvariantProviderE2E(t, InvariantVerificationReport, 0, invariantE2EViolation)
	if outcome.err != nil {
		t.Fatalf("execute report-mode invariant review: %v", outcome.err)
	}
	assertInvariantE2EAssessment(t, outcome.todo, TaskDone, InvariantViolated)
	semantic := EvaluateSemanticRegression(outcome.coordinator.executionRunID, []*TodoItem{outcome.todo})
	if semantic.Configured || !semantic.Clear || semantic.BlockingCount != 0 {
		t.Fatalf("report-mode semantic decision = %#v, want clear/not configured", semantic)
	}
	assertInvariantE2ECompletion(t, outcome.coordinator, RunOutcomeCompleted)
	assertInvariantE2EProviderContext(t, outcome.requests)
}

func TestGateModeInvariantViolationBlocksTaskAndRunE2E(t *testing.T) {
	outcome := runInvariantProviderE2E(t, InvariantVerificationGate, 0, invariantE2EViolation)
	if outcome.err == nil {
		t.Fatal("gate-mode violation unexpectedly completed")
	}
	assertInvariantE2EAssessment(t, outcome.todo, TaskError, InvariantViolated)
	semantic := EvaluateSemanticRegression(outcome.coordinator.executionRunID, []*TodoItem{outcome.todo})
	if !semantic.Configured || semantic.Clear || semantic.BlockingCount != 1 {
		t.Fatalf("gate-mode semantic decision = %#v, want one blocker", semantic)
	}
	assertInvariantE2ECompletion(t, outcome.coordinator, RunOutcomePartial)
}

func TestGateModeInvariantPreservedCompletesRunE2E(t *testing.T) {
	outcome := runInvariantProviderE2E(t, InvariantVerificationGate, 0, invariantE2EPreserved)
	if outcome.err != nil {
		t.Fatalf("execute clean gate invariant review: %v", outcome.err)
	}
	assertInvariantE2EAssessment(t, outcome.todo, TaskDone, InvariantPreserved)
	semantic := EvaluateSemanticRegression(outcome.coordinator.executionRunID, []*TodoItem{outcome.todo})
	if !semantic.Configured || !semantic.Clear || semantic.BlockingCount != 0 {
		t.Fatalf("clean gate semantic decision = %#v, want clear/configured", semantic)
	}
	assertInvariantE2ECompletion(t, outcome.coordinator, RunOutcomeCompleted)
}

func TestGateModeInvariantRepairReplacesCurrentVerdictE2E(t *testing.T) {
	outcome := runInvariantProviderE2E(t, InvariantVerificationGate, 1, invariantE2EViolation, invariantE2EPreserved)
	if outcome.err != nil {
		t.Fatalf("execute repaired gate invariant review: %v", outcome.err)
	}
	assertInvariantE2EAssessment(t, outcome.todo, TaskDone, InvariantPreserved)
	semantic := EvaluateSemanticRegression(outcome.coordinator.executionRunID, []*TodoItem{outcome.todo})
	if !semantic.Configured || !semantic.Clear || semantic.BlockingCount != 0 {
		t.Fatalf("repaired gate semantic decision = %#v, want clear/configured", semantic)
	}
	if !hasHistoricalInvariantStatus(outcome.todo.ExecutionReceipts, InvariantViolated) {
		t.Fatalf("repaired gate lost historical violation receipt: %#v", outcome.todo.ExecutionReceipts)
	}
	assertInvariantE2ECompletion(t, outcome.coordinator, RunOutcomeCompleted)
}

func runInvariantProviderE2E(t *testing.T, mode InvariantVerificationMode, maxRetries int, submissions ...string) invariantE2EOutcome {
	t.Helper()
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: invariantE2EModelID, ContextWindow: 8192, MaxOutputTokens: 256, SafetyMarginTokens: 32,
	})

	var (
		requestMu       sync.Mutex
		requestBodies   []string
		submissionIndex int
	)
	provider := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requestMu.Lock()
		requestBodies = append(requestBodies, string(body))
		isToolContinuation := strings.Contains(string(body), `"role":"tool"`)
		arguments := ""
		if !isToolContinuation && submissionIndex < len(submissions) {
			arguments = submissions[submissionIndex]
			submissionIndex++
		}
		requestNumber := len(requestBodies)
		requestMu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if arguments != "" {
			fmt.Fprintf(w, "data: {\"id\":\"invariant-e2e\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", invariantE2EModelID, fmt.Sprintf("submit-%d", requestNumber), arguments)
			fmt.Fprint(w, "data: {\"id\":\"invariant-e2e\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"invariant-e2e\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"invariant-e2e\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"invariant-e2e\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(provider.Close)

	workspace := t.TempDir()
	contextRepo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = contextRepo.Close() })
	runID := "invariant-e2e-" + string(mode)
	store, err := NewEventStore(workspace, runID, runID+"-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "reviewer", Role: "worker", Tools: "view,grep,glob,ls", MaxRetries: maxRetries,
		Timeout: 10, Generation: agent.GenerationParams{Model: invariantE2EModelID, MaxTokens: "256"},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "hufu-code-review", Timeout: 10, MaxRetries: maxRetries,
			Generation: agent.GenerationParams{Model: invariantE2EModelID, MaxTokens: "256"},
		},
		Agents: map[string]*agent.AgentDef{"reviewer": worker},
		InvariantCatalog: []InvariantDefinition{{
			ID: invariantE2EID, Statement: invariantE2EStatement, Severity: InvariantSeverityError,
			AppliesTo: []string{"internal/context/", "internal/team/shared_memory.go", "internal/team/worker_memory.go"},
		}},
	}
	providerManager, err := agent.NewProviderManager(provider.URL, "", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: provider.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		session: session, projectDir: workspace, providerManager: providerManager,
		modelProfileRuntime: &ModelProfileRuntime{
			manager: providerManager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return auxiliaryProfileIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		coreTools:   agent.BuildAllAgentTools(workspace, tools.WithAllowedPaths([]string{workspace})),
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(),
		eventStore: store, contextRepo: contextRepo, executionRunID: runID, reportStatus: func(StatusEvent) {},
	}

	task := TaskDef{
		ContractID: "review-workset", Agent: "reviewer", Goal: "Review the canonical-memory workset item.", Phase: PhaseVerify,
		InvariantVerification: mode, Recovery: RecoveryRetry,
		Execution: ExecutionContract{RequiresResult: true, RequiresGroundedResult: true},
	}
	resolvedModel := coordinator.resolveAgentModel(worker, "")
	spec := TodoSpec{
		PlanTaskID: task.ContractID, Agent: task.Agent, Desc: task.Goal, Goal: task.Goal, Phase: task.Phase,
		InvariantVerification: task.InvariantVerification, Recovery: task.Recovery, Execution: task.Execution,
		Model: resolvedModel, ModelTopology: initialTaskModelTopology(worker, resolvedModel), Source: TaskSourceCoordinator,
	}
	ids := coordinator.taskTracker.TodoList().ReserveIDs(1)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
		t.Fatal(err)
	}
	items, err := coordinator.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatal(err)
	}
	_, executionErr := coordinator.executeTask(t.Context(), task, items[0].ID)
	current := todoItemByID(coordinator.taskTracker.TodoList().Items(), items[0].ID)
	requestMu.Lock()
	requests := slices.Clone(requestBodies)
	requestMu.Unlock()
	return invariantE2EOutcome{coordinator: coordinator, todo: current, err: executionErr, requests: requests}
}

func assertInvariantE2EAssessment(t *testing.T, todo *TodoItem, status TaskStatus, assessmentStatus InvariantAssessmentStatus) {
	t.Helper()
	if todo == nil || todo.Status != status || todo.TypedResult == nil || todo.TypedResult.InvariantVerification == nil {
		t.Fatalf("invariant E2E todo = %#v, want status %s with attestation", todo, status)
	}
	assessments := todo.TypedResult.InvariantVerification.Assessments
	if len(assessments) != 1 || assessments[0].InvariantID != invariantE2EID || assessments[0].Status != assessmentStatus || assessments[0].Severity != InvariantSeverityError {
		t.Fatalf("runtime-attested assessments = %#v", assessments)
	}
}

func assertInvariantE2ECompletion(t *testing.T, coordinator *Coordinator, want RunOutcome) {
	t.Helper()
	manifest := &EvidenceManifest{RunID: coordinator.executionRunID, Status: "accepted", EvidenceResults: []EvidenceResult{{RequirementID: "run:acceptance", Status: "passed"}}}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	coordinator.lastEvidenceManifest = manifest
	result := &RunResult{RunID: coordinator.executionRunID, Outcome: RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &AcceptanceResult{State: AcceptancePassed, Passed: true}}
	got := coordinator.applyCompletionGate(t.Context(), result, result.Acceptance)
	if got.Outcome != want || (want == RunOutcomeCompleted && !got.GoalSatisfied) || (want != RunOutcomeCompleted && got.GoalSatisfied) {
		t.Fatalf("completion outcome = %#v, want %s", got, want)
	}
}

func assertInvariantE2EProviderContext(t *testing.T, requests []string) {
	t.Helper()
	if len(requests) == 0 || !strings.Contains(requests[0], invariantE2EID) || !strings.Contains(requests[0], invariantE2EStatement) || !strings.Contains(requests[0], "invariant_assessments") {
		t.Fatalf("provider request omitted invariant context/contract: %v", requests)
	}
}

func hasHistoricalInvariantStatus(receipts []ExecutionReceipt, status InvariantAssessmentStatus) bool {
	for _, receipt := range receipts {
		if receipt.SubmittedResult == nil || receipt.SubmittedResult.InvariantVerification == nil {
			continue
		}
		for _, assessment := range receipt.SubmittedResult.InvariantVerification.Assessments {
			if assessment.Status == status {
				return true
			}
		}
	}
	return false
}
