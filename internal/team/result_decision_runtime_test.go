package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/tools"
)

type boundDecisionFixture struct {
	c       *Coordinator
	item    *TodoItem
	sources []*TodoItem
	plans   []resultFixedCollectionPlan
	ctx     context.Context
	calls   atomic.Int32
}

func newBoundDecisionFixture(t *testing.T, count int, amendments ...func(map[string]any)) *boundDecisionFixture {
	t.Helper()
	c, sources, item, _ := fixedCollectionFixture(t, count)
	f := &boundDecisionFixture{c: c, item: item, sources: sources}
	root, err := decodeResultContractJSON([]byte(fixedCollectionTestSchema))
	if err != nil {
		t.Fatal(err)
	}
	spec := root.(map[string]any)["x-hufu-fixed-collections"].([]any)[0].(map[string]any)
	spec["decision_groups"] = []any{map[string]any{"id": "assessment", "catalog_entry": "classify", "outputs": []string{"state"}, "value_map": map[string]any{"a": map[string]string{"state": "accept"}, "b": map[string]string{"state": "reject"}}, "context": map[string]any{"mode": "observed_artifact_reads", "max_bytes": resultDecisionContextMax}, "on_abstained": "blocked", "on_error": "blocked"}}
	for _, amend := range amendments {
		amend(root.(map[string]any))
	}
	raw, _ := json.Marshal(root)
	compiled, err := CompileResultContractDocument("decisions.json", raw)
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	item.ResultContract = &ref
	c.session.Workspace = t.TempDir()
	c.session.Agents = map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker", Tools: "view,decision_primitive"}}
	entry := catalog.Entry{Backend: "sidecar", Version: "1", Kind: decisionrt.KindChoice, Question: "Choose from the runtime evidence", Agents: []string{"worker"}, Inputs: resultDecisionInputs(), Options: []catalog.Option{{ID: "a"}, {ID: "b"}}, MaxCalls: 100}
	c.session.Config.DecisionPrimitives = map[string]catalog.Entry{"classify": entry}
	c.decisionPrimitives, err = catalog.New(c.session.Config.DecisionPrimitives, catalog.WithSidecarGenerator(func(string) (sidecarbackend.Generator, error) { return countingGenerator{calls: &f.calls}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	c.executionRunID = "run-test"
	c.setCurrentTaskAttempt(item.ID, 1)
	c.decisionPrimitiveGate = make(chan struct{}, 1)
	store, err := NewEventStore(c.session.Workspace, "run-test", "session-test")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	t.Cleanup(func() { _ = store.Close() })
	c.eventStore = store
	c.eventJournal = eventStoreJournal{store: store}
	raw, _ = json.Marshal(item)
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Actor: item.Agent, BranchID: "main", Payload: raw}); err != nil {
		t.Fatal(err)
	}
	f.ctx = withInvocationMetadata(t.Context(), InvocationMetadata{RunID: "run-test", TaskID: item.ID, AgentName: "worker", Attempt: 1})
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		t.Fatal("missing task occurrence")
	}
	f.ctx = withSubmitResultRuntimeIdentity(f.ctx, identity)
	f.ctx = context.WithValue(f.ctx, todoIDKey{}, item.ID)
	f.ctx = tools.SetToolsAllowed(f.ctx, []string{"view", decisionPrimitiveToolName})
	artifacts, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := artifacts.Put(t.Context(), PutArtifactRequest{Kind: "test", Path: "source.txt", Content: []byte("immutable source evidence\n"), TaskID: sources[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	f.ctx = context.WithValue(f.ctx, artifactAccessScopeKey, &ArtifactAccessScope{TaskID: item.ID, Attempt: 1, AuthorizedRefs: []ArtifactRef{artifact.ArtifactRef}})
	view := tools.NewViewTool(tools.WithArtifactOpener(c.openArtifactRef))
	input, _ := json.Marshal(map[string]any{"artifact_ref": artifact.ID, "byte_limit": 1024})
	call := fantasy.ToolCall{ID: "read-1", Name: "view", Input: string(input)}
	response, err := view.Run(f.ctx, call)
	if err != nil || response.IsError {
		t.Fatalf("view: %v %s", err, response.Content)
	}
	if err := c.captureDecisionArtifactRead(f.ctx, view, call, &response); err != nil {
		t.Fatal(err)
	}
	f.plans, err = c.fixedCollectionPlans(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *boundDecisionFixture) call(t *testing.T, index int, id string) fantasy.ToolResponse {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"collection": "/decisions", "row_key": f.plans[0].rows[index].Key, "group": "assessment"})
	response, err := (&decisionPrimitiveTool{coordinator: f.c}).Run(f.ctx, fantasy.ToolCall{ID: id, Name: decisionPrimitiveToolName, Input: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func (f *boundDecisionFixture) submission(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	rows := map[string]any{}
	for _, row := range f.plans[0].rows {
		rows[row.Key] = map[string]any{"reason": "observed evidence"}
	}
	if mutate != nil {
		mutate(rows)
	}
	raw, err := json.Marshal(map[string]any{"decisions": rows})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBoundDecisionCoverageReplayAndAssembly(t *testing.T) {
	for _, count := range []int{0, 1, 32} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newBoundDecisionFixture(t, count)
			for i := range count {
				if response := f.call(t, i, fmt.Sprint(i)); response.IsError {
					t.Fatal(response.Content)
				}
			}
			for i := range count {
				if response := f.call(t, i, "replay-"+fmt.Sprint(i)); response.IsError {
					t.Fatal(response.Content)
				}
			}
			if f.calls.Load() != int32(count) {
				t.Fatalf("model calls=%d", f.calls.Load())
			}
			f.c.SetBudget(1, 0)
			f.c.sessionTime = time.Now().Add(-time.Hour)
			for i := range count {
				if response := f.call(t, i, "exhausted-"+fmt.Sprint(i)); response.IsError {
					t.Fatal(response.Content)
				}
			}
			payload, rejection := f.c.structuredPayloadForSubmission(f.item.ID, f.submission(t, nil))
			if rejection != "" {
				t.Fatal(rejection)
			}
			if len(payload.CollectionBindings[0].Decisions) != count {
				t.Fatal("missing decision provenance")
			}
			if !strings.Contains(string(payload.Value), `"state":"reject"`) && count > 0 {
				t.Fatal(string(payload.Value))
			}
			if err := f.c.validateSubmittedEvidenceInputs(f.item.ID, payload); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBoundDecisionRejectsInvalidSubmission(t *testing.T) {
	f := newBoundDecisionFixture(t, 2)
	if r := f.call(t, 0, "first"); r.IsError {
		t.Fatal(r.Content)
	}
	if _, err := f.c.structuredPayloadForSubmission(f.item.ID, f.submission(t, nil)); !strings.Contains(err, "decision_missing") {
		t.Fatal(err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("repair inferred")
	}
	if r := f.call(t, 1, "second"); r.IsError {
		t.Fatal(r.Content)
	}
	for _, name := range []string{"missing", "unknown", "owned", "identity", "reason"} {
		t.Run(name, func(t *testing.T) {
			raw := f.submission(t, func(rows map[string]any) {
				key := f.plans[0].rows[0].Key
				row := rows[key].(map[string]any)
				switch name {
				case "missing":
					delete(rows, key)
				case "unknown":
					rows["fake"] = row
				case "owned":
					row["state"] = "reject"
				case "identity":
					row["position"] = 0
				case "reason":
					delete(row, "reason")
				}
			})
			if payload, rejection := f.c.structuredPayloadForSubmission(f.item.ID, raw); payload != nil || rejection == "" {
				t.Fatal("accepted invalid submission")
			}
		})
	}
	raw, _ := json.Marshal(map[string]any{"name": "classify", "context": map[string]any{}})
	response, err := (&decisionPrimitiveTool{coordinator: f.c}).Run(f.ctx, fantasy.ToolCall{ID: "legacy", Input: string(raw)})
	if err != nil || !response.IsError {
		t.Fatal("legacy bypass")
	}
}

func TestBoundDecisionConcurrentAndDrift(t *testing.T) {
	f := newBoundDecisionFixture(t, 1)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			response := f.call(t, 0, fmt.Sprint(i))
			if response.IsError {
				t.Error(response.Content)
			}
		})
	}
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Fatalf("duplicate inference: %d", f.calls.Load())
	}
	payload, rejection := f.c.structuredPayloadForSubmission(f.item.ID, f.submission(t, nil))
	if rejection != "" {
		t.Fatal(rejection)
	}
	payload.CollectionBindings[0].Decisions[0].EventID = "forged"
	if err := f.c.validateSubmittedEvidenceInputs(f.item.ID, payload); err == nil {
		t.Fatal("forged provenance accepted")
	}
	source := f.sources[0]
	original := source.TypedResult.StructuredPayload
	raw := []byte(`{"entries":[{"id":"different-same-count"}]}`)
	replacement, err := validateStructuredResultPayload(f.c.session.ResultContracts["assets.json"], original.Contract, raw)
	if err != nil {
		t.Fatal(err)
	}
	source.TypedResult.StructuredPayload = replacement
	if r := f.call(t, 0, "changed"); !r.IsError {
		t.Fatal("stale row accepted")
	}
	if f.calls.Load() != 1 {
		t.Fatal("stale binding inferred")
	}
}

func TestBoundDecisionInterruptedNeverRetries(t *testing.T) {
	f := newBoundDecisionFixture(t, 1)
	events, err := f.c.readActiveLineageEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := invocationMetadataFromContext(f.ctx)
	scope := decisionPrimitiveScope(events, f.c.activeBranchID(), metadata)
	compiled := f.c.session.ResultContracts["decisions.json"]
	group := f.plans[0].spec.DecisionGroups[0]
	binding := newResultDecisionBinding(scope, compiled, f.plans[0], f.plans[0].rows[0], group)
	raw, err := f.c.buildDecisionContext(f.ctx, events, scope, compiled, f.plans[0].rows[0], group.Context.MaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	binding.Context, err = f.c.putDecisionArtifact(f.ctx, metadata, "decision_context", raw)
	if err != nil {
		t.Fatal(err)
	}
	values, err := resultDecisionRequestContext(binding, raw)
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := f.c.decisionPrimitives.Request("classify", "worker", values)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := decisionrt.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	p := decisionPrimitivePayload{Version: 2, Name: "classify", Scope: scope, CatalogHash: f.c.decisionPrimitives.Hash(), RequestDigest: digest, CallID: "crashed", Binding: &binding}
	if err := f.c.appendDecisionPrimitiveEvent(f.ctx, metadata, EventDecisionPrimitiveStarted, p, binding.Slot+":start"); err != nil {
		t.Fatal(err)
	}
	if r := f.call(t, 0, "resume"); !r.IsError || !strings.Contains(r.Content, "interrupted") {
		t.Fatal(r.Content)
	}
	if f.calls.Load() != 0 {
		t.Fatal("interrupted slot retried")
	}
}

func TestDecisionPrimitiveNoNetKeepsInferenceGrants(t *testing.T) {
	f := newBoundDecisionFixture(t, 1)
	f.c.noNet = true
	f.c.session.Agents["worker"].NoNet = true
	f.ctx = context.WithValue(f.ctx, tools.AgentNetworkBlockKey, true)
	if !staticDecisionPrimitiveGrant(f.c.session, f.c.session.Agents["worker"], EffectiveTeamContractContext{NoNet: true}) {
		t.Fatal("static no-net denied inference")
	}
	if r := f.call(t, 0, "offline-tools"); r.IsError {
		t.Fatal(r.Content)
	}
	f.c.forceMCP = true
	if r := f.call(t, 0, "force-mcp"); !r.IsError {
		t.Fatal("force-mcp bypass")
	}
}

func TestBoundDecisionBlockedEnvelopeDoesNotRequireSettledPayload(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprint(gated), func(t *testing.T) {
			f := newBoundDecisionFixture(t, 1)
			ctx := tools.SetToolsAllowed(f.ctx, []string{"view", decisionPrimitiveToolName, submitResultToolName})
			var tool fantasy.AgentTool = &submitResultTool{coordinator: f.c, todoID: f.item.ID}
			if gated {
				tool = f.c.gatePolicyTools([]fantasy.AgentTool{tool})[0]
			}
			// The schema must permit a truthful failure without forcing an audit
			// payload that cannot exist after an unsettled decision.
			input := `{"status":"blocked","summary":"Required decision could not settle"}`
			if err := validateToolArguments(input, tool.Info()); err != nil {
				t.Fatal(err)
			}
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
			if err != nil || response.IsError {
				t.Fatalf("blocked result: %#v %v", response, err)
			}
			result := f.c.GetTaskResult(f.item.ID)
			if result == nil || result.Status != TaskResultStatusBlocked || result.StructuredPayload != nil || taskResultStatusIsSuccessful(result.Status) {
				t.Fatalf("result=%+v", result)
			}
			receipt := ExecutionReceipt{}
			f.c.applyResultContractReceipt(f.item.ID, result, &receipt)
			if receipt.ResultValidation != ResultValidationAbsent || receipt.ResultPayloadSHA256 != "" {
				t.Fatalf("blocked receipt claims validated payload: %+v", receipt)
			}
			source := *f.item
			source.TypedResult = result
			if err := f.c.validateAcceptedDependencyPayload(&source); err == nil {
				t.Fatal("blocked envelope became accepted dependency evidence")
			}
		})
	}
}

func TestBoundDecisionBlockedExceptionDoesNotRelaxSuccessfulPayloads(t *testing.T) {
	for _, input := range []string{
		`{"status":"success","summary":"done"}`,
		`{"status":"completed_with_gaps","summary":"done"}`,
		`{"status":"partial","summary":"unfinished"}`,
		`{"status":"blocked","summary":"cannot settle","structured_payload":{"decisions":{}}}`,
	} {
		f := newBoundDecisionFixture(t, 1)
		response, err := (&submitResultTool{coordinator: f.c, todoID: f.item.ID}).Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
		if err != nil || !response.IsError || f.c.GetTaskResult(f.item.ID) != nil {
			t.Fatalf("invalid payload accepted: %s, %#v %v", input, response, err)
		}
	}
	f := newBoundDecisionFixture(t, 1)
	f.item.ResultContract.SchemaSHA256 = strings.Repeat("0", 64)
	response, err := (&submitResultTool{coordinator: f.c, todoID: f.item.ID}).Run(f.ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"blocked","summary":"cannot settle"}`})
	if err != nil || !response.IsError || f.c.GetTaskResult(f.item.ID) != nil {
		t.Fatalf("blocked bypassed contract drift: %#v %v", response, err)
	}
}

func TestBoundDecisionSearchContextIncludesOnlyShownExcerpts(t *testing.T) {
	f := newBoundDecisionFixture(t, 1)
	artifacts, err := NewFileArtifactStore(f.c.session.Workspace, f.c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("UNSEEN sentinel\n" + strings.Repeat("source padding\n", 30000) + "specific symbol\n")
	ref, err := artifacts.Put(t.Context(), PutArtifactRequest{Kind: "test", Path: "large.txt", Content: content, TaskID: f.sources[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	scope, _ := artifactAccessScopeFromContext(f.ctx)
	next := cloneArtifactAccessScope(scope)
	next.AuthorizedRefs = append(next.AuthorizedRefs, ref.ArtifactRef)
	f.ctx = context.WithValue(f.ctx, artifactAccessScopeKey, next)
	view := f.c.gatePolicyTools([]fantasy.AgentTool{tools.NewViewTool(tools.WithArtifactOpener(f.c.openArtifactRef))})[0]
	input, _ := json.Marshal(map[string]any{"artifact_ref": ref.ID, "search_text": "specific symbol", "context_bytes": 32})
	response, err := view.Run(f.ctx, fantasy.ToolCall{Name: "view", ID: "search-large", Input: string(input)})
	if err != nil || response.IsError || strings.Contains(response.Content, "UNSEEN sentinel") {
		t.Fatalf("search=%#v %v", response, err)
	}
	if response := f.call(t, 0, "decide-after-search"); response.IsError {
		t.Fatal(response.Content)
	}
	if f.calls.Load() != 1 {
		t.Fatal("search did not reach bounded decision inference")
	}
}

type boundDecisionBlockedWorker struct {
	c     *Coordinator
	calls int
}

func (a *boundDecisionBlockedWorker) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	id, _ := ctx.Value(todoIDKey{}).(string)
	response, err := (&submitResultTool{coordinator: a.c, todoID: id}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"blocked","summary":"Required helper could not settle"}`})
	if err != nil || response.IsError {
		return nil, fmt.Errorf("blocked submission: %s (%v)", response.Content, err)
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "blocked result submitted"}}}}, nil
}
func (a *boundDecisionBlockedWorker) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt})
}

