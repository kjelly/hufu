package team

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/tools"
)

// This file freezes the current hufu-local SubagentProvider semantics before
// any external-provider work begins (spec.md §36 Phase 0 / PR-00). Every test
// here drives the real production dispatch path — SubagentRegistry().Resolve
// → HufuLocalSubagentProvider.RunAttempt via Coordinator.executeTask — rather
// than the c.workerAgentOverride test seam used throughout the rest of the
// suite. That seam bypasses the provider abstraction entirely, so it cannot
// stand in as a parity baseline for a future external (e.g. Codex) provider.
//
// Three of the four PR-00-named baselines already exist elsewhere under
// different names and are deliberately not duplicated here:
//   - TestUnknownSubagentProviderFailsClosed pins the resolution error itself
//     via TestSubagentRegistryFailsClosedForUnknownProvider
//     (subagent_registry_test.go). This file adds the complementary
//     "before any side effect" proof.
//   - TestProviderCannotAcceptTask pins the DTO-shape half via
//     TestSubagentProviderCannotDeclareAcceptance (subagent_registry_test.go).
//     This file adds the end-to-end pipeline half.
//   - TestDurableTaskModelDoesNotRetargetOnResume pins the resume-after-crash
//     path via TestRestoredTodoOccurrenceKeepsCanonicalModelWithoutJournal
//     (task_occurrence_durability_test.go). This file adds the same-run
//     retry-loop path, which is a separate call site
//     (coordinator_task_run.go's frozenTaskOccurrenceModel check vs.
//     resolveTaskExecutionModel).
//
// TestLocalSubagentProviderParity has no prior analog: it is the actual new
// baseline this phase exists to establish.

// subagentBaselineBackend is a fake OpenAI-compatible chat backend. It always
// answers a worker turn with a submit_result success call.
type subagentBaselineBackend struct {
	mu       sync.Mutex
	requests int
	models   []string
	tools    [][]string
}

