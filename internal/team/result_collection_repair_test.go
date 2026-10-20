package team

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestFixedCollectionRepairRetainsTheSameManifest(t *testing.T) {
	for _, purpose := range []string{"result_repair", "protocol_repair"} {
		t.Run(purpose, func(t *testing.T) {
			c, _, item, plans := fixedCollectionFixture(t, 2)
			ctx := withTestAuxiliaryInvocationContext(context.WithValue(t.Context(), todoIDKey{}, item.ID))
			invocation, _ := providerBoundInvocationContextFromContext(ctx, "")
			invocation.AdmissionContext.ContextWindow = 100000
			invocation.ModelContext.ContextWindow = 100000
			ctx = withProviderBoundInvocationContext(ctx, invocation)
			prompt, err := c.prepareAuxiliaryPromptWithPersistence(ctx, purpose, "Repair only the rejected decision", false)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{"Runtime Fixed Collections", plans[0].rows[0].Key, `"asset":"asset-0-0"`, `"uncertain"`, `"submission_pointer":"/structured_payload/decisions"`, "complete outer argument object"} {
				if !strings.Contains(prompt, required) {
					t.Fatalf("repair dropped required fixed input %q", required)
				}
			}
		})
	}
}

func TestFixedCollectionGeneratedValuesMustSatisfySchemaBeforeDispatch(t *testing.T) {
	for _, contradiction := range []string{"fixed field", "cardinality"} {
		t.Run(contradiction, func(t *testing.T) {
			c, _, item, _ := fixedCollectionFixture(t, 2)
			schema := strings.Replace(fixedCollectionTestSchema, `"asset":{"type":"string"}`, `"asset":{"type":"integer"}`, 1)
			if contradiction == "cardinality" {
				schema = strings.Replace(fixedCollectionTestSchema, `"decisions":{"type":"array"`, `"decisions":{"type":"array","maxItems":1`, 1)
			}
			dir := t.TempDir()
			writeResultContractSchema(t, dir, "decisions.json", schema)
			compiled, err := compileResultContractSchema(dir, "decisions.json")
			if err != nil {
				t.Fatal(err)
			}
			c.session.ResultContracts[compiled.ID] = compiled
			ref := compiled.ref(true)
			item.ResultContract = &ref
			if _, err := c.fixedCollectionPlans(item.ID); err == nil || !strings.Contains(err.Error(), contradiction) {
				t.Fatalf("static contradiction reached model execution: %v", err)
			}
		})
	}
}

func TestFixedCollectionPayloadClonesDoNotShareProvenance(t *testing.T) {
	c, _, item, plans := fixedCollectionFixture(t, 1)
	payload, rejection := c.structuredPayloadForSubmission(item.ID, []byte(fixedCollectionSubmission(t, plans[0], nil)))
	if rejection != "" {
		t.Fatal(rejection)
	}
	clone := cloneTaskResult(&TaskResult{StructuredPayload: payload})
	clone.StructuredPayload.CollectionBindings[0].Sources[0].TaskID = "other"
	if payload.CollectionBindings[0].Sources[0].TaskID == "other" {
		t.Fatal("a cached result clone changed persisted collection provenance")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var restored ResultPayload
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := c.validateSubmittedEvidenceInputs(item.ID, &restored); err != nil {
		t.Fatal(err)
	}
}

func TestFixedCollectionDuplicateKeysThroughPolicyAndRepairWrappers(t *testing.T) {
	for _, route := range []string{"policy", "repair"} {
		t.Run(route, func(t *testing.T) {
			c, _, item, plans := fixedCollectionFixture(t, 1)
			var tool fantasy.AgentTool = &policyGatedTool{inner: &submitResultTool{coordinator: c, todoID: item.ID}, coordinator: c}
			if route == "repair" {
				tool = newProtocolWrapper(c, tool)
			}
			key := plans[0].rows[0].Key
			input := `{"status":"SUCCESS","summary":"classified","structured_payload":{"decisions":{"` + key + `":{"state":"accept","reason":"first"},"` + key + `":{"state":"reject","reason":"second"}}}}`
			ctx := occurrenceTestContext(c, item.ID, 1)
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
			if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("wrappers erased duplicate decisions: response=%+v err=%v", response, err)
			}
			valid := `{"status":"success","summary":"classified","structured_payload":` + fixedCollectionSubmission(t, plans[0], nil) + `}`
			response, err = tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: valid})
			if err != nil || response.IsError || c.GetTaskResult(item.ID) == nil {
				t.Fatalf("same-attempt correction through wrapper failed: response=%+v err=%v", response, err)
			}
		})
	}
}

