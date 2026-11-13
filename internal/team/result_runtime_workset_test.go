package team

import (
	"encoding/json/v2"
	"strings"
	"testing"
)

const runtimeWorksetSchema = `{"type":"object","properties":{"record":{"$ref":"#/$defs/record"}},"required":["record"],"$defs":{"record":{"type":"object","properties":{"scope":{"type":"string"},"judgment":{"type":"string"}},"required":["scope","judgment"],"additionalProperties":false}},"x-hufu-runtime-inputs":[{"target":"/record/scope","source_kind":"workset_binding","output":"label"}]}`

func runtimeWorksetFixture(t *testing.T) (*Coordinator, *TodoItem, *CompiledResultContract) {
	t.Helper()
	c := newTestCoordinator(t)
	compiled, err := CompileResultContractDocument("workset-result.json", []byte(runtimeWorksetSchema))
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	producer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", PlanTaskID: "producer"}})[0]
	store, err := NewFileArtifactStore(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "item-a", Bindings: map[string]string{"label": "frozen-value"}}}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(t.Context(), PutArtifactRequest{Kind: "workset_manifest", Path: "manifest.json", Content: data, RunID: c.executionRunID, TaskID: producer.ID, Attempt: 1, Agent: producer.Agent})
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{TaskID: producer.ID, Agent: producer.Agent, Attempt: 1, Status: TaskResultStatusSuccess, Source: "runtime", Artifacts: []ArtifactRef{source.ArtifactRef}}
	c.storeSubmittedTaskResult(producer.ID, result)
	c.taskTracker.TodoList().UpdateStatus(producer.ID, TaskDone, "prepared")
	ref := compiled.ref(true)
	children, err := c.expandFanOutTasks([]TaskDef{{ID: "fanout", Agent: "worker", ResultContract: &ref, FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: producer.ID, Artifact: source.ID}, GoalTemplate: "Evaluate {label}"}}})
	if err != nil {
		t.Fatal(err)
	}
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	receipts, err := buildWorksetReceipts(children, ids, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	child := todoItemFromSpec(TodoSpec{Agent: "worker", WorksetBinding: children[0].WorksetBinding, ResultContract: &ref}, ids[0])
	child.WorksetReceipt = receipts[child.WorksetBinding.WorksetID]
	c.taskTracker.TodoList().AddReserved([]*TodoItem{child})
	return c, child, compiled
}

func TestRuntimeResultInputsBindFrozenWorksetValues(t *testing.T) {
	c, item, compiled := runtimeWorksetFixture(t)
	info := (&submitResultTool{coordinator: c, todoID: item.ID}).Info()
	shape := info.Parameters["structured_payload"].(map[string]any)
	recordShape := shape["properties"].(map[string]any)["record"].(map[string]any)
	if _, visible := recordShape["properties"].(map[string]any)["scope"]; visible {
		t.Fatal("runtime-owned workset field advertised to the model")
	}
	for _, external := range []bool{false, true} {
		for _, override := range []bool{true, false} {
			raw := `{"record":{"judgment":"uncertain"}}`
			if override {
				raw = `{"record":{"scope":"frozen-value","judgment":"uncertain"}}`
			}
			var payload *ResultPayload
			var rejection string
			if external {
				proposal := raw
				request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: compiled, resultRuntimeInputs: func() (map[string]any, error) { return c.resultRuntimeInputValues(compiled, item.ID) }}
				var err error
				payload, err = canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: &proposal}, nil)
				if err != nil {
					rejection = err.Error()
				}
			} else {
				payload, rejection = c.structuredPayloadForSubmission(item.ID, []byte(raw))
			}
			if override {
				if payload != nil || !strings.Contains(rejection, "runtime-owned") {
					t.Fatalf("override accepted external=%t: %s", external, rejection)
				}
				continue
			}
			if rejection != "" {
				t.Fatal(rejection)
			}
			value, err := decodeUniqueJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			record := value.(map[string]any)["record"].(map[string]any)
			if record["scope"] != "frozen-value" || record["judgment"] != "uncertain" {
				t.Fatal("runtime changed the judgment or binding")
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var restored ResultPayload
			if err := json.Unmarshal(encoded, &restored); err != nil {
				t.Fatal(err)
			}
			if err := c.validateSubmittedEvidenceInputs(item.ID, &restored); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, scenario := range []string{"unbound", "receipt", "binding", "missing_key", "source"} {
		t.Run(scenario, func(t *testing.T) {
			before := c.taskTracker.TodoList().Items()
			items := c.taskTracker.TodoList().Items()
			child := todoItemByID(items, item.ID)
			defer c.taskTracker.TodoList().Restore(before)
			switch scenario {
			case "unbound":
				child.WorksetBinding = nil
			case "receipt":
				child.WorksetReceipt.Children[child.WorksetBinding.ItemKey] = "another-child"
			case "binding":
				child.WorksetBinding.Bindings["label"] = "forged"
			case "missing_key":
				delete(child.WorksetBinding.Bindings, "label")
			case "source":
				child.WorksetBinding.SourceSHA256 = strings.Repeat("a", 64)
			}
			c.taskTracker.TodoList().Restore(items)
			if _, err := c.resultRuntimeInputValues(compiled, item.ID); err == nil {
				t.Fatal("invalid workset source accepted")
			}
		})
	}
}

