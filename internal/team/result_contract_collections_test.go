package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
	"text/template"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

const fixedCollectionTestSchema = `{
 "type":"object","required":["decisions"],"additionalProperties":false,
 "properties":{"decisions":{"type":"array","items":{
  "type":"object","additionalProperties":false,"required":["origin","position","asset","state","reason"],
  "properties":{"origin":{"type":"string"},"position":{"type":"integer"},"asset":{"type":"string"},
   "state":{"type":"string","enum":["accept","reject","uncertain"]},"reason":{"type":"string","minLength":1}}
 }}},
 "x-hufu-fixed-collections":[{"items_pointer":"/decisions","source_contracts":["assets.json"],
  "source_pointer":"/entries","task_id_field":"origin","index_field":"position",
  "value_bindings":{"asset":"/id"},"choice_fields":["state"]}]
}`

func fixedCollectionFixture(t *testing.T, counts ...int) (*Coordinator, []*TodoItem, *TodoItem, []resultFixedCollectionPlan) {
	t.Helper()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "assets.json", `{"type":"object","required":["entries"],"properties":{"entries":{"type":"array","items":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}}}`)
	writeResultContractSchema(t, dir, "decisions.json", fixedCollectionTestSchema)
	c := newBudgetCoordinator(t)
	c.session.ResultContracts = make(map[string]*CompiledResultContract)
	for _, path := range []string{"assets.json", "decisions.json"} {
		compiled, err := compileResultContractSchema(dir, path)
		if err != nil {
			t.Fatal(err)
		}
		c.session.ResultContracts[path] = compiled
	}
	var sources []*TodoItem
	for i, count := range counts {
		contract := c.session.ResultContracts["assets.json"]
		ref := contract.ref(true)
		source := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "producer", ResultContract: &ref}})[0]
		entries := make([]any, 0, count)
		for j := range count {
			entries = append(entries, map[string]any{"id": fmt.Sprintf("asset-%d-%d", i, j)})
		}
		raw, err := json.Marshal(map[string]any{"entries": entries})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := validateStructuredResultPayload(contract, ref, raw)
		if err != nil {
			t.Fatal(err)
		}
		source.Status = TaskDone
		source.TypedResult = &TaskResult{TaskID: source.ID, Agent: source.Agent, Status: TaskResultStatusSuccess, Summary: "accepted inventory", StructuredPayload: payload}
		sources = append(sources, source)
	}
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.ID)
	}
	ref := c.session.ResultContracts["decisions.json"].ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Goal: "classify accepted assets", ResultContract: &ref, EvidenceFrom: ids, Execution: ExecutionContract{RequiresResult: true}}})[0]
	plans, err := c.fixedCollectionPlans(item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return c, sources, item, plans
}

