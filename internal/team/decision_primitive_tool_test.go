package team

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/tools"
)

func primitiveTestEntry(endpoint string) catalog.Entry {
	return catalog.Entry{Backend: "systemone", Endpoint: endpoint, Model: "nimble", Version: "1", Kind: decisionrt.KindChoice, Question: "Select a category", Options: []catalog.Option{{ID: "a"}, {ID: "b"}}, Inputs: map[string]string{"summary": "string"}, Agents: []string{"helper", "coordinator"}}
}

func primitiveTestCoordinator(t *testing.T, entry catalog.Entry) *Coordinator {
	t.Helper()
	service, err := catalog.New(map[string]catalog.Entry{"classify": entry})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	_, err = store.AppendPersisted(RunEvent{Type: string(EventTaskCreated), TaskID: "1", BranchID: "main", Actor: "helper", Payload: json.RawMessage(`{"id":"1","status":"pending"}`)})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "test", DecisionPrimitives: map[string]catalog.Entry{"classify": entry}}, Agents: map[string]*agent.AgentDef{"helper": {Name: "helper", Role: "worker", Tools: decisionPrimitiveToolName}}}, decisionPrimitives: service, decisionPrimitiveGate: make(chan struct{}, 1), eventStore: store, eventJournal: eventStoreJournal{store: store}, sessionTime: time.Now()}
	c.coreTools = []fantasy.AgentTool{&decisionPrimitiveTool{coordinator: c}}
	return c
}

func primitiveTestContext(t *testing.T, actor string) context.Context {
	t.Helper()
	ctx := withInvocationMetadata(t.Context(), InvocationMetadata{RunID: "run-test", TaskID: "1", AgentName: actor, Attempt: 1})
	ctx = context.WithValue(ctx, tools.AgentNameKey, actor)
	ctx = context.WithValue(ctx, todoIDKey{}, "1")
	return tools.SetToolsAllowed(ctx, []string{decisionPrimitiveToolName})
}

func primitiveTestCall(id, summary string) fantasy.ToolCall {
	input, _ := json.Marshal(map[string]any{"name": "classify", "context": map[string]any{"summary": summary}})
	return fantasy.ToolCall{ID: id, Name: decisionPrimitiveToolName, Input: string(input)}
}

const primitiveChoiceResponse = `{"answers":{"decision":{"type":"choice","choice":"o00","probabilities":{"o00":0.9,"o01":0.1},"confidence":0.5}}}`

func TestDecisionPrimitiveTwoTeamsUseSharedAdapter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected transport %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(primitiveChoiceResponse))
	}))
	defer server.Close()
	for _, teamName := range []string{"support", "engineering"} {
		t.Run(teamName, func(t *testing.T) {
			c := primitiveTestCoordinator(t, primitiveTestEntry(server.URL+"/v1/systemone"))
			c.session.Config.Name = teamName
			ctx := context.WithValue(primitiveTestContext(t, "helper"), tools.AgentReadOnlyExecutionKey, true)
			tool := c.gatePolicyTools(c.coreTools)[0]
			response, err := tool.Run(ctx, primitiveTestCall("call-1", "inspect request"))
			if err != nil || response.IsError {
				t.Fatalf("call: %v %#v", err, response)
			}
			var output struct {
				Result  decisionrt.Result
				Receipt decisionrt.Receipt
			}
			if err := json.Unmarshal([]byte(response.Content), &output); err != nil {
				t.Fatal(err)
			}
			if output.Result.Value.Choice != "a" || output.Result.Confidence != .9 || output.Result.ConfidenceSemantics != decisionrt.ConfidenceRaw || output.Receipt.Backend != "systemone" {
				t.Fatalf("output: %#v", output)
			}
			records, err := c.DecisionPrimitiveResults(t.Context())
			if err != nil || len(records) != 1 || records[0].Status != "decided" {
				t.Fatalf("records: %#v %v", records, err)
			}
			events, _ := c.EventJournal().ReadEvents(t.Context())
			projected := ReduceToSessionData(events)
			if len(projected.DecisionPrimitiveResults) != 1 {
				t.Fatal("settlement missing from session replay")
			}
			if err := SaveSession(c.session.Workspace, projected); err != nil {
				t.Fatal(err)
			}
			loaded := LoadSession(c.session.Workspace)
			if loaded == nil || len(loaded.DecisionPrimitiveResults) != 1 {
				t.Fatal("session reload lost helper decisions")
			}
		})
	}
	if calls.Load() != 2 {
		t.Fatalf("backend calls: %d", calls.Load())
	}
}

