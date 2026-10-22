package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"
)

func TestRuntimeEvidenceBindingSubmissionAndReplay(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			c, source, audit, _ := reviewedInputsFixture(t)
			compiled := c.session.ResultContracts[audit.ResultContract.ID]
			compiled.evidenceInputs.RuntimeBind = true
			submit := func(raw string) (*ResultPayload, string) {
				if route == "local" {
					return c.structuredPayloadForSubmission(audit.ID, []byte(raw))
				}
				request := AttemptRequest{Task: taskDefFromTodoItem(audit), resultContractSchema: compiled, resultEvidenceInputs: c.dependencyResultsForTask(audit.ID)}
				payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(raw)}, nil)
				if err != nil {
					return nil, err.Error()
				}
				return payload, ""
			}
			for _, raw := range []string{`{"done":true,"reviews":[]}`, `{"done":true,"reviews":null}`, `{"done":"yes"}`, `{"done":"true","done":"false"}`} {
				if payload, rejection := submit(raw); payload != nil || rejection == "" {
					t.Fatalf("invalid input accepted: %s", raw)
				}
			}
			payload, rejection := submit(`{"done":"true"}`)
			if rejection != "" {
				t.Fatal(rejection)
			}
			value, err := decodeResultContractJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateResultEvidenceInputs(value, compiled.evidenceInputs, audit.EvidenceFrom, c.dependencyResultsForTask(audit.ID)); err != nil {
				t.Fatal(err)
			}
			stored, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var restored ResultPayload
			if err := json.Unmarshal(stored, &restored); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(payload.Value, restored.Value) || payload.SHA256 != restored.SHA256 || c.validateSubmittedEvidenceInputs(audit.ID, &restored) != nil {
				t.Fatal("binding did not survive persistence")
			}
			source.TypedResult.StructuredPayload.Value = []byte(`{"category":"tampered"}`)
			if _, rejection := submit(`{"done":true}`); rejection == "" {
				t.Fatal("tampered source accepted")
			}
			if c.validateSubmittedEvidenceInputs(audit.ID, &restored) == nil {
				t.Fatal("stored binding accepted a changed source")
			}
			audit.EvidenceFrom = nil
			if _, rejection := submit(`{"done":true}`); rejection == "" {
				t.Fatal("missing evidence accepted")
			}
		})
	}
}

func TestRuntimeEvidenceBindingAggregateCapacity(t *testing.T) {
	for _, route := range []string{"local", "external"} {
		for _, sourceBytes := range []int{16 << 10, 128 << 10} {
			t.Run(fmt.Sprintf("%s/source_bytes=%d", route, sourceBytes), func(t *testing.T) {
				c, source, audit, _ := reviewedInputsFixture(t)
				compiled := c.session.ResultContracts[audit.ResultContract.ID]
				compiled.evidenceInputs.RuntimeBind = true
				sourceSchema := c.session.ResultContracts[source.ResultContract.ID]
				raw, err := json.Marshal(map[string]any{"category": strings.Repeat("a", sourceBytes)})
				if err != nil {
					t.Fatal(err)
				}
				payload, err := validateStructuredResultPayload(sourceSchema, *source.ResultContract, raw)
				if err != nil {
					t.Fatal(err)
				}
				audit.EvidenceFrom = nil
				for range 20 {
					item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: source.ResultContract}})[0]
					item.Status = TaskDone
					item.TypedResult = &TaskResult{TaskID: item.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload.clone()}
					audit.EvidenceFrom = append(audit.EvidenceFrom, item.ID)
				}
				var result *ResultPayload
				var rejection string
				if route == "local" {
					result, rejection = c.structuredPayloadForSubmission(audit.ID, []byte(`{"done":true}`))
				} else {
					request := AttemptRequest{Task: taskDefFromTodoItem(audit), resultContractSchema: compiled, resultEvidenceInputs: c.dependencyResultsForTask(audit.ID)}
					result, err = canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(`{"done":true}`)}, nil)
					if err != nil {
						rejection = err.Error()
					}
				}
				if sourceBytes == 128<<10 {
					if result != nil || !strings.Contains(rejection, "byte limit") {
						t.Fatalf("oversized aggregate was not rejected: %s", rejection)
					}
					return
				}
				if result == nil || rejection != "" || len(result.Value) <= 256<<10 {
					t.Fatalf("valid aggregate above the former limit was rejected: %s", rejection)
				}
				if err := c.validateSubmittedEvidenceInputs(audit.ID, result); err != nil {
					t.Fatalf("aggregate failed evidence replay: %v", err)
				}
				value, err := decodeResultContractJSON(result.Value)
				if err != nil {
					t.Fatal(err)
				}
				rows := value.(map[string]any)["reviews"].([]any)
				if len(rows) != 20 || rows[19].(map[string]any)["category"] != strings.Repeat("a", sourceBytes) {
					t.Fatal("aggregate dropped or shortened source records")
				}
			})
		}
	}
}

func TestRuntimeEvidenceBindingDeclarationAndWorkerGuidance(t *testing.T) {
	root := map[string]any{"type": "object", "properties": map[string]any{
		"done": map[string]any{"type": "boolean"},
		"reviews": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
			"origin": map[string]any{"type": "string"}, "digest": map[string]any{"type": "string"}, "data": map[string]any{"type": "object"},
		}}},
	}, "required": []any{"done", "reviews"}}
	spec := &resultEvidenceInputsSpec{RuntimeBind: true, ItemsPointer: "/reviews", CompletePointer: "/done", TaskIDPointer: "/origin", HashPointer: "/digest", ValueBindings: map[string]string{"/data": "/record"}}
	if err := validateRuntimeEvidenceBinding(root, spec); err != nil {
		t.Fatal(err)
	}
	spec.HashPointer = "/origin"
	if validateRuntimeEvidenceBinding(root, spec) == nil {
		t.Fatal("overlapping binding fields accepted")
	}
	spec.HashPointer = "/missing"
	if validateRuntimeEvidenceBinding(root, spec) == nil {
		t.Fatal("undeclared binding field accepted")
	}
	c, _, audit, _ := reviewedInputsFixture(t)
	compiled := c.session.ResultContracts[audit.ResultContract.ID]
	compiled.evidenceInputs.RuntimeBind = true
	projected := providerVisibleResultPayloadSchema(*audit.ResultContract, compiled)
	if _, exists := projected["properties"].(map[string]any)["reviews"]; exists {
		t.Fatal("provider still asks for source copies")
	}
	prompt := resultContractPromptSection(audit.ResultContract, compiled, true)
	if !strings.Contains(prompt, "Omit this field entirely") || strings.Contains(prompt, `"origin"`) {
		t.Fatalf("worker guidance asks for bound data: %s", prompt)
	}
}
