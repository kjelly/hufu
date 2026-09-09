package team

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/tools"
)

// This file proves, through the REAL production dispatch path
// (SubagentRegistry().Resolve("hufu-local") -> HufuLocalSubagentProvider ->
// Coordinator.executeTask's own verification/classification logic), the
// exact TaskFailureClass a reviewer-shaped submission produces — the gap a
// prior review correctly flagged: hufu_coding_workflow_test.go's DAG-level
// tests fabricate FailureEvent.FailureClass directly rather than deriving it
// from a real submit_result call, so they cannot prove team.yaml's
// task_result_assert verify-spec actually produces the class the
// on-failure-classes gate expects. It reuses
// subagent_provider_baseline_test.go's fake-OpenAI-compatible-backend
// pattern (a real HTTP server, real SSE tool-call streaming, real Fantasy
// agent loop) rather than the workerAgentOverride test seam, which bypasses
// exactly the pipeline being verified here.

// classificationTestBackend answers every worker turn with a submit_result
// tool call carrying the given argsJSON, so a single fixture can drive both
// the "genuine finding" and "evidence gap" scenarios below.
type classificationTestBackend struct{ argsJSON string }

func (p *classificationTestBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"id\":\"cls\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-1\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", request.Model, p.argsJSON)
	fmt.Fprint(w, "data: {\"id\":\"cls\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// newClassificationTestCoordinator mirrors
// subagent_provider_baseline_test.go's newSubagentBaselineCoordinator: no
// workerAgentOverride, so executeTask resolves the attempt through the real
// hufu-local SubagentProvider exactly like production.
func newClassificationTestCoordinator(t *testing.T, argsJSON string) (*Coordinator, *agent.AgentDef) {
	t.Helper()
	modelID := "classification-test-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})
	server := newIPv4TestServer(t, &classificationTestBackend{argsJSON: argsJSON})
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	runID := "classification-test-run"
	store, err := NewEventStore(workspace, runID, "classification-test-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "reviewer", Role: "worker", Tools: "view", MaxRetries: 0, Timeout: 10,
		Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "classification-test", Timeout: 10, MaxRetries: 0,
			Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
		},
		Agents: map[string]*agent.AgentDef{"reviewer": worker},
	}
	providerManager, err := agent.NewProviderManager(server.URL, "", map[string]config.ProviderConfig{"ollama": {ProviderURL: server.URL}})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session: session, projectDir: workspace, providerManager: providerManager,
		modelProfileRuntime: &ModelProfileRuntime{
			manager: providerManager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return auxiliaryProfileIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		coreTools:   agent.BuildAllAgentTools(workspace, tools.WithAllowedPaths([]string{workspace})),
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(),
		eventStore: store, executionRunID: runID, reportStatus: func(StatusEvent) {},
	}
	return c, worker
}

func admitClassificationTestTask(t *testing.T, c *Coordinator, worker *agent.AgentDef, verify *VerificationSpec) *TodoItem {
	t.Helper()
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	resolvedModel := c.resolveAgentModel(worker, "")
	canonical, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: worker.Name}, worker, resolvedModel)
	if err != nil {
		t.Fatalf("canonicalizeTaskOccurrence: %v", err)
	}
	spec := TodoSpec{
		Agent: worker.Name, Desc: "REVIEW_CODE: review the change", Goal: "REVIEW_CODE: review the change",
		Model: canonical.Model, ModelTopology: initialTaskModelTopology(worker, canonical.Model),
		ExecutionTarget: canonical.ResolvedExecutionTarget, ExecutionTopology: canonical.ExecutionTopology,
		Source: TaskSourceCoordinator, Recovery: RecoveryRetry,
		Execution:        ExecutionContract{RequiresResult: true},
		VerifySpec:       verify,
		SubagentProvider: canonical.SubagentProvider,
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := c.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	return items[0]
}