func TestDecisionPrimitiveResumeAndConcurrentDuplicate(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(primitiveChoiceResponse))
	}))
	defer server.Close()
	c := primitiveTestCoordinator(t, primitiveTestEntry(server.URL))
	ctx := primitiveTestContext(t, "helper")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			response, err := c.coreTools[0].Run(ctx, primitiveTestCall(string(rune('a'+i)), "same request"))
			if err != nil || response.IsError {
				t.Errorf("concurrent call: %v %#v", err, response)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate inference: %d", calls.Load())
	}
	// Reopen the real durable store, with a new execution run ID, just as a
	// crash-resume does. The task-created anchor remains unchanged.
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(c.session.Workspace, "run-resumed", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetBranchID("main")
	resumed := &Coordinator{session: c.session, decisionPrimitives: c.decisionPrimitives, decisionPrimitiveGate: make(chan struct{}, 1), eventStore: store, eventJournal: eventStoreJournal{store: store}}
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: "run-resumed", TaskID: "1", AgentName: "helper", Attempt: 2})
	response, err := (&decisionPrimitiveTool{coordinator: resumed}).Run(ctx, primitiveTestCall("new-id", "same request"))
	if err != nil || response.IsError || calls.Load() != 1 {
		t.Fatalf("resume recomputed: %d %v %#v", calls.Load(), err, response)
	}
	// Sibling branches have independent decision identities.
	tree := NewSessionTree()
	tree.Branches["other"] = &SessionBranch{ID: "other"}
	tree.ActiveBranch = "other"
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("other")
	response, err = (&decisionPrimitiveTool{coordinator: resumed}).Run(ctx, primitiveTestCall("other-id", "same request"))
	if err != nil || response.IsError || calls.Load() != 2 {
		t.Fatalf("branch leaked: %d %v %#v", calls.Load(), err, response)
	}
}

func TestDecisionPrimitiveAbstainFailureRetryAndLimits(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(primitiveChoiceResponse))
	}))
	defer server.Close()
	entry := primitiveTestEntry(server.URL)
	entry.MinConfidence, entry.MaxCalls = new(.95), 2
	c := primitiveTestCoordinator(t, entry)
	ctx := primitiveTestContext(t, "helper")
	tool := c.coreTools[0]
	first, err := tool.Run(ctx, primitiveTestCall("failure", "same"))
	if err != nil || !first.IsError {
		t.Fatalf("backend failure: %v %#v", err, first)
	}
	second, err := tool.Run(ctx, primitiveTestCall("retry", "same"))
	if err != nil || second.IsError || !strings.Contains(second.Content, `"status":"abstained"`) {
		t.Fatalf("low-confidence retry: %v %#v", err, second)
	}
	third, err := tool.Run(ctx, primitiveTestCall("exhausted", "different"))
	if err != nil || !third.IsError || third.Content != "decision_call_limit" || calls.Load() != 2 {
		t.Fatalf("call bound: %v %#v", err, third)
	}
}

