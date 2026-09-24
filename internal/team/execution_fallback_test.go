package team

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/tools"
)

func TestClassifyProviderError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want ProviderFailureClass
	}{
		{name: "429", err: &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}, want: ProviderRateLimited},
		{name: "wrapped 503", err: fmt.Errorf("stream: %w", &fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable}), want: ProviderUnavailable},
		{name: "500", err: &fantasy.ProviderError{StatusCode: http.StatusInternalServerError}, want: ProviderUnavailable},
		{name: "404", err: &fantasy.ProviderError{StatusCode: http.StatusNotFound}, want: ProviderModelUnavailable},
		{name: "400 with an OpenAI model_not_found body", err: &fantasy.ProviderError{StatusCode: http.StatusBadRequest, ResponseBody: []byte(`{"error":{"code":"model_not_found"}}`)}, want: ProviderModelUnavailable},
		{name: "400 with an Ollama not-found body", err: &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Message: `model "x" not found, try pulling it first`}, want: ProviderModelUnavailable},
		{name: "plain 400", err: &fantasy.ProviderError{StatusCode: http.StatusBadRequest, Message: "bad tool schema"}, want: ProviderOther},
		{name: "401", err: &fantasy.ProviderError{StatusCode: http.StatusUnauthorized}, want: ProviderAuthFailed},
		{name: "403", err: &fantasy.ProviderError{StatusCode: http.StatusForbidden}, want: ProviderAuthFailed},
		{name: "auth refresh flag", err: &fantasy.ProviderError{AuthError: true}, want: ProviderAuthFailed},
		{name: "context too large", err: &fantasy.ProviderError{StatusCode: http.StatusBadRequest, ContextTooLargeErr: true}, want: ProviderContextExceeded},
		{name: "connection refused", err: &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, want: ProviderUnavailable},
		{name: "connection reset", err: fmt.Errorf("read: %w", syscall.ECONNRESET), want: ProviderUnavailable},
		{name: "dns", err: &net.DNSError{Err: "no such host", Name: "llm.invalid"}, want: ProviderUnavailable},
		{name: "tls alert", err: tls.AlertError(40), want: ProviderUnavailable},
		{name: "transport deadline", err: fmt.Errorf("post: %w", context.DeadlineExceeded), want: ProviderTransportTimeout},
		{name: "not a provider error", err: errors.New("deliverable verification failed"), want: ProviderOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyProviderError(tt.err); got != tt.want {
				t.Fatalf("ClassifyProviderError = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExecutionFallbackDecision(t *testing.T) {
	route := func(candidates int) *ExecutionRouteBinding {
		binding := &ExecutionRouteBinding{Name: "coding", FallbackOn: []ProviderFailureClass{ProviderRateLimited, ProviderUnavailable, ProviderModelUnavailable, ProviderTransportTimeout}}
		for i := range candidates {
			binding.Candidates = append(binding.Candidates, execution.ExecutionTarget{Backend: "ollama", Model: fmt.Sprintf("m%d", i)})
		}
		return binding
	}
	rateLimited := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}
	recorded := func(calls ...executedToolCall) *executedToolCallRecorder {
		return &executedToolCallRecorder{calls: calls}
	}
	readOnly := []executedToolCall{{name: "view", input: `{"file_path":"a.go"}`}, {name: "bash", input: `{"command":"git status"}`}}
	bashWrite := executedToolCall{name: "bash", input: `{"command":"echo x > f"}`}
	tests := []struct {
		name     string
		route    *ExecutionRouteBinding
		in       executionFallbackInput
		fallback bool
		denied   string
	}{
		{name: "read-only side effect falls back", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectNone}, fallback: true},
		{name: "shared write with only read-only calls falls back", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectWorkspaceWrite, Recorder: recorded(readOnly...)}, fallback: true},
		{name: "shared write after a bash write does not", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectWorkspaceWrite, Recorder: recorded(append(readOnly, bashWrite)...)}, denied: fallbackDeniedSideEffects},
		{name: "shared write without a record does not", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectWorkspaceWrite}, denied: fallbackDeniedCallsUnrecorded},
		{name: "isolated write falls back after writes", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectWorkspaceWrite, Isolated: true, Recorder: recorded(bashWrite)}, fallback: true},
		{name: "external write after any call does not", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectExternalWrite, Recorder: recorded(readOnly[0])}, denied: fallbackDeniedSideEffects},
		{name: "external write with no call falls back", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectExternalWrite, Recorder: recorded()}, fallback: true},
		{name: "auth failure is never eligible", route: route(2), in: executionFallbackInput{Err: &fantasy.ProviderError{StatusCode: http.StatusUnauthorized}, SideEffect: SideEffectNone}, denied: fallbackDeniedNotInFallbackOn},
		{name: "context too large is never eligible", route: route(2), in: executionFallbackInput{Err: &fantasy.ProviderError{ContextTooLargeErr: true}, SideEffect: SideEffectNone}, denied: fallbackDeniedNotInFallbackOn},
		{name: "a verification failure is not a provider failure", route: route(2), in: executionFallbackInput{Err: errors.New("deliverable verification failed: exit 1"), SideEffect: SideEffectNone}},
		{name: "a semantic rejection is not a provider failure", route: route(2), in: executionFallbackInput{Err: withFailureClassOverride(errors.New("worker reported failed"), FailureExecution), SideEffect: SideEffectNone}},
		{name: "a task timeout is not a provider failure", route: route(2), in: executionFallbackInput{Err: context.DeadlineExceeded, AttemptContextErr: context.DeadlineExceeded, SideEffect: SideEffectNone}},
		{name: "cancellation does not fall back", route: route(2), in: executionFallbackInput{Err: rateLimited, ParentContextErr: context.Canceled, SideEffect: SideEffectNone}, denied: fallbackDeniedCancelled},
		{name: "no next candidate", route: route(1), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectNone}},
		{name: "budget exhausted", route: route(2), in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectNone, BudgetExceeded: true}, denied: fallbackDeniedBudgetExhausted},
		{name: "no route", in: executionFallbackInput{Err: rateLimited, SideEffect: SideEffectNone}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newExecutionFallbackState(tt.route)
			state.beginAttempt(1)
			_, fellBack, denied := state.decide(tt.in)
			if fellBack != tt.fallback || denied != tt.denied {
				t.Fatalf("decide = fallback %v denied %q, want %v %q", fellBack, denied, tt.fallback, tt.denied)
			}
		})
	}
}