func TestRuntimeResultInputsRecoveredWorksetValues(t *testing.T) {
	c := newRecoveredArtifactCoordinator(t)
	compiled, err := CompileResultContractDocument("workset-result.json", []byte(runtimeWorksetSchema))
	if err != nil {
		t.Fatal(err)
	}
	values, err := c.resultRuntimeInputValues(compiled, "2")
	if err != nil || values["/record/scope"] != "frozen-value" {
		t.Fatalf("recovered values=%v err=%v", values, err)
	}
	c.executionRunID = "unadmitted-invocation"
	if _, err := c.resultRuntimeInputValues(compiled, "2"); err == nil {
		t.Fatal("unadmitted invocation borrowed a historical workset")
	}
}

func TestRuntimeResultInputsModelLeafUsesCanonicalWorksetOwner(t *testing.T) {
	parent, item, compiled := runtimeWorksetFixture(t)
	leaf := newTestCoordinator(t)
	leaf.modelFanout = &modelFanoutBinding{parent: parent, taskID: item.ID}
	// A model leaf owns only its worker projection; the parent retains the
	// producer and shared expansion receipt, which can be on another child.
	copy := cloneTodoItem(item)
	copy.WorksetReceipt = nil
	leaf.taskTracker.TodoList().Restore([]*TodoItem{copy})
	values, err := leaf.resultRuntimeInputValues(compiled, item.ID)
	if err != nil || values["/record/scope"] != "frozen-value" {
		t.Fatalf("leaf values=%v err=%v", values, err)
	}
}

func TestRuntimeResultInputsWorksetSchemaBoundaries(t *testing.T) {
	for _, change := range []struct{ name, from, to string }{
		{"source_task", `"source_kind":"workset_binding"`, `"source_kind":"workset_binding","source_task":"another-task"`},
		{"missing_binding", `"output":"label"`, `"output":""`},
		{"external_ref", `"$ref":"#/$defs/record"`, `"$ref":"https://invalid.example/record"`},
		{"cyclic_ref", `"$ref":"#/$defs/record"`, `"$ref":"#/properties/record"`},
		{"ref_sibling", `"$ref":"#/$defs/record"`, `"$ref":"#/$defs/record","properties":{"scope":{"type":"number"}}`},
	} {
		t.Run(change.name, func(t *testing.T) {
			if _, err := CompileResultContractDocument("invalid.json", []byte(strings.Replace(runtimeWorksetSchema, change.from, change.to, 1))); err == nil {
				t.Fatal("invalid workset input schema accepted")
			}
		})
	}
	text := strings.Replace(runtimeWorksetSchema, `"required":["record"]`, `"required":["record"],"description":"stable"`, 1)
	compiled, err := CompileResultContractDocument("alias.json", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeResultContractJSON(compiled.CanonicalSchema)
	if err != nil {
		t.Fatal(err)
	}
	root := value.(map[string]any)
	root["properties"].(map[string]any)["other"] = map[string]any{"$ref": "#/$defs/record"}
	if err := omitRuntimeResultInputs(root, compiled.runtimeInputs); err != nil {
		t.Fatal(err)
	}
	other, err := runtimeInputSchemaAt(root, "/other/scope")
	if err != nil || other["type"] != "string" {
		t.Fatal("runtime omission erased an unrelated shared-reference field")
	}
}

func TestContractRecoveryRejectsUnavailableWorksetRuntimeInput(t *testing.T) {
	c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 14), SideEffectNone)
	compiled := c.session.ResultContracts["research.json"]
	value, err := decodeResultContractJSON(compiled.CanonicalSchema)
	if err != nil {
		t.Fatal(err)
	}
	document := value.(map[string]any)
	document["x-hufu-runtime-inputs"] = []any{map[string]any{"target": "/findings", "source_kind": "workset_binding", "output": "missing"}}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err = CompileResultContractDocument(compiled.ID, data)
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	c.session.ContractTasks[0].ResultContract = &ref
	before := mustReadEvents(t, c.eventStore)
	if _, err := c.AssessContractRecovery(contractCollectionContext(t, id), source); err == nil || !strings.Contains(err.Error(), "runtime inputs") {
		t.Fatalf("unavailable target binding was not refused in preview: %v", err)
	}
	after := mustReadEvents(t, c.eventStore)
	if len(before) != len(after) {
		t.Fatal("rejected preview published events")
	}
}

func TestResultCollectionRecoveryMissingRuntimePayload(t *testing.T) {
	for _, test := range []struct {
		name, body       string
		allowed, corrupt bool
	}{
		{"missing", `{"status":"success","summary":"stored work"}`, true, false},
		{"null", `{"status":"success","summary":"stored work","structured_payload":null}`, true, false},
		{"string_envelope", `{"result":"{\"status\":\"success\"}"}`, true, false},
		{"forged_receipt", `{"status":"success","summary":"stored work","receipt_ids":["invented"]}`, false, false},
		{"changed_binding", `{"status":"success","summary":"stored work"}`, false, true},
		{"malformed_payload", `{"status":"success","summary":"stored work","structured_payload":[]}`, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, item, compiled := runtimeWorksetFixture(t)
			if test.corrupt {
				item.WorksetBinding.Bindings["label"] = "changed"
			}
			var checks []ResultCollectionRecoveryCheck
			check := func(code string, err error) bool {
				checks = append(checks, ResultCollectionRecoveryCheck{Code: code, Passed: err == nil})
				return err == nil
			}
			allowed := c.validateRecoveryIndependentClaims(t.Context(), &ResultCollectionCheckpoint{TaskID: item.ID, Attempt: 1}, &resultCollectionWork{Workspace: c.projectDir}, compiled, test.body, check)
			if allowed != test.allowed {
				t.Fatalf("allowed=%t checks=%+v", allowed, checks)
			}
		})
	}
}