func TestBoundDecisionBlockedActualWorkerStopsWithoutProtocolRepair(t *testing.T) {
	f := newBoundDecisionFixture(t, 1)
	def := f.c.session.Agents["worker"]
	def.Generation.Model = "test"
	def.SideEffect = "none"
	def.MaxRetries = -1
	worker := &boundDecisionBlockedWorker{c: f.c}
	f.c.workerAgentOverride = worker
	if _, err := f.c.admitTaskOccurrence(t.Context(), f.item, f.item.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.executeTask(t.Context(), taskDefFromTodoItem(f.item), f.item.ID); err == nil {
		t.Fatal("blocked worker became successful")
	}
	item := f.c.todoItemByID(f.item.ID)
	if worker.calls != 1 || item.Status == TaskDone || item.ExecutionReceipt == nil {
		t.Fatalf("calls=%d item=%+v", worker.calls, item)
	}
	if item.ExecutionReceipt.RepairProvenance != nil && item.ExecutionReceipt.RepairProvenance.Attempted {
		t.Fatal("valid blocked envelope caused protocol repair")
	}
	if !strings.Contains(f.c.taskFinalizationBinding(f.item.ID, false), "OMIT structured_payload") {
		t.Fatal("wrap-up guidance demands an impossible payload")
	}
}

func TestBoundDecisionConditionalClaimsAreRepairable(t *testing.T) {
	f := newBoundDecisionFixture(t, 1, func(root map[string]any) {
		item := root["properties"].(map[string]any)["decisions"].(map[string]any)["items"].(map[string]any)
		item["properties"].(map[string]any)["reason"] = map[string]any{"type": "string"}
		item["allOf"] = []any{map[string]any{
			"if":   map[string]any{"properties": map[string]any{"state": map[string]any{"const": "reject"}}},
			"then": map[string]any{"properties": map[string]any{"reason": map[string]any{"const": ""}}},
		}}
	})
	if response := f.call(t, 0, "decision"); response.IsError {
		t.Fatal(response.Content)
	}
	settled, err := f.c.resolveDecisionCollectionPlans(f.ctx, f.item.ID, f.plans)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := resultFixedCollectionContextForStage(settled, true, true)
	if err != nil || !strings.Contains(manifest, `"state":"reject"`) || !strings.Contains(manifest, `"canonical_item_schema"`) || !strings.Contains(manifest, `"const":""`) {
		t.Fatalf("collector lacks settled value or conditional contract: %v %s", err, manifest)
	}
	payload, message, repairable := f.c.structuredPayloadForAdmission(f.item.ID, f.submission(t, nil))
	if payload != nil || !repairable || !strings.Contains(message, "/decisions/0/reason") {
		t.Fatalf("payload=%v repairable=%v diagnostic=%s", payload, repairable, message)
	}
	valid := f.submission(t, func(rows map[string]any) {
		rows[f.plans[0].rows[0].Key] = map[string]any{"reason": ""}
	})
	payload, message, repairable = f.c.structuredPayloadForAdmission(f.item.ID, valid)
	if payload == nil || message != "" || repairable || !strings.Contains(string(payload.Value), `"state":"reject"`) {
		t.Fatalf("payload=%v repairable=%v diagnostic=%s", payload, repairable, message)
	}
	compiled := f.c.session.ResultContracts["decisions.json"]
	invalidSource := []byte(`{"decisions":[{"origin":5,"position":0,"asset":"a","state":"reject","reason":""}]}`)
	_, sourceErr := validateStructuredResultPayload(compiled, compiled.ref(true), invalidSource)
	if sourceErr == nil || collectionModelSchemaFailure(compiled, f.plans, sourceErr) {
		t.Fatal("trusted source error must not trigger model repair")
	}
}