func TestExecutionFallbackStateReturnsToPrimaryOnRetry(t *testing.T) {
	route := &ExecutionRouteBinding{Name: "coding", FallbackOn: []ProviderFailureClass{ProviderRateLimited}, Candidates: []execution.ExecutionTarget{
		{Backend: "ollama", Model: "a"}, {Backend: "ollama", Model: "b"}, {Backend: "ollama", Model: "c"},
	}}
	state := newExecutionFallbackState(route)
	rateLimited := &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}
	primary := route.Candidates[0]
	steps := []struct {
		attempt  int
		want     string
		fallback bool
		fail     error
	}{
		{attempt: 1, want: "a", fail: rateLimited},
		{attempt: 2, want: "b", fallback: true, fail: errors.New("verification failed")},
		{attempt: 3, want: "a", fail: rateLimited}, // a normal retry returns to the primary
		{attempt: 4, want: "b", fallback: true, fail: rateLimited},
		{attempt: 5, want: "a"}, // two fallbacks used: none left, so the retry restarts at the primary
	}
	for _, step := range steps {
		fallback := state.beginAttempt(step.attempt)
		if got := state.target(primary).Model; got != step.want || fallback != step.fallback {
			t.Fatalf("attempt %d runs %s (fallback %v), want %s (fallback %v)", step.attempt, got, fallback, step.want, step.fallback)
		}
		if state.beginAttempt(step.attempt) != fallback || state.target(primary).Model != step.want {
			t.Fatalf("resuming attempt %d changed its candidate", step.attempt)
		}
		if step.fail != nil {
			state.decide(executionFallbackInput{Err: step.fail, SideEffect: SideEffectNone})
		}
	}
	if state.used != 2 {
		t.Fatalf("fallbacks used = %d, want the route's len-1 bound of 2", state.used)
	}
}