func TestDecisionPrimitiveRejectsBeforeInference(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*Coordinator)
		actor string
		input string
	}{
		{"ungranted actor", func(*Coordinator) {}, "other", `{"name":"classify","context":{"summary":"text"}}`},
		{"unknown decision", func(*Coordinator) {}, "helper", `{"name":"unknown","context":{"summary":"text"}}`},
		{"unknown input", func(*Coordinator) {}, "helper", `{"name":"classify","context":{"summary":"text"},"endpoint":"http://other"}`},
		{"duplicate key", func(*Coordinator) {}, "helper", `{"name":"classify","name":"classify","context":{"summary":"text"}}`},
		{"null context", func(c *Coordinator) {
			entry := primitiveTestEntry("http://127.0.0.1:1")
			entry.Inputs = nil
			var err error
			c.decisionPrimitives, err = catalog.New(map[string]catalog.Entry{"classify": entry})
			if err != nil {
				t.Fatal(err)
			}
		}, "helper", `{"name":"classify","context":null}`},
		{"nested context", func(*Coordinator) {}, "helper", `{"name":"classify","context":{"summary":{"a":"b"}}}`},
		{"no net", func(c *Coordinator) { c.noNet = true }, "helper", `{"name":"classify","context":{"summary":"text"}}`},
		{"force mcp", func(c *Coordinator) { c.forceMCP = true }, "helper", `{"name":"classify","context":{"summary":"text"}}`},
		{"coordinator no net", func(c *Coordinator) {
			c.session.Agents["planner"] = &agent.AgentDef{Name: "planner", Role: "coordinator", NoNet: true}
		}, "coordinator", `{"name":"classify","context":{"summary":"text"}}`},
		{"denied", func(c *Coordinator) { c.session.Config.ToolsDenied = []string{decisionPrimitiveToolName} }, "helper", `{"name":"classify","context":{"summary":"text"}}`},
		{"journal missing", func(c *Coordinator) { c.eventStore, c.eventJournal = nil, nil }, "helper", `{"name":"classify","context":{"summary":"text"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := primitiveTestCoordinator(t, primitiveTestEntry("http://127.0.0.1:1"))
			test.setup(c)
			response, err := c.coreTools[0].Run(primitiveTestContext(t, test.actor), fantasy.ToolCall{ID: "denied", Input: test.input})
			if err != nil || !response.IsError || strings.Contains(response.Content, "backend") {
				t.Fatalf("rejection: %v %#v", err, response)
			}
		})
	}
}

func TestDecisionPrimitiveStaticSurfaceAndPolicyGate(t *testing.T) {
	entry := primitiveTestEntry("http://127.0.0.1:1")
	c := primitiveTestCoordinator(t, entry)
	def := c.session.Agents["helper"]
	for _, mode := range []WorkerToolResolutionMode{WorkerToolResolutionNormal, WorkerToolResolutionInitialPlan, WorkerToolResolutionResultRepair, WorkerToolResolutionResume} {
		resolution, err := ResolveStaticWorkerTools(StaticToolResolutionInput{Session: c.session, Agent: def, BaseTools: []string{decisionPrimitiveToolName}, LifecycleMode: mode, WorkflowEnabled: true, WorkflowPhase: PhaseAudit})
		if err != nil {
			t.Fatal(err)
		}
		want := mode != WorkerToolResolutionResultRepair && mode != WorkerToolResolutionResume
		if slices.Contains(resolution.Names, decisionPrimitiveToolName) != want {
			t.Fatalf("mode %s: %v", mode, resolution.Names)
		}
	}
	for _, grant := range []string{"", "all", "view"} {
		copyDef := *def
		copyDef.Tools = grant
		resolution, err := ResolveStaticWorkerTools(StaticToolResolutionInput{Session: c.session, Agent: &copyDef, BaseTools: []string{decisionPrimitiveToolName}})
		if err != nil || slices.Contains(resolution.Names, decisionPrimitiveToolName) {
			t.Fatalf("implicit grant %q accepted: %v", grant, err)
		}
	}
	ctx := tools.SetToolsAllowed(primitiveTestContext(t, "helper"), []string{"view"})
	response, err := c.gatePolicyTools(c.coreTools)[0].Run(ctx, primitiveTestCall("policy-denied", "text"))
	if err != nil || !response.IsError {
		t.Fatalf("policy denial: %v %#v", err, response)
	}
	if !slices.Contains(agentToolNames(c.buildOrchestratorTools()), decisionPrimitiveToolName) {
		t.Fatal("coordinator did not receive granted helper decision")
	}
	c.session.Agents["planner"] = &agent.AgentDef{Name: "planner", Role: "coordinator", ForceMCP: true}
	if slices.Contains(agentToolNames(c.buildOrchestratorTools()), decisionPrimitiveToolName) {
		t.Fatal("force-MCP coordinator received the helper decision tool")
	}
	for _, key := range []any{tools.AgentNetworkBlockKey, tools.AgentForceMCPKey} {
		ctx := context.WithValue(primitiveTestContext(t, "helper"), key, true)
		response, err := c.coreTools[0].Run(ctx, primitiveTestCall("blocked-context", "text"))
		if err != nil || !response.IsError || response.Content != "decision_policy_denied" {
			t.Fatalf("context policy bypassed: %#v %v", response, err)
		}
	}
}

func TestDecisionPrimitiveParseAndPolicyDrift(t *testing.T) {
	dir := t.TempDir()
	manifest := `name: generic
decision-primitives:
  classify:
    backend: rule
    version: "1"
    kind: choice
    question: Select a category
    options: [{id: a}, {id: b}]
    inputs: {summary: string}
    agents: [helper]
    timeout: 5s
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DecisionPrimitives["classify"].Timeout != 5*time.Second {
		t.Fatal("timeout not parsed")
	}
	c := newExecutionPolicySnapshotCoordinator(t, t.TempDir(), 2, 2)
	service, err := catalog.New(cfg.DecisionPrimitives)
	if err != nil {
		t.Fatal(err)
	}
	c.decisionPrimitives = service
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	entry := cfg.DecisionPrimitives["classify"]
	entry.Question = "Choose differently"
	cfg.DecisionPrimitives["classify"] = entry
	c.decisionPrimitives, err = catalog.New(cfg.DecisionPrimitives)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if changed.snapshot.ConfigurationHash == state.snapshot.ConfigurationHash {
		t.Fatal("policy failed to pin decision contracts")
	}
}

type primitiveFailingJournal struct {
	EventJournal
	kind EventType
}

func (j primitiveFailingJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == string(j.kind) {
		return RunEvent{}, errors.New("durability rejected")
	}
	return j.EventJournal.Append(ctx, event)
}

func TestDecisionPrimitiveDurabilityFailureDoesNotPublishResult(t *testing.T) {
	entry := primitiveTestEntry("http://127.0.0.1:1")
	entry.Backend = "rule"
	c := primitiveTestCoordinator(t, entry)
	c.eventJournal = primitiveFailingJournal{EventJournal: c.eventJournal, kind: EventDecisionPrimitiveSettled}
	response, err := c.coreTools[0].Run(primitiveTestContext(t, "helper"), primitiveTestCall("publish", "text"))
	if err == nil || response.Content != "" {
		t.Fatalf("undurable result escaped: %#v %v", response, err)
	}
}

func TestDecisionPrimitiveCoordinatorIdentity(t *testing.T) {
	entry := primitiveTestEntry("http://127.0.0.1:1")
	entry.Backend = "rule"
	c := primitiveTestCoordinator(t, entry)
	ctx := withInvocationMetadata(t.Context(), InvocationMetadata{RunID: "run-test", AgentName: "coordinator", AgentRole: "coordinator", Attempt: 1})
	response, err := c.coreTools[0].Run(ctx, primitiveTestCall("coordinator-call", "text"))
	if err != nil || response.IsError {
		t.Fatalf("coordinator call: %v %#v", err, response)
	}
	records, err := c.DecisionPrimitiveResults(t.Context())
	if err != nil || len(records) != 1 || records[0].TaskID != CoordTodoID || records[0].Agent != "coordinator" {
		t.Fatalf("coordinator identity: %#v %v", records, err)
	}
}

func TestDecisionPrimitiveCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[timeout], func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(started)
				<-release
				_, _ = w.Write([]byte(primitiveChoiceResponse))
			}))
			defer server.Close()
			defer close(release)
			entry := primitiveTestEntry(server.URL)
			wantCode := "decision_canceled"
			if timeout {
				entry.Timeout = 100 * time.Millisecond
				wantCode = "decision_timeout"
			}
			c := primitiveTestCoordinator(t, entry)
			ctx, cancel := context.WithCancel(primitiveTestContext(t, "helper"))
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				response, err := c.coreTools[0].Run(ctx, primitiveTestCall("cancel-call", "text"))
				if err == nil && (!response.IsError || response.Content != wantCode) {
					err = errors.New("timeout or cancellation published a decision")
				}
				finished <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("inference did not start")
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-finished:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("inference did not stop")
			}
			records, err := c.DecisionPrimitiveResults(t.Context())
			if err != nil || len(records) != 1 || records[0].Status != "error" || records[0].ErrorCode != wantCode {
				t.Fatalf("canceled outcome not committed: %#v %v", records, err)
			}
		})
	}
}