func TestFixedCollectionValidatesTransitiveSourceProvenance(t *testing.T) {
	c, sources, parent, plans := fixedCollectionFixture(t, 1)
	payload, rejection := c.structuredPayloadForSubmission(parent.ID, []byte(fixedCollectionSubmission(t, plans[0], nil)))
	if rejection != "" {
		t.Fatal(rejection)
	}
	parent.Status = TaskDone
	parent.TypedResult = &TaskResult{TaskID: parent.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
	dir := t.TempDir()
	schema := strings.NewReplacer(`"source_contracts":["assets.json"]`, `"source_contracts":["decisions.json"]`, `"source_pointer":"/entries"`, `"source_pointer":"/decisions"`, `"asset":"/id"`, `"asset":"/asset"`).Replace(fixedCollectionTestSchema)
	writeResultContractSchema(t, dir, "child.json", schema)
	compiled, err := compileResultContractSchema(dir, "child.json")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	child := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: &ref, EvidenceFrom: []string{parent.ID}}})[0]
	plans, err = c.fixedCollectionPlans(child.ID)
	if err != nil {
		t.Fatal(err)
	}
	childPayload, rejection := c.structuredPayloadForSubmission(child.ID, []byte(fixedCollectionSubmission(t, plans[0], nil)))
	if rejection != "" {
		t.Fatal(rejection)
	}
	child.Status = TaskDone
	child.TypedResult = &TaskResult{TaskID: child.ID, Status: TaskResultStatusSuccess, StructuredPayload: childPayload}
	if err := c.validateAcceptedDependencyPayload(child); err != nil {
		t.Fatal(err)
	}
	source := sources[0]
	source.TypedResult.StructuredPayload, err = validateStructuredResultPayload(c.session.ResultContracts["assets.json"], *source.ResultContract, []byte(`{"entries":[{"id":"replacement"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	// The immediate parent payload and its hash have not changed, but its
	// own source is stale. Consumers must validate the entire fixed lineage.
	if err := c.validateAcceptedDependencyPayload(child); err == nil {
		t.Fatal("transitively stale decisions were accepted as new evidence")
	}
}

func TestFixedCollectionReplayRejectsCyclicProvenance(t *testing.T) {
	c, _, _, _ := fixedCollectionFixture(t, 0)
	dir := t.TempDir()
	schema := strings.NewReplacer(`"source_contracts":["assets.json"]`, `"source_contracts":["cycle.json"]`, `"source_pointer":"/entries"`, `"source_pointer":"/decisions"`, `"asset":"/id"`, `"asset":"/asset"`).Replace(fixedCollectionTestSchema)
	writeResultContractSchema(t, dir, "cycle.json", schema)
	compiled, err := compileResultContractSchema(dir, "cycle.json")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: &ref}})[0]
	item.EvidenceFrom = []string{item.ID}
	raw, err := json.Marshal(map[string]any{"decisions": []any{map[string]any{"origin": item.ID, "position": 0, "asset": "a", "state": "uncertain", "reason": "corrupt replay fixture"}}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := validateStructuredResultPayload(compiled, ref, raw)
	if err != nil {
		t.Fatal(err)
	}
	payload.CollectionBindings = []ResultCollectionBinding{{ItemsPointer: "/decisions", Sources: []ResultCollectionSource{{TaskID: item.ID, PayloadSHA256: payload.SHA256}}}}
	item.Status = TaskDone
	item.TypedResult = &TaskResult{TaskID: item.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}
	if err := c.validateAcceptedDependencyPayload(item); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cyclic persisted evidence did not fail closed: %v", err)
	}
}