// fallbackTestServer is a fake OpenAI-compatible backend. It answers by the
// requested model: a model in failures gets that HTTP status (after first
// calling toolFirst's tool, when one is listed); any other model submits a
// successful result.
type fallbackTestServer struct {
	mu        sync.Mutex
	models    []string
	failures  map[string]int
	toolFirst map[string][2]string
	calls     map[string]int
}

func (s *fallbackTestServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	s.mu.Lock()
	if s.calls == nil {
		s.calls = make(map[string]int)
	}
	s.calls[request.Model]++
	call := s.calls[request.Model]
	s.models = append(s.models, request.Model)
	tool, hasTool := s.toolFirst[request.Model]
	status, fails := s.failures[request.Model]
	s.mu.Unlock()
	if hasTool && call == 1 {
		streamToolCall(w, request.Model, tool[0], tool[1])
		return
	}
	if fails {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"injected %d for %s","type":"test_error"}}`, status, request.Model)
		return
	}
	streamToolCall(w, request.Model, submitResultToolName, `{"status":"success","summary":"done after fallback"}`)
}

func (s *fallbackTestServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.models...)
}

func streamToolCall(w http.ResponseWriter, model, name, args string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"fb\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-%s\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", model, name, name, args)
	_, _ = fmt.Fprint(w, "data: {\"id\":\"fb\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

type fallbackFixture struct {
	c      *Coordinator
	store  *EventStore
	item   *TodoItem
	worker *agent.AgentDef
}

// newFallbackFixture drives the real hufu-local pipeline (no worker
// override) against fake backends, with the worker bound to a route.
func newFallbackFixture(t *testing.T, providers map[string]string, candidates []string, fallbackOn []ProviderFailureClass, sideEffect SideEffectClass) *fallbackFixture {
	t.Helper()
	for _, raw := range candidates {
		_, model, _ := strings.Cut(raw, "/")
		for _, id := range []string{model, raw} {
			GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: id, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})
		}
	}
	workspace := t.TempDir()
	runID := "fallback-test-run"
	store, err := NewEventStore(workspace, runID, "fallback-test-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", Tools: "view,write,bash", MaxRetries: 0, Timeout: 20, SideEffect: string(sideEffect),
		Generation: agent.GenerationParams{MaxTokens: "128"},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{Name: "fallback-test", Timeout: 20, MaxRetries: 0, Generation: agent.GenerationParams{MaxTokens: "128"}},
		Agents: map[string]*agent.AgentDef{"worker": worker},
	}
	configs := make(map[string]config.ProviderConfig, len(providers))
	var defaultURL string
	for name, url := range providers {
		configs[name] = config.ProviderConfig{ProviderURL: url}
		if name == "ollama" {
			defaultURL = url
		}
	}
	providerManager, err := agent.NewProviderManager(defaultURL, "", configs)
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
		modelList: []config.ModelEntry{{ID: "ollama/never-escalate"}},
	}
	route := &ExecutionRouteDefinition{Name: "coding", FallbackOn: fallbackOn}
	for _, raw := range candidates {
		target, err := c.executionRouteCandidate(raw)
		if err != nil {
			t.Fatalf("candidate %s: %v", raw, err)
		}
		route.Candidates = append(route.Candidates, target)
	}
	route.Digest = executionRouteDigest(route.Name, route.Candidates, route.FallbackOn)
	session.ExecutionRoutes = map[string]*ExecutionRouteDefinition{"coding": route}
	session.AgentExecutionRoutes = map[string]string{"worker": "coding"}

	ids := c.taskTracker.TodoList().ReserveIDs(1)
	canonical, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: "worker", Goal: "do the work"}, worker, c.resolveAgentModel(worker, ""))
	if err != nil {
		t.Fatalf("canonicalizeTaskOccurrence: %v", err)
	}
	if canonical.ExecutionRoute == nil {
		t.Fatal("the task was not bound to its route")
	}
	spec := TodoSpec{
		Agent: "worker", Desc: "do the work", Goal: "do the work",
		Model: canonical.Model, ModelTopology: initialTaskModelTopology(worker, canonical.Model),
		ExecutionTarget: canonical.ResolvedExecutionTarget, ExecutionTopology: canonical.ExecutionTopology,
		ExecutionRoute: canonical.ExecutionRoute.clone(), SideEffect: canonical.SideEffect, Recovery: canonical.Recovery,
		Source: TaskSourceCoordinator, Execution: ExecutionContract{RequiresResult: true}, SubagentProvider: canonical.SubagentProvider,
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	return &fallbackFixture{c: c, store: store, item: items[0], worker: worker}
}

func (f *fallbackFixture) run(t *testing.T) error {
	t.Helper()
	_, err := f.c.executeTask(context.Background(), taskDefFromTodoItem(f.item), f.item.ID)
	return err
}

func (f *fallbackFixture) receipts(t *testing.T) []ExecutionReceipt {
	t.Helper()
	item := f.c.todoItemByID(f.item.ID)
	return append([]ExecutionReceipt(nil), item.ExecutionReceipts...)
}

func (f *fallbackFixture) fallbackEvents(t *testing.T) []map[string]any {
	t.Helper()
	events, err := f.store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var decided []map[string]any
	for _, event := range events {
		if event.Type != string(EventExecutionFallbackDecided) {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		decided = append(decided, payload)
	}
	return decided
}

func closedServerURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable: %v", err)
	}
	url := "http://" + listener.Addr().String()
	_ = listener.Close()
	return url
}

var allFallbackClasses = []ProviderFailureClass{ProviderRateLimited, ProviderUnavailable, ProviderModelUnavailable, ProviderTransportTimeout}

func TestProviderFailureFallsBackToTheNextCandidate(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		refused   bool
		wantClass ProviderFailureClass
	}{
		{name: "429 rate limited", status: http.StatusTooManyRequests, wantClass: ProviderRateLimited},
		{name: "503 unavailable", status: http.StatusServiceUnavailable, wantClass: ProviderUnavailable},
		{name: "404 model not found", status: http.StatusNotFound, wantClass: ProviderModelUnavailable},
		{name: "connection refused", refused: true, wantClass: ProviderUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fallbackTestServer{failures: map[string]int{"fb-primary": tt.status}}
			server := newIPv4TestServer(t, backend)
			t.Cleanup(server.Close)
			providers := map[string]string{"ollama": server.URL}
			candidates := []string{"ollama/fb-primary", "ollama/fb-fallback"}
			if tt.refused {
				providers["down"] = closedServerURL(t)
				candidates = []string{"down/fb-primary", "ollama/fb-fallback"}
			}
			f := newFallbackFixture(t, providers, candidates, allFallbackClasses, SideEffectNone)
			if err := f.run(t); err != nil {
				t.Fatalf("executeTask: %v", err)
			}
			if got := f.c.todoItemByID(f.item.ID); got.Status != TaskDone {
				t.Fatalf("status = %s (%s)", got.Status, got.Detail)
			}
			if seen := backend.seen(); len(seen) == 0 || seen[len(seen)-1] != "fb-fallback" {
				t.Fatalf("backend models = %v, want the fallback model to run", seen)
			}
			for _, model := range backend.seen() {
				if model == "never-escalate" {
					t.Fatal("a route-bound task escalated to a model-list model")
				}
			}
			receipts := f.receipts(t)
			if len(receipts) != 2 {
				t.Fatalf("receipts = %d, want one per attempt (%+v)", len(receipts), receipts)
			}
			first, second := receipts[0], receipts[1]
			if first.ExecutionTarget.Model != "fb-primary" || first.CandidateIndex == nil || *first.CandidateIndex != 0 {
				t.Fatalf("first attempt receipt = target %v candidate %v", first.ExecutionTarget, first.CandidateIndex)
			}
			if second.ExecutionTarget.Model != "fb-fallback" || second.CandidateIndex == nil || *second.CandidateIndex != 1 ||
				second.FallbackFrom == nil || second.FallbackFrom.Model != "fb-primary" || second.FallbackFailureClass != tt.wantClass || second.Backend != "ollama" {
				t.Fatalf("fallback attempt receipt = %+v", second)
			}
			decided := f.fallbackEvents(t)
			if len(decided) != 1 || decided[0]["failure_class"] != string(tt.wantClass) || decided[0]["candidate_index"] != float64(1) {
				t.Fatalf("execution_fallback_decided events = %v", decided)
			}
			if frozen := f.c.todoItemByID(f.item.ID).ExecutionTarget; frozen.Model != "fb-primary" {
				t.Fatalf("the occurrence's frozen target changed to %v", frozen)
			}
			// max-retries is 0: the fallback ran outside the retry budget and
			// recorded no retry.
			if retries := f.c.Metrics().RetriesByFailureClass; len(retries) != 0 {
				t.Fatalf("a fallback was counted as a retry: %v", retries)
			}
		})
	}
}

func TestProviderFailureWithoutFallback(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		fallbackOn []ProviderFailureClass
		sideEffect SideEffectClass
		toolFirst  [2]string
		wantDenied string
	}{
		{name: "401 is never fallback-eligible", status: http.StatusUnauthorized, fallbackOn: allFallbackClasses, sideEffect: SideEffectNone, wantDenied: fallbackDeniedNotInFallbackOn + ": " + string(ProviderAuthFailed)},
		{name: "class not in fallback-on", status: http.StatusServiceUnavailable, fallbackOn: []ProviderFailureClass{ProviderRateLimited}, sideEffect: SideEffectNone, wantDenied: fallbackDeniedNotInFallbackOn + ": " + string(ProviderUnavailable)},
		{name: "shared write after a file write", status: http.StatusTooManyRequests, fallbackOn: allFallbackClasses, sideEffect: SideEffectWorkspaceWrite,
			toolFirst: [2]string{"write", `{"file_path":"written.txt","content":"written\n"}`}, wantDenied: fallbackDeniedSideEffects + ": " + string(ProviderRateLimited)},
		{name: "external write after any call", status: http.StatusTooManyRequests, fallbackOn: allFallbackClasses, sideEffect: SideEffectExternalWrite,
			toolFirst: [2]string{"view", `{"file_path":"notes.txt"}`}, wantDenied: fallbackDeniedSideEffects + ": " + string(ProviderRateLimited)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &fallbackTestServer{failures: map[string]int{"fb-primary": tt.status}}
			if tt.toolFirst[0] != "" {
				backend.toolFirst = map[string][2]string{"fb-primary": tt.toolFirst}
			}
			server := newIPv4TestServer(t, backend)
			t.Cleanup(server.Close)
			f := newFallbackFixture(t, map[string]string{"ollama": server.URL}, []string{"ollama/fb-primary", "ollama/fb-fallback"}, tt.fallbackOn, tt.sideEffect)
			if err := os.WriteFile(filepath.Join(f.c.projectDir, "notes.txt"), []byte("notes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := f.run(t); err == nil {
				item := f.c.todoItemByID(f.item.ID)
				t.Fatalf("executeTask succeeded without the fallback candidate: models %v status %s receipts %+v", backend.seen(), item.Status, item.ExecutionReceipts)
			}
			for _, model := range backend.seen() {
				if model == "fb-fallback" {
					t.Fatalf("the fallback candidate ran (models %v)", backend.seen())
				}
			}
			receipts := f.receipts(t)
			if len(receipts) == 0 || receipts[len(receipts)-1].FallbackDeniedReason != tt.wantDenied {
				t.Fatalf("receipts = %+v, want denied reason %q", receipts, tt.wantDenied)
			}
			if decided := f.fallbackEvents(t); len(decided) != 0 {
				t.Fatalf("unexpected fallback events %v", decided)
			}
			if tt.toolFirst[0] == "write" {
				if _, err := os.Stat(filepath.Join(f.c.projectDir, "written.txt")); err != nil {
					t.Fatalf("the write the fallback was denied for never ran: %v", err)
				}
			}
		})
	}
}

func TestSharedWriteFallsBackAfterOnlyReadOnlyCalls(t *testing.T) {
	backend := &fallbackTestServer{
		failures:  map[string]int{"fb-primary": http.StatusTooManyRequests},
		toolFirst: map[string][2]string{"fb-primary": {"view", `{"file_path":"notes.txt"}`}},
	}
	server := newIPv4TestServer(t, backend)
	t.Cleanup(server.Close)
	f := newFallbackFixture(t, map[string]string{"ollama": server.URL}, []string{"ollama/fb-primary", "ollama/fb-fallback"}, allFallbackClasses, SideEffectWorkspaceWrite)
	if err := os.WriteFile(filepath.Join(f.c.projectDir, "notes.txt"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if seen := backend.seen(); seen[len(seen)-1] != "fb-fallback" {
		t.Fatalf("models = %v, want the fallback candidate after a read-only view", seen)
	}
}

func TestFallbackAcrossBackendsUsesTheCandidateBackend(t *testing.T) {
	primary := &fallbackTestServer{failures: map[string]int{"fb-primary": http.StatusServiceUnavailable}}
	remote := &fallbackTestServer{}
	primaryServer := newIPv4TestServer(t, primary)
	remoteServer := newIPv4TestServer(t, remote)
	t.Cleanup(primaryServer.Close)
	t.Cleanup(remoteServer.Close)
	f := newFallbackFixture(t, map[string]string{"ollama": primaryServer.URL, "remote": remoteServer.URL}, []string{"ollama/fb-primary", "remote/fb-remote"}, allFallbackClasses, SideEffectNone)
	if err := f.run(t); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if seen := remote.seen(); len(seen) != 1 || seen[0] != "fb-remote" {
		t.Fatalf("remote backend models = %v, want the candidate's model", seen)
	}
	receipts := f.receipts(t)
	if last := receipts[len(receipts)-1]; last.Backend != "remote" || last.ExecutionTarget.Backend != "remote" {
		t.Fatalf("fallback receipt backend = %q target %v", last.Backend, last.ExecutionTarget)
	}
}

func TestNormalRetryOfARouteTaskReturnsToThePrimary(t *testing.T) {
	backend := &fallbackTestServer{failures: map[string]int{"fb-primary": http.StatusTooManyRequests}}
	backend.toolFirst = map[string][2]string{"fb-fallback": {submitResultToolName, `{"status":"failed","summary":"could not finish"}`}}
	server := newIPv4TestServer(t, backend)
	t.Cleanup(server.Close)
	f := newFallbackFixture(t, map[string]string{"ollama": server.URL}, []string{"ollama/fb-primary", "ollama/fb-fallback"}, allFallbackClasses, SideEffectNone)
	f.worker.MaxRetries = 1
	if err := f.run(t); err == nil {
		t.Fatal("executeTask succeeded")
	}
	// primary (429) → fallback (reports failed) → retry on the primary
	// (429, the route's only fallback is spent) → stop.
	want := []string{"fb-primary", "fb-fallback", "fb-primary"}
	if seen := backend.seen(); !slices.Equal(seen, want) {
		t.Fatalf("models = %v, want %v", seen, want)
	}
	receipts := f.receipts(t)
	if last := receipts[len(receipts)-1]; last.FallbackDeniedReason != fallbackDeniedNoNextCandidate+": "+string(ProviderRateLimited) {
		t.Fatalf("last receipt denied reason = %q", last.FallbackDeniedReason)
	}
}

func TestHufuLocalAttemptRejectsATargetOutsideTheRoute(t *testing.T) {
	server := newIPv4TestServer(t, &fallbackTestServer{})
	t.Cleanup(server.Close)
	f := newFallbackFixture(t, map[string]string{"ollama": server.URL}, []string{"ollama/fb-primary", "ollama/fb-fallback"}, allFallbackClasses, SideEffectNone)
	task := taskDefFromTodoItem(f.item)
	_, err := NewHufuLocalSubagentProvider(f.c).RunAttempt(context.Background(), AttemptRequest{
		TaskID: f.item.ID, Attempt: 1, ModelID: "fb-other", Agent: f.worker, Task: task,
		ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "fb-other"},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match canonical") {
		t.Fatalf("RunAttempt error = %v, want a rejected target outside the route", err)
	}
}