func (p *subagentBaselineBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model string `json:"model"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Function.Name)
	}

	p.mu.Lock()
	p.requests++
	requestNumber := p.requests
	p.models = append(p.models, request.Model)
	p.tools = append(p.tools, names)
	p.mu.Unlock()

	// The summary varies by request so a genuine retry (different attempt,
	// same claimed success) is not misclassified by anti-thrashing as the
	// worker repeating an identical failing command.
	arguments := fmt.Sprintf(`{"status":"success","summary":"baseline worker result (request %d)"}`, requestNumber)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"id\":\"baseline\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-%d\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", request.Model, requestNumber, arguments)
	fmt.Fprint(w, "data: {\"id\":\"baseline\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (p *subagentBaselineBackend) snapshot() (int, []string, [][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests, append([]string(nil), p.models...), append([][]string(nil), p.tools...)
}

// newSubagentBaselineCoordinator builds a Coordinator with no
// workerAgentOverride, so executeTask must resolve the worker attempt through
// SubagentRegistry().Resolve(hufu-local) exactly as production dispatch does
// (coordinator_task_run.go).
func newSubagentBaselineCoordinator(t *testing.T, modelID string) (*Coordinator, *agent.AgentDef, *subagentBaselineBackend) {
	t.Helper()
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})
	backend := &subagentBaselineBackend{}
	server := newIPv4TestServer(t, backend)
	t.Cleanup(server.Close)

	workspace := t.TempDir()
	runID := "subagent-baseline-" + modelID
	store, err := NewEventStore(workspace, runID, "subagent-baseline-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", Tools: "view", MaxRetries: 0, Timeout: 10,
		Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "subagent-baseline", Timeout: 10, MaxRetries: 0,
			Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
		},
		Agents: map[string]*agent.AgentDef{"worker": worker},
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
	return c, worker, backend
}

// admitSubagentBaselineTask durably admits a task the same way production
// admission does, so the resulting Todo is a genuine "durable task
// occurrence" (isDurableTaskOccurrence) rather than an ephemeral in-memory one.
func admitSubagentBaselineTask(t *testing.T, c *Coordinator, worker *agent.AgentDef, verify *VerificationSpec) *TodoItem {
	t.Helper()
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	resolvedModel := c.resolveAgentModel(worker, "")
	provider, err := c.resolveSubagentProvider(TaskDef{Agent: worker.Name}, worker)
	if err != nil {
		t.Fatalf("resolveSubagentProvider: %v", err)
	}
	spec := TodoSpec{
		Agent: worker.Name, Desc: "baseline worker task", Goal: "baseline worker task",
		Model: resolvedModel, ModelTopology: initialTaskModelTopology(worker, resolvedModel),
		Source: TaskSourceCoordinator, Recovery: RecoveryRetry,
		Execution:        ExecutionContract{RequiresResult: true},
		VerifySpec:       verify,
		SubagentProvider: provider,
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

// TestLocalSubagentProviderParity records the current, normalized baseline for
// a durable task dispatched through the real hufu-local SubagentProvider path:
// result, receipt, verification and completion all land the way the rest of
// this specification assumes, and the live state replays identically from the
// event log. Any future provider (Codex included) is judged against this
// same shape (spec.md §36 Phase 0 / PR-00, §37 mandatory test matrix).
func TestLocalSubagentProviderParity(t *testing.T) {
	modelID := "subagent-baseline-model-parity"
	c, worker, backend := newSubagentBaselineCoordinator(t, modelID)
	verify := &VerificationSpec{Type: VerifyCommandExit, Command: "true"}
	item := admitSubagentBaselineTask(t, c, worker, verify)
	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task",
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}

	if _, err := c.executeTask(oversizedCoordinatorPreflightContext(t, modelID), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}

	requests, models, toolNames := backend.snapshot()
	if requests != 1 {
		t.Fatalf("provider requests = %d, want exactly 1 for a first-attempt success", requests)
	}
	if models[0] != modelID {
		t.Fatalf("provider-visible model = %q, want %q (provider identity is not the same as model identity)", models[0], modelID)
	}
	if !containsToolName(toolNames[0], submitResultToolName) {
		t.Fatalf("provider-visible tools = %v, missing %q", toolNames[0], submitResultToolName)
	}

	got := c.todoItemByID(item.ID)
	if got == nil {
		t.Fatal("task disappeared after execution")
	}

	// result
	if got.TypedResult == nil || got.TypedResult.Status != TaskResultStatusSuccess || got.TypedResult.Source != "submitted" {
		t.Fatalf("typed result = %#v, want a submitted success result", got.TypedResult)
	}

	// completion
	if got.Status != TaskDone {
		t.Fatalf("task status = %s, want done", got.Status)
	}

	// receipt
	if got.ExecutionReceipt == nil {
		t.Fatal("execution receipt missing")
	}
	receipt := got.ExecutionReceipt
	if receipt.RunID != c.executionRunID || receipt.TaskID != item.ID || receipt.Attempt != 1 || receipt.ProducerID == "" {
		t.Fatalf("execution receipt = %#v, want populated Hufu-owned run/task/attempt/producer identity", receipt)
	}

	// verification
	if got.VerifyResult == nil || got.VerifyResult.ExitCode != 0 || got.VerifyResult.Command != "true" {
		t.Fatalf("verify result = %#v, want a recorded passing verification", got.VerifyResult)
	}

	// replay
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 {
		t.Fatalf("replayed task count = %d, want 1", len(replayed))
	}
	if replayed[0].Status != TaskDone || replayed[0].ExecutionReceipt == nil {
		t.Fatalf("replayed state = %#v, want done with a receipt matching live state", replayed[0])
	}
	if replayed[0].ExecutionReceipt.TaskID != receipt.TaskID || replayed[0].ExecutionReceipt.Attempt != receipt.Attempt {
		t.Fatalf("replayed receipt = %#v, want it to match the live receipt %#v", replayed[0].ExecutionReceipt, receipt)
	}
}

// TestProviderCannotAcceptTask proves INV-02 end to end: a provider's own
// claim of "success" does not transition the Todo to done. Only Hufu-owned
// verification governs task acceptance (spec.md §2.4, §9.1, §21, §44 stop
// condition 15). TestSubagentProviderCannotDeclareAcceptance
// (subagent_registry_test.go) already proves this at the DTO-shape level
// (AttemptResult has no acceptance field to forge); this test proves it at
// the pipeline level (a structurally valid success proposal still loses to a
// failing Hufu verification command).
func TestProviderCannotAcceptTask(t *testing.T) {
	modelID := "subagent-baseline-model-veto"
	c, worker, backend := newSubagentBaselineCoordinator(t, modelID)
	verify := &VerificationSpec{Type: VerifyCommandExit, Command: "false"}
	item := admitSubagentBaselineTask(t, c, worker, verify)
	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task",
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}

	_, err := c.executeTask(oversizedCoordinatorPreflightContext(t, modelID), task, item.ID)
	if err == nil {
		t.Fatal("expected executeTask to report failure when Hufu verification rejects a provider success claim")
	}

	if requests, _, _ := backend.snapshot(); requests != 1 {
		t.Fatalf("provider requests = %d, want exactly 1 (no retry budget on this agent)", requests)
	}

	got := c.todoItemByID(item.ID)
	if got == nil {
		t.Fatal("task disappeared after execution")
	}
	if got.Status == TaskDone {
		t.Fatalf("task status = done despite failed Hufu verification; a provider success claim must not bypass verification")
	}
	if got.VerifyResult == nil || got.VerifyResult.ExitCode == 0 {
		t.Fatalf("verify result = %#v, want a recorded verification failure overriding the provider's claimed success", got.VerifyResult)
	}
}

// callTrackingSubagentProvider records RunAttempt invocations so a test can
// prove the *absence* of a call, not just a returned error.
type callTrackingSubagentProvider struct {
	name  string
	calls int
}

func (p *callTrackingSubagentProvider) Name() string { return p.name }
func (p *callTrackingSubagentProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{}
}
func (p *callTrackingSubagentProvider) RunAttempt(context.Context, AttemptRequest) (AttemptResult, error) {
	p.calls++
	return AttemptResult{}, nil
}

// TestUnknownSubagentProviderFailsClosed complements
// TestSubagentRegistryFailsClosedForUnknownProvider (subagent_registry_test.go),
// which pins the resolution error string. This test adds the "before any side
// effect" half of INV-10/spec.md §1.2: resolving an unknown provider must not
// silently fall back to a different registered provider's RunAttempt.
func TestUnknownSubagentProviderFailsClosed(t *testing.T) {
	known := &callTrackingSubagentProvider{name: localSubagentProviderName}
	registry := NewSubagentRegistry(known)

	provider, err := registry.Resolve("codex")
	if err == nil || !strings.Contains(err.Error(), "unknown subagent provider") {
		t.Fatalf(`Resolve("codex") error = %v, want a fail-closed unknown-provider error`, err)
	}
	if provider != nil {
		t.Fatalf(`Resolve("codex") returned a provider = %#v, want nil`, provider)
	}
	if known.calls != 0 {
		t.Fatalf("registered provider RunAttempt calls = %d, want 0 (unknown-provider resolution must precede any side effect)", known.calls)
	}
}

