package team

import (
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestSubmitResultUnknownFindingFieldIdentifiesParentPath(t *testing.T) {
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "nested.json", `{"type":"object","properties":{"record":{"type":"object","properties":{"findings":{"type":"array","items":{"type":"object","properties":{"evidence":{"type":"array","items":{"type":"string"}},"location":{"type":"string"}},"required":["evidence","location"],"additionalProperties":false}}},"required":["findings"],"additionalProperties":false}},"required":["record"],"additionalProperties":false}`)
	compiled, err := compileResultContractSchema(dir, "nested.json")
	if err != nil {
		t.Fatal(err)
	}
	ref := compiled.ref(true)
	for _, field := range []string{"evidence", "location"} {
		t.Run(field, func(t *testing.T) {
			c := &Coordinator{
				session:     &TeamSession{ResultContracts: map[string]*CompiledResultContract{ref.ID: compiled}},
				taskTracker: NewTaskTracker(), executionRunID: "run-nested-diagnostic",
			}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "review", ResultContract: &ref}})[0]
			sink := &assertionRecordingSink{}
			tool := &submitResultTool{coordinator: c, todoID: item.ID, sink: sink}
			finding := map[string]any{"summary": "reachable failure", "detail": "grounded scenario", "severity": "warning"}
			richFinding := map[string]any{"evidence": []string{"opaque-source"}, "location": "source.go:10"}
			finding[field] = richFinding[field]
			input := map[string]any{
				"status": "success", "summary": "review complete", "findings": []any{finding},
				"structured_payload": map[string]any{"record": map[string]any{"findings": []any{richFinding}}},
			}
			call := func() fantasy.ToolResponse {
				t.Helper()
				raw, marshalErr := json.Marshal(input)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				response, runErr := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Name: submitResultToolName, Input: string(raw)})
				if runErr != nil {
					t.Fatal(runErr)
				}
				return response
			}
			response := call()
			if !response.IsError || !strings.Contains(response.Content, "$.findings[0]."+field) {
				t.Fatalf("rejection lacks the offending parent path: %#v", response)
			}
			if sink.calls != 0 || c.GetTaskResult(item.ID) != nil || item.TypedResult != nil {
				t.Fatal("invalid nested finding was published")
			}
			delete(finding, field)
			if response = call(); response.IsError {
				t.Fatalf("removing only the rejected field must preserve the legal domain namesake: %#v", response)
			}
			result := c.GetTaskResult(item.ID)
			if sink.calls != 1 || result == nil || len(result.Findings) != 1 || result.StructuredPayload == nil || !strings.Contains(string(result.StructuredPayload.Value), "opaque-source") {
				t.Fatalf("corrected submission lost finding/evidence: calls=%d result=%#v", sink.calls, result)
			}
		})
	}
}

func TestCompactExamplePreservesConstantsAndOptionalEmptyArrays(t *testing.T) {
	for _, value := range []any{"review", false, nil} {
		if got := generateCompactExample(map[string]any{"const": value}); got != value {
			t.Fatalf("const example = %#v, want %#v", got, value)
		}
	}
	for _, minimum := range []int{0, 1} {
		got := generateCompactExample(map[string]any{"type": "array", "minItems": minimum, "items": map[string]any{"type": "string"}}).([]any)
		if len(got) != minimum {
			t.Fatalf("array example has %d entries, want %d", len(got), minimum)
		}
	}
	schema := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	if got := generateCompactExample(schema).([]any); len(got) != 1 {
		t.Fatalf("ordinary tool array lost its illustrative item: %#v", got)
	}
	if got := generateCompactExampleWithArrayDefaults(schema, true).([]any); len(got) != 0 {
		t.Fatalf("domain array invented optional claims: %#v", got)
	}
}
