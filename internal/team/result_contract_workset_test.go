package team

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"charm.land/fantasy"
)

const worksetRowsSchema = `{"type":"object","required":["rows"],"properties":{"rows":{"type":"array","items":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}},"x-hufu-workset-inputs":{"items_pointer":"/rows","item_id_pointer":"/id","binding":"target_ids"}}`

func worksetResultFixture(t *testing.T, ids string) (*Coordinator, *TodoItem, *CompiledResultContract) {
	t.Helper()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "rows.json", worksetRowsSchema)
	compiled, err := compileResultContractSchema(dir, "rows.json")
	if err != nil {
		t.Fatal(err)
	}
	c, item := resultContractCoordinator(t, true)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	item.ResultContract = &ref
	item.WorksetBinding = &WorksetBinding{ItemKey: "batch-a", Bindings: map[string]string{"target_ids": ids}}
	return c, item, compiled
}

func TestWorksetInputExtensionPreflight(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"items_pointer":"/rows","item_id_pointer":"/id","binding":""}`, `{"items_pointer":"rows","item_id_pointer":"/id","binding":"ids"}`, `{"items_pointer":"/rows","item_id_pointer":"/id","binding":"ids","unknown":true}`} {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		if _, err := compileResultWorksetInputs(map[string]any{"x-hufu-workset-inputs": v}); err == nil {
			t.Fatalf("invalid extension accepted: %s", raw)
		}
	}
	c, _, compiled := worksetResultFixture(t, `[]`)
	if compiled.worksetInputs == nil || len(c.session.ResultContracts) != 1 {
		t.Fatal("extension was not compiled")
	}
}

func TestWorksetInputOrderedCoverage(t *testing.T) {
	spec := &resultWorksetInputsSpec{ItemsPointer: "/rows", ItemIDPointer: "/id", Binding: "target_ids"}
	for _, tc := range []struct {
		name, ids, value string
		valid            bool
	}{
		{"asset inventory", `["asset-a","asset-b"]`, `{"rows":[{"id":"asset-a","state":"checked"},{"id":"asset-b"}]}`, true},
		{"translation segments", `["segment-1"]`, `{"rows":[{"id":"segment-1","text":"translated"}]}`, true},
		{"empty", `[]`, `{"rows":[]}`, true},
		{"foreign", `[]`, `{"rows":[{"id":"other"}]}`, false},
		{"missing", `["a"]`, `{"rows":[]}`, false},
		{"substituted", `["a"]`, `{"rows":[{"id":"b"}]}`, false},
		{"reordered", `["a","b"]`, `{"rows":[{"id":"b"},{"id":"a"}]}`, false},
		{"duplicated", `["a","b"]`, `{"rows":[{"id":"a"},{"id":"a"}]}`, false},
		{"null result", `[]`, `{"rows":null}`, false},
		{"missing result", `[]`, `{}`, false},
		{"null binding", `null`, `{"rows":[]}`, false},
		{"duplicate binding", `["a","a"]`, `{"rows":[{"id":"a"},{"id":"a"}]}`, false},
		{"empty id binding", `[""]`, `{"rows":[{"id":""}]}`, false},
		{"malformed binding", `[`, `{"rows":[]}`, false},
		{"nonstring binding", `[1]`, `{"rows":[{"id":1}]}`, false},
		{"nonstring result", `["a"]`, `{"rows":[{"id":{}}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, err := decodeResultContractJSON([]byte(tc.value))
			if err != nil {
				t.Fatal(err)
			}
			binding := &WorksetBinding{Bindings: map[string]string{"target_ids": tc.ids}}
			err = validateResultWorksetInputs(v, spec, binding)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	if err := validateResultWorksetInputs(map[string]any{}, spec, nil); err == nil {
		t.Fatal("unbound result accepted")
	}
	if err := validateResultWorksetInputs(map[string]any{}, spec, &WorksetBinding{}); err == nil {
		t.Fatal("missing binding accepted")
	}
}

func TestWorksetSubmissionRejectsBeforePublicationAndAllowsCorrection(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			c, item, compiled := worksetResultFixture(t, `[]`)
			ctx := occurrenceTestContext(c, item.ID, 1)
			for _, valid := range []bool{false, true} {
				value := `{"rows":[{"id":"sibling-only"}]}`
				if valid {
					value = `{"rows":[]}`
				}
				if route == "external" {
					request := AttemptRequest{Task: taskDefFromTodoItem(item), resultContractSchema: compiled}
					_, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(value)}, nil)
					if (err == nil) != valid {
						t.Fatalf("valid=%v error=%v", valid, err)
					}
				} else {
					response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"checked assigned rows","structured_payload":` + value + `}`})
					if err != nil || response.IsError == valid {
						t.Fatalf("valid=%v response=%#v error=%v", valid, response, err)
					}
					if !valid && (c.GetTaskResult(item.ID) != nil || item.TypedResult != nil) {
						t.Fatal("rejected result was published")
					}
					if !valid && !strings.Contains(response.Content, "requires exactly 0 items") {
						t.Fatalf("missing corrective feedback: %s", response.Content)
					}
					if valid && c.GetTaskResult(item.ID) == nil {
						t.Fatal("corrected result was not accepted")
					}
				}
			}
		})
	}
}

func TestWorksetResultReplayAndDependenciesUseDurableBinding(t *testing.T) {
	c, item, compiled := worksetResultFixture(t, `["asset-a"]`)
	payload, err := validateStructuredResultPayload(compiled, *item.ResultContract, []byte(`{"rows":[{"id":"asset-a"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	item.TypedResult = &TaskResult{TaskID: item.ID, Agent: item.Agent, Status: TaskResultStatusSuccess, StructuredPayload: payload}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var restored TodoItem
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	c.taskTracker.TodoList().Restore([]*TodoItem{&restored})
	for _, changed := range []bool{false, true} {
		if changed {
			restored.WorksetBinding.Bindings["target_ids"] = `["asset-b"]`
		}
		for _, err := range []error{c.validateRequiredEvidenceDelivery(restored.ID, 1), c.validateAcceptedDependencyPayload(&restored)} {
			if (err != nil) != changed {
				t.Fatalf("changed=%v error=%v", changed, err)
			}
		}
	}
}

func TestWorksetBindingIsCheckedAgainAtOccurrenceCommit(t *testing.T) {
	c, item, compiled := worksetResultFixture(t, `[]`)
	ctx := occurrenceTestContext(c, item.ID, 1)
	for _, valid := range []bool{false, true} {
		value := `{"rows":[{"id":"sibling"}]}`
		if valid {
			value = `{"rows":[]}`
		}
		payload, err := validateStructuredResultPayload(compiled, *item.ResultContract, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		err = (coordinatorTaskResultSink{coordinator: c}).Submit(ctx, item.ID, TaskResult{Status: TaskResultStatusSuccess, Summary: "canonical proposal", StructuredPayload: payload})
		if (err == nil) != valid {
			t.Fatalf("valid=%v error=%v", valid, err)
		}
		if !valid && (c.GetTaskResult(item.ID) != nil || item.TypedResult != nil) {
			t.Fatal("commit published out-of-scope rows")
		}
	}
}