// TestHufuCodingReviewerCleanReviewReachesTaskDone confirms the baseline
// positive case: status:success reaches TaskDone with no verify-spec
// involved at all (hufu-coding's semantic roles ship with none — see
// TestHufuCodingNoVerifySpecOnSemanticRoles).
func TestHufuCodingReviewerCleanReviewReachesTaskDone(t *testing.T) {
	c, worker := newClassificationTestCoordinator(t, `{"status":"success","summary":"no issues found"}`)
	item := admitClassificationTestTask(t, c, worker, nil)

	task := TaskDef{Agent: worker.Name, Goal: item.Goal, Execution: ExecutionContract{RequiresResult: true}}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if got := c.todoItemByID(item.ID).Status; got != TaskDone {
		t.Fatalf("status = %s, want done for a clean review", got)
	}
}

// TestHufuCodingGenuineFailedReportAuthorizesReset is the direct regression
// for a prior review finding: a task_result_assert verify-spec on a fact
// like `must_fix_found` enforces at submit_result *admission* time
// (task_result_contract.go) and would reject the tool call itself for an
// honest, confirmed finding — making a true positive literally
// un-submittable. hufu-coding's reviewer contract instead reports the
// verdict directly via `status: failed` (reviewer.md), which is fully
// admission-accepted (only success/completed_with_gaps are ever gated), and
// stores a genuine TypedResult. This proves that real, end-to-end submission
// is exactly what dagScheduler's isGenuineWorkerReportedFailure (the runtime
// gate that authorizes the on_failure edge for a class the task's
// on-failure-classes allowlist does not itself list) requires.
func TestHufuCodingGenuineFailedReportAuthorizesReset(t *testing.T) {
	c, worker := newClassificationTestCoordinator(t, `{"status":"failed","summary":"found a concrete bug","findings":[{"category":"correctness","summary":"unchecked error at handler.go:42"}]}`)
	item := admitClassificationTestTask(t, c, worker, nil)

	task := TaskDef{Agent: worker.Name, Goal: item.Goal, Execution: ExecutionContract{RequiresResult: true}}
	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
		t.Fatal("expected executeTask to fail: status:failed must never reach TaskDone")
	}
	got := c.todoItemByID(item.ID)
	if got.TypedResult == nil || got.TypedResult.Status != TaskResultStatusFailed {
		t.Fatalf("expected a stored TypedResult with status failed, got %+v", got.TypedResult)
	}
	if len(got.TypedResult.Findings) != 1 {
		t.Fatalf("expected the finding to be preserved on the stored result, got %+v", got.TypedResult.Findings)
	}
	if !isGenuineWorkerReportedFailure(got) {
		t.Fatalf("isGenuineWorkerReportedFailure = false, want true for a complete status:failed self-report: %+v", got.TypedResult)
	}
}

// TestHufuCodingEvidenceGapDoesNotAuthorizeReset proves the contrasting
// honest-but-inconclusive case: status:partial (reviewer.md's instruction
// for an evidence gap) stores no confirmed-failed TypedResult, so
// isGenuineWorkerReportedFailure correctly refuses to authorize a reset —
// spec.md §10.1's "evidence incomplete... must not automatically send the
// coder through a rewrite loop."
func TestHufuCodingEvidenceGapDoesNotAuthorizeReset(t *testing.T) {
	c, worker := newClassificationTestCoordinator(t, `{"status":"partial","summary":"one cited file could not be read"}`)
	item := admitClassificationTestTask(t, c, worker, nil)

	task := TaskDef{Agent: worker.Name, Goal: item.Goal, Execution: ExecutionContract{RequiresResult: true}}
	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
		t.Fatal("expected executeTask to fail: status:partial must never reach TaskDone")
	}
	got := c.todoItemByID(item.ID)
	if isGenuineWorkerReportedFailure(got) {
		t.Fatalf("isGenuineWorkerReportedFailure = true, want false for an evidence gap (status:partial), TypedResult=%+v", got.TypedResult)
	}
}