// TestDurableTaskModelDoesNotRetargetOnResume proves INV-04's model-pinning
// half for the in-run retry loop specifically: coordinator_task_run.go's
// executeTask checks frozenTaskOccurrenceModel inline, ahead of escalation,
// before ever resolving a retry's model. This is a distinct call site from
// resolveTaskExecutionModel, which
// TestRestoredTodoOccurrenceKeepsCanonicalModelWithoutJournal
// (task_occurrence_durability_test.go) already covers for the
// resume-after-crash/repair path — no existing test exercised
// frozenTaskOccurrenceModel directly. Driving this through a full two-attempt
// HTTP round trip would additionally exercise fantasy's own tool-loop
// mechanics, which is orthogonal to the identity guarantee this test exists
// to pin; calling the primitive directly keeps the test deterministic and
// fast, matching this package's convention of testing coordinator logic
// against a bare struct without mocking an LLM (escalation_test.go).
func TestDurableTaskModelDoesNotRetargetOnResume(t *testing.T) {
	const frozenModel = "subagent-baseline-model-frozen"
	const strongerModel = "subagent-baseline-model-stronger"
	c, worker, _ := newSubagentBaselineCoordinator(t, frozenModel)
	item := admitSubagentBaselineTask(t, c, worker, nil)

	// Simulate configuration changing after admission: a stronger model
	// becomes available in the team's model list. A durable occurrence must
	// ignore it — prove the list really would have picked it if consulted.
	c.modelList = []config.ModelEntry{{ID: frozenModel}, {ID: strongerModel}}
	if got := nextStrongerModel(c.modelList, frozenModel); got != strongerModel {
		t.Fatalf("nextStrongerModel(%q) = %q, want %q (test setup did not actually offer a stronger model)", frozenModel, got, strongerModel)
	}

	got, frozen := c.frozenTaskOccurrenceModel(item.ID)
	if !frozen || got != frozenModel {
		t.Fatalf("frozenTaskOccurrenceModel(durable) = (%q, %v), want (%q, true) despite a stronger model being available", got, frozen, frozenModel)
	}

	// Contrast: an ephemeral (non-admitted) Todo must NOT freeze, so the
	// normal live-config resolution path still applies to it.
	ephemeral := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: worker.Name, Desc: "ephemeral", Model: frozenModel}})[0]
	if _, frozen := c.frozenTaskOccurrenceModel(ephemeral.ID); frozen {
		t.Fatalf("frozenTaskOccurrenceModel(ephemeral) reported frozen, want the live/ephemeral occurrence to remain retargetable")
	}
}

func containsToolName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}