func fixedCollectionSubmission(t *testing.T, plan resultFixedCollectionPlan, mutate func(map[string]any)) string {
	t.Helper()
	decisions := make(map[string]any)
	for _, row := range plan.rows {
		decisions[row.Key] = map[string]any{"state": "uncertain", "reason": "requires semantic review"}
	}
	if mutate != nil {
		mutate(decisions)
	}
	raw, err := json.Marshal(map[string]any{"decisions": decisions})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestFixedCollectionSubmissionRejectsInventedMembershipAndAllowsCorrection(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		for _, mistake := range []string{"52 instead of 51", "missing", "unknown key", "fixed identity", "invalid choice", "missing choice", "array"} {
			t.Run(route+"/"+mistake, func(t *testing.T) {
				c, _, item, plans := fixedCollectionFixture(t, 25, 24, 2)
				valid := fixedCollectionSubmission(t, plans[0], nil)
				invalid := fixedCollectionSubmission(t, plans[0], func(decisions map[string]any) {
					first := plans[0].rows[0].Key
					switch mistake {
					case "52 instead of 51":
						decisions["invented_gap_2"] = decisions[first]
					case "missing":
						delete(decisions, first)
					case "unknown key":
						decisions["invented"] = decisions[first]
						delete(decisions, first)
					case "fixed identity":
						decisions[first].(map[string]any)["origin"] = "foreign-source"
					case "invalid choice":
						decisions[first].(map[string]any)["state"] = "maybe"
					case "missing choice":
						delete(decisions[first].(map[string]any), "state")
					}
				})
				if mistake == "array" {
					invalid = `{"decisions":[]}`
				}
				ctx := occurrenceTestContext(c, item.ID, 1)
				for _, raw := range []string{invalid, valid} {
					wantValid := raw == valid
					var payload *ResultPayload
					if route == "local" {
						response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"classified assets","structured_payload":` + raw + `}`})
						if err != nil || response.IsError == wantValid {
							t.Fatalf("valid=%v response=%+v err=%v", wantValid, response, err)
						}
						if !wantValid && !strings.Contains(response.Content, structuredPayloadInvalidCode) && !strings.Contains(response.Content, "$.structured_payload.decisions") {
							t.Fatalf("missing submission feedback: %s", response.Content)
						}
						if result := c.GetTaskResult(item.ID); result != nil {
							payload = result.StructuredPayload
						}
					} else {
						request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: c.session.ResultContracts["decisions.json"], resultEvidenceInputs: c.dependencyResultsForTask(item.ID)}
						var err error
						payload, err = canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(raw)}, nil)
						if (err == nil) != wantValid {
							t.Fatalf("valid=%v error=%v", wantValid, err)
						}
					}
					if !wantValid {
						if payload != nil || c.GetTaskResult(item.ID) != nil {
							t.Fatal("rejected decisions were published")
						}
						continue
					}
					value, err := decodeResultContractJSON(payload.Value)
					if err != nil {
						t.Fatal(err)
					}
					rows := value.(map[string]any)["decisions"].([]any)
					if len(rows) != 51 || len(payload.CollectionBindings) != 1 || fmt.Sprint(rows[50].(map[string]any)["position"]) != "1" {
						t.Fatalf("runtime did not assemble exactly the original 51 rows: %s", payload.Value)
					}
					if err := c.validateSubmittedEvidenceInputs(item.ID, payload); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestFixedCollectionKeysBindSourceIdentityAndContent(t *testing.T) {
	c, sources, item, plans := fixedCollectionFixture(t, 1)
	raw := fixedCollectionSubmission(t, plans[0], nil)
	payload, rejection := c.structuredPayloadForSubmission(item.ID, []byte(raw))
	if rejection != "" {
		t.Fatal(rejection)
	}
	// Preserve count, index and source ID, but change the accepted evidence.
	source := sources[0]
	changed, err := validateStructuredResultPayload(c.session.ResultContracts["assets.json"], *source.ResultContract, []byte(`{"entries":[{"id":"replacement-asset"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	source.TypedResult.StructuredPayload = changed
	if _, rejection := c.structuredPayloadForSubmission(item.ID, []byte(raw)); rejection == "" {
		t.Fatal("old decision key was reinterpreted against new evidence")
	}
	if err := c.validateSubmittedEvidenceInputs(item.ID, payload); err == nil {
		t.Fatal("persisted result with stale provenance was accepted")
	}
}

func TestFixedCollectionProviderAndPromptExposeOnlyDecisionFields(t *testing.T) {
	c, _, item, plans := fixedCollectionFixture(t, 2)
	compiled := c.session.ResultContracts["decisions.json"]
	before := string(compiled.CanonicalSchema)
	schema := (&submitResultTool{coordinator: c, todoID: item.ID}).Info().Parameters["structured_payload"].(map[string]any)
	raw := fixedCollectionSubmission(t, plans[0], nil)
	if _, err := validateDynamicArguments(schema, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	row := schema["properties"].(map[string]any)["decisions"].(map[string]any)["properties"].(map[string]any)[plans[0].rows[0].Key].(map[string]any)
	props := row["properties"].(map[string]any)
	if props["origin"] != nil || props["position"] != nil || props["asset"] != nil || props["state"] == nil {
		t.Fatalf("submission schema exposes runtime-owned fields: %#v", props)
	}
	prompt, err := resultFixedCollectionContext(plans)
	if err != nil || !strings.Contains(prompt, plans[0].rows[0].Key) || !strings.Contains(prompt, `"asset":"asset-0-0"`) || !strings.Contains(prompt, `"uncertain"`) {
		t.Fatalf("missing fixed inventory/choices: err=%v prompt=%s", err, prompt)
	}
	if string(compiled.CanonicalSchema) != before {
		t.Fatal("submission guidance changed the pinned canonical schema")
	}
}

type fixedCollectionCaptureAgent struct {
	c      *Coordinator
	prompt string
}

func (a *fixedCollectionCaptureAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.prompt = call.Prompt
	id, _ := ctx.Value(todoIDKey{}).(string)
	plans, err := a.c.fixedCollectionPlans(id)
	if err != nil {
		return nil, err
	}
	decisions := make(map[string]any)
	for _, row := range plans[0].rows {
		decisions[row.Key] = map[string]any{"state": "uncertain", "reason": "insufficient evidence"}
	}
	args, err := json.Marshal(map[string]any{"status": "success", "summary": "classified assets", "structured_payload": map[string]any{"decisions": decisions}})
	if err != nil {
		return nil, err
	}
	response, err := (&submitResultTool{coordinator: a.c, todoID: id}).Run(ctx, fantasy.ToolCall{Input: string(args)})
	if err != nil || response.IsError {
		return nil, fmt.Errorf("submit decision: %s (%w)", response.Content, err)
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "submitted"}}}}, nil
}

func (a *fixedCollectionCaptureAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt})
}

func TestFixedCollectionActualWorkerDispatchAndCompletion(t *testing.T) {
	c, _, item, plans := fixedCollectionFixture(t, 2)
	c.session.Workspace = t.TempDir()
	c.session.Agents = map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker", Tools: "view", SideEffect: "none", Generation: agent.GenerationParams{Model: "test"}, MaxRetries: -1}}
	worker := &fixedCollectionCaptureAgent{c: c}
	c.workerAgentOverride = worker
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(worker.prompt, "Runtime Fixed Collections") || !strings.Contains(worker.prompt, plans[0].rows[0].Key) {
		t.Fatal("actual worker prompt omitted the runtime collection manifest")
	}
	result := c.todoItemByID(item.ID)
	if result.Status != TaskDone || result.TypedResult == nil || len(result.TypedResult.StructuredPayload.CollectionBindings) != 1 {
		t.Fatalf("canonical assembled result did not complete: %+v", result)
	}
}

func TestFixedCollectionProvenanceSurvivesSQLiteReplay(t *testing.T) {
	c, sources, item, _ := fixedCollectionFixture(t, 2)
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-fixed", "session-fixed")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore, c.executionRunID = store, "run-fixed"
	for _, source := range sources {
		raw, err := json.Marshal(c.taskTransitionPayloadWithCoordinator(source))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Append(RunEvent{Type: string(EventTaskCompleted), TaskID: source.ID, Actor: source.Agent, Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	c.session.Workspace = t.TempDir()
	c.session.Agents = map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker", Tools: "view", SideEffect: "none", Generation: agent.GenerationParams{Model: "test"}, MaxRetries: -1}}
	c.workerAgentOverride = &fixedCollectionCaptureAgent{c: c}
	if _, err := c.admitTaskOccurrence(t.Context(), item, item.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal(err)
	}
	original := c.todoItemByID(item.ID).TypedResult.StructuredPayload
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewEventStore(workspace, "run-fixed", "session-fixed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	c.taskTracker.TodoList().Restore(replayed)
	restored := c.todoItemByID(item.ID)
	if restored == nil || restored.TypedResult == nil || restored.TypedResult.StructuredPayload.SHA256 != original.SHA256 || len(restored.TypedResult.StructuredPayload.CollectionBindings) != 1 {
		t.Fatalf("SQLite replay lost canonical decisions/provenance: %+v", restored)
	}
	if restored.ExecutionReceipt == nil || restored.ExecutionReceipt.ResultPayloadSHA256 != original.SHA256 {
		t.Fatal("receipt did not bind the assembled canonical array")
	}
	if err := c.validateCompletedEvidenceDeliveries(); err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts["decisions.json"].finalReport = template.Must(template.New("fixed").Parse("{{len .decisions}} rows"))
	if report, selected, err := c.renderedContractFinalReport(); err != nil || !selected || !strings.Contains(report, "2 rows") {
		t.Fatalf("replayed report failed: %q selected=%v err=%v", report, selected, err)
	}
	source := c.todoItemByID(sources[0].ID)
	source.TypedResult.StructuredPayload, err = validateStructuredResultPayload(c.session.ResultContracts["assets.json"], *source.ResultContract, []byte(`{"entries":[{"id":"changed"},{"id":"asset-0-1"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.taskTracker.TodoList().SetTypedResult(source.ID, source.TypedResult); err != nil {
		t.Fatal(err)
	}
	if err := c.validateCompletedEvidenceDeliveries(); err == nil {
		t.Fatal("replayed completion accepted changed source content")
	}
	if _, _, err := c.renderedContractFinalReport(); err == nil {
		t.Fatal("final rendering accepted decisions for changed sources")
	}
}

func TestFixedCollectionRejectsDuplicateKeysBeforeNormalization(t *testing.T) {
	c, _, item, plans := fixedCollectionFixture(t, 1)
	key := plans[0].rows[0].Key
	raw := fmt.Sprintf(`{"decisions":{%q:{"state":"accept","reason":"first"},%q:{"state":"reject","reason":"second"}}}`, key, key)
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Input: `{"status":"success","summary":"reviewed","structured_payload":` + raw + `}`})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "duplicate") || c.GetTaskResult(item.ID) != nil {
		t.Fatalf("duplicate decisions survived normalization: response=%+v err=%v", response, err)
	}
	request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: c.session.ResultContracts["decisions.json"], resultEvidenceInputs: c.dependencyResultsForTask(item.ID)}
	if _, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: &raw}, nil); err == nil {
		t.Fatal("external duplicate keys were accepted")
	}
}

func TestFixedCollectionCommitRejectsForgedCanonicalRows(t *testing.T) {
	for _, mistake := range []string{"provenance", "identity", "extra row"} {
		t.Run(mistake, func(t *testing.T) {
			c, _, item, plans := fixedCollectionFixture(t, 1)
			raw := fixedCollectionSubmission(t, plans[0], nil)
			payload, rejection := c.structuredPayloadForSubmission(item.ID, []byte(raw))
			if rejection != "" {
				t.Fatal(rejection)
			}
			if mistake == "provenance" {
				payload.CollectionBindings = nil
			} else {
				value, err := decodeResultContractJSON(payload.Value)
				if err != nil {
					t.Fatal(err)
				}
				rows := value.(map[string]any)["decisions"].([]any)
				if mistake == "identity" {
					rows[0].(map[string]any)["origin"] = "foreign"
				} else {
					value.(map[string]any)["decisions"] = append(rows, rows[0])
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				forged, err := validateStructuredResultPayload(c.session.ResultContracts["decisions.json"], *item.ResultContract, encoded)
				if err != nil {
					t.Fatal(err)
				}
				forged.CollectionBindings = payload.CollectionBindings
				payload = forged
			}
			ctx := occurrenceTestContext(c, item.ID, 1)
			err := (coordinatorTaskResultSink{coordinator: c}).Submit(ctx, item.ID, TaskResult{Status: TaskResultStatusSuccess, Summary: "canonical forgery", StructuredPayload: payload})
			if err == nil || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("commit accepted forged runtime collection: %v", err)
			}
		})
	}
}

func TestFixedCollectionDeclarationPreflight(t *testing.T) {
	for _, mistake := range []string{"unknown field", "empty", "null", "bad target", "missing fixed", "duplicate fixed", "missing choice", "optional choice", "uppercase", "overlap", "missing source"} {
		t.Run(mistake, func(t *testing.T) {
			root, err := decodeResultContractJSON([]byte(fixedCollectionTestSchema))
			if err != nil {
				t.Fatal(err)
			}
			v := root.(map[string]any)
			spec := v["x-hufu-fixed-collections"].([]any)[0].(map[string]any)
			switch mistake {
			case "unknown field":
				spec["guess_from_prose"] = true
			case "empty":
				v["x-hufu-fixed-collections"] = []any{}
			case "null":
				v["x-hufu-fixed-collections"] = nil
			case "bad target":
				spec["items_pointer"] = "/missing"
			case "missing fixed":
				spec["task_id_field"] = "missing"
			case "duplicate fixed":
				spec["index_field"] = "origin"
			case "missing choice":
				spec["choice_fields"] = []any{}
			case "optional choice", "uppercase":
				item := v["properties"].(map[string]any)["decisions"].(map[string]any)["items"].(map[string]any)
				if mistake == "optional choice" {
					item["required"] = []any{"origin", "position", "asset", "reason"}
				} else {
					item["properties"].(map[string]any)["state"].(map[string]any)["enum"] = []any{"ACCEPT", "reject"}
				}
			case "overlap":
				v["x-hufu-fixed-collections"] = []any{spec, spec}
			case "missing source":
				spec["source_contracts"] = []any{}
			}
			if _, err := compileResultFixedCollections(v); err == nil {
				t.Fatal("invalid fixed collection contract accepted")
			}
		})
	}
	c, _, _, _ := fixedCollectionFixture(t, 0)
	if err := validateFixedCollectionSourceContracts(map[string]*CompiledResultContract{"decisions.json": c.session.ResultContracts["decisions.json"]}); err == nil {
		t.Fatal("unloaded source contract was accepted")
	}
}

func TestFixedCollectionEmptyAndMissingEvidenceAreDistinct(t *testing.T) {
	c, _, item, plans := fixedCollectionFixture(t, 0)
	if _, rejection := c.structuredPayloadForSubmission(item.ID, []byte(fixedCollectionSubmission(t, plans[0], nil))); rejection != "" {
		t.Fatal(rejection)
	}
	if _, err := buildResultFixedCollections(c.session.ResultContracts["decisions.json"].fixedCollections, nil, nil); err == nil {
		t.Fatal("missing evidence was guessed to be an empty inventory")
	}
}

func TestFixedCollectionSupportsTranslationSegmentsWithoutIdentityRetyping(t *testing.T) {
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "segments.json", `{"type":"object","properties":{"segments":{"type":"array","items":{"type":"string"}}}}`)
	writeResultContractSchema(t, dir, "translations.json", `{
 "type":"object","required":["translations"],"properties":{"translations":{"type":"array","items":{
  "type":"object","additionalProperties":false,"required":["segment","decision","text"],
  "properties":{"segment":{"type":"string"},"decision":{"type":"string","enum":["translate","keep","uncertain"]},"text":{"type":"string"}}
 }}},
 "x-hufu-fixed-collections":[{"items_pointer":"/translations","source_contracts":["segments.json"],"source_pointer":"/segments","value_bindings":{"segment":""},"choice_fields":["decision"]}]
}`)
	source, err := compileResultContractSchema(dir, "segments.json")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compileResultContractSchema(dir, "translations.json")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := validateStructuredResultPayload(source, source.ref(true), []byte(`{"segments":["原始段落","Next paragraph"]}`))
	if err != nil {
		t.Fatal(err)
	}
	results := []TaskResult{{TaskID: "document", Status: TaskResultStatusSuccess, StructuredPayload: payload}}
	plans, err := buildResultFixedCollections(compiled.fixedCollections, []string{"document"}, results)
	if err != nil {
		t.Fatal(err)
	}
	decisions := map[string]any{
		plans[0].rows[0].Key: map[string]any{"decision": "translate", "text": "Original paragraph"},
		plans[0].rows[1].Key: map[string]any{"decision": "keep", "text": "Next paragraph"},
	}
	raw, err := json.Marshal(map[string]any{"translations": decisions})
	if err != nil {
		t.Fatal(err)
	}
	assembled, bindings, err := assembleResultFixedCollections(raw, plans)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := validateStructuredResultPayload(compiled, compiled.ref(true), assembled)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeResultContractJSON(canonical.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateResultFixedCollections(value, bindings, plans); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical.Value), `"segment":"原始段落"`) {
		t.Fatal("runtime did not preserve source text independently of the model's translation")
	}
}

func TestFixedCollectionLimitsFailBeforeModelExecution(t *testing.T) {
	c, sources, item, _ := fixedCollectionFixture(t, 0)
	entries := make([]any, maxResultFixedCollectionRows+1)
	for i := range entries {
		entries[i] = map[string]any{"id": "a"}
	}
	raw, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		t.Fatal(err)
	}
	sources[0].TypedResult.StructuredPayload, err = validateStructuredResultPayload(c.session.ResultContracts["assets.json"], *sources[0].ResultContract, raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.fixedCollectionPlans(item.ID); err == nil {
		t.Fatal("oversized fixed inventory was accepted")
	}
	_, _, _, plans := fixedCollectionFixture(t, 300)
	if _, err := resultFixedCollectionContext(plans); err == nil {
		t.Fatal("oversized manifest was silently truncated")
	}
}