func TestDecisionPrimitiveEventValidation(t *testing.T) {
	entry := primitiveTestEntry("http://127.0.0.1:1")
	entry.Backend = "rule"
	c := primitiveTestCoordinator(t, entry)
	response, err := c.coreTools[0].Run(primitiveTestContext(t, "helper"), primitiveTestCall("event", "text"))
	if err != nil || response.IsError {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	event := events[len(events)-1]
	var payload decisionPrimitivePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.Receipt.RequestDigest = "sha256:" + strings.Repeat("0", 64)
	event.Payload, _ = json.Marshal(payload)
	if validateDecisionPrimitiveEvent(event) == nil {
		t.Fatal("receipt mismatch accepted")
	}
	if _, err := projectDecisionPrimitiveResults([]RunEvent{event}); err == nil {
		t.Fatal("invalid settlement published by projection")
	}
	if !ReduceToSessionData([]RunEvent{event}).RecoveryRequired {
		t.Fatal("invalid settlement did not mark recovery required")
	}
}

func TestDecisionPrimitiveJournalIdempotencyConflicts(t *testing.T) {
	entry := primitiveTestEntry("http://127.0.0.1:1")
	entry.Backend = "rule"
	c := primitiveTestCoordinator(t, entry)
	response, err := c.coreTools[0].Run(primitiveTestContext(t, "helper"), primitiveTestCall("event", "text"))
	if err != nil || response.IsError {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	event := events[len(events)-1]
	var payload decisionPrimitivePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload.CallID = "conflicting-call"
	event.ID = "conflicting-event"
	event.Payload, _ = json.Marshal(payload)
	if _, err := c.EventJournal().Append(t.Context(), event); !errors.Is(err, ErrDecisionIdempotencyConflict) {
		t.Fatalf("duplicate helper identity accepted: %v", err)
	}
	// A separately appended, hash-valid tail must fail the same conflict
	// gate when the store rescans. A valid chain alone does not bind a result.
	appendRawChainedEvent(t, filepath.Join(c.session.Workspace, logsDir, eventStoreFile), event)
	if err := c.EventJournal().VerifyHashChain(t.Context()); !errors.Is(err, ErrDecisionIdempotencyConflict) {
		t.Fatalf("conflicting helper replay accepted: %v", err)
	}
}

func TestDecisionPrimitiveConstructorAndExtraModelSurface(t *testing.T) {
	entry := primitiveTestEntry("http://127.0.0.1:1")
	entry.Backend = "rule"
	fixture := primitiveTestCoordinator(t, entry)
	c, err := newCoordinator(coordinatorParams{Session: fixture.session}, RuntimeServices{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !containsTool(c.coreTools, decisionPrimitiveToolName) {
		t.Fatal("constructor omitted the native decision tool")
	}
	for _, mode := range []WorkerToolResolutionMode{WorkerToolResolutionNormal, WorkerToolResolutionInitialPlan} {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "helper", Desc: "inspect"}})[0]
		resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), c.session.Agents["helper"], WorkerToolResolutionRequest{Task: TaskDef{Agent: "helper"}, TodoID: item.ID, Mode: mode})
		if err != nil || !containsTool(resolved.Tools, decisionPrimitiveToolName) {
			t.Fatalf("runtime resolver mode %s: %v", mode, err)
		}
	}
	// Isolated leaf workspaces must keep the parent-owned helper service and
	// journal; otherwise fanout could bypass limits or strand its receipts.
	clonedSession := *c.session
	clonedSession.Workspace = t.TempDir()
	clone := cloneCoordinator(c, &clonedSession)
	if clone.decisionPrimitiveGate != c.decisionPrimitiveGate || clone.decisionPrimitives != c.decisionPrimitives {
		t.Fatal("extra-model clone created independent helper state")
	}
	for _, tool := range clone.coreTools {
		if primitive, ok := tool.(*decisionPrimitiveTool); ok && primitive.coordinator != c {
			t.Fatal("extra-model tool no longer belongs to the parent control plane")
		}
	}
	fixture.session.Config.DecisionPrimitives = nil
	plain, err := newCoordinator(coordinatorParams{Session: fixture.session}, RuntimeServices{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	if containsTool(plain.coreTools, decisionPrimitiveToolName) {
		t.Fatal("unconfigured team received a decision tool")
	}
	fixture.session.Config.DecisionPrimitives = map[string]catalog.Entry{"classify": entry}
	entry.Agents = []string{"unknown-worker"}
	fixture.session.Config.DecisionPrimitives["classify"] = entry
	if _, err := newCoordinator(coordinatorParams{Session: fixture.session}, RuntimeServices{}); err == nil {
		t.Fatal("unknown worker grant passed constructor preflight")
	}
}

func TestDecisionPrimitiveRefusesOnlyWhenBudgetIsExhausted(t *testing.T) {
	for _, test := range []struct {
		name         string
		wrapUp       bool
		exhausted    bool
		wantDecision bool
	}{
		{name: "running", wantDecision: true},
		{name: "wrap-up", wrapUp: true, wantDecision: true},
		{name: "budget exhausted", exhausted: true},
		{name: "wrap-up with budget exhausted", wrapUp: true, exhausted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(primitiveChoiceResponse))
			}))
			defer server.Close()
			c := primitiveTestCoordinator(t, primitiveTestEntry(server.URL))
			if test.wrapUp {
				c.wrapUp.Store(1)
			}
			if test.exhausted {
				c.budgetLedger.setLimits(0, 10)
				c.budgetLedger.addTokens(10)
			}
			response, err := c.coreTools[0].Run(primitiveTestContext(t, "helper"), primitiveTestCall("call", "text"))
			if err != nil {
				t.Fatal(err)
			}
			if test.wantDecision {
				if response.IsError || !strings.Contains(response.Content, `"status":"decided"`) || calls.Load() != 1 {
					t.Fatalf("response = %#v after %d backend calls, want a decision", response, calls.Load())
				}
				return
			}
			if !response.IsError || response.Content != "decision_budget_exceeded" || calls.Load() != 0 {
				t.Fatalf("response = %#v after %d backend calls, want a budget refusal before inference", response, calls.Load())
			}
		})
	}
}
