package team

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestResultCollectionFormatFeedback(t *testing.T) {
	info := fantasy.ToolInfo{Parameters: map[string]any{
		"findings": map[string]any{"type": "array", "items": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"summary": map[string]any{"type": "string"}}, "required": []string{"summary"}}},
	}}
	for _, tc := range []struct {
		name      string
		raw       string
		count     int
		truncated bool
	}{
		{"multiple layers", `{"gaps":[],"findings":[{"summary":"one","location":"wrong"},{"summary":"two","location":"wrong"},{"summary":"three","location":"wrong"}]}`, 4, false},
		{"invalid JSON", `{"findings":`, 1, false},
		{"duplicate keys", `{"findings":[],"findings":[]}`, 1, false},
		{"issue limit", `{"findings":[` + strings.Repeat(`{"location":"wrong"},`, 20) + `{}]}`, maxResultIssues, true},
		{"large pointer", `{"` + strings.Repeat("x", maxCollectionFeedbackBytes) + `":true}`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			feedback := resultCollectionFormatFeedback([]byte(tc.raw), info)
			if feedback == nil || len(feedback.Issues) != tc.count || feedback.Truncated != tc.truncated {
				t.Fatalf("feedback = %+v", feedback)
			}
			encoded, err := json.Marshal(feedback)
			if err != nil || len(encoded) > maxCollectionFeedbackBytes {
				t.Fatalf("unbounded feedback: bytes=%d err=%v", len(encoded), err)
			}
			if tc.name == "multiple layers" {
				for _, pointer := range []string{"/gaps", "/findings/0/location", "/findings/1/location", "/findings/2/location"} {
					if !slices.ContainsFunc(feedback.Issues, func(issue ResultSubmissionIssue) bool {
						return issue.Pointer == pointer && issue.Code == "additional_property"
					}) {
						t.Errorf("missing issue %s", pointer)
					}
				}
				if !slices.ContainsFunc(feedback.Objects, func(hint collectionSchemaHint) bool {
					return hint.Pointer == "/findings/0" && slices.Equal(hint.AllowedFields, []string{"summary"}) && hint.Example != nil
				}) {
					t.Fatal("missing local object guidance")
				}
			}
		})
	}
}

func TestLargeResultContractSyntaxFeedbackIsBounded(t *testing.T) {
	properties := make(map[string]any)
	var required []string
	for i := range 20 {
		name := fmt.Sprintf("field_%s_%d", strings.Repeat("x", 100), i)
		properties[name] = map[string]any{"type": "string"}
		required = append(required, name)
	}
	raw, err := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "large.json", string(raw))
	compiled, err := compileResultContractSchema(dir, "large.json")
	if err != nil {
		t.Fatal(err)
	}
	c := newBudgetCoordinator(t)
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: &ref}})[0]
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	response, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{Input: `{"status":`})
	if err != nil || !response.IsError || len(response.Content) > 4096 {
		t.Fatalf("unbounded or accepted invalid JSON: response=%+v err=%v", response, err)
	}
	if !strings.Contains(response.Content, "Allowed fields at $") || strings.Contains(response.Content, required[19]) {
		t.Fatalf("syntax feedback did not focus on the outer argument object: %s", response.Content)
	}
}

func TestFixedCollectionFeedbackLocatesRejectedObjectWithoutRepeatingManifest(t *testing.T) {
	for _, mistake := range []string{"nested_analysis", "outer_handling", "invalid_json"} {
		t.Run(mistake, func(t *testing.T) {
			c, _, item, plans := fixedCollectionFixture(t, 20)
			var payload map[string]any
			if err := json.Unmarshal([]byte(fixedCollectionSubmission(t, plans[0], nil)), &payload); err != nil {
				t.Fatal(err)
			}
			input := map[string]any{"status": "success", "summary": "Accounted for inputs", "structured_payload": payload}
			path := "$.handling"
			if mistake == "nested_analysis" {
				key := plans[0].rows[0].Key
				payload["decisions"].(map[string]any)[key].(map[string]any)["analysis"] = "wrong object"
				path = "$.structured_payload.decisions." + key + ".analysis"
			} else if mistake == "outer_handling" {
				input["handling"] = "addressed"
			} else {
				path = "$"
			}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			if mistake == "invalid_json" {
				raw = []byte(`{"status":`)
			}
			tool := &submitResultTool{coordinator: c, todoID: item.ID}
			ctx := occurrenceTestContext(c, item.ID, 1)
			response, err := tool.Run(ctx, fantasy.ToolCall{Input: string(raw)})
			if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("invalid submission was accepted: response=%+v err=%v", response, err)
			}
			wantFeedback := []string{path, "Allowed fields at", "complete tool argument object"}
			if mistake == "invalid_json" {
				// Duplicate-preserving raw JSON validation precedes DTO decoding.
				wantFeedback = []string{structuredPayloadInvalidCode, "invalid fixed collection JSON"}
			}
			for _, want := range wantFeedback {
				if !strings.Contains(response.Content, want) {
					t.Fatalf("feedback lacks %q: %s", want, response.Content)
				}
			}
			if len(response.Content) > 4096 || strings.Contains(response.Content, plans[0].rows[19].Key) {
				t.Fatalf("feedback repeated the large manifest instead of the rejected object: %s", response.Content)
			}
			if mistake == "nested_analysis" && (!strings.Contains(response.Content, `"state"`) || !strings.Contains(response.Content, `"reason"`)) {
				t.Fatalf("feedback lacks the accepted decision fields: %s", response.Content)
			}
			valid := `{"status":"success","summary":"Accounted for inputs","structured_payload":` + fixedCollectionSubmission(t, plans[0], nil) + `}`
			response, err = tool.Run(ctx, fantasy.ToolCall{Input: valid})
			if err != nil || response.IsError || c.GetTaskResult(item.ID) == nil {
				t.Fatalf("same-attempt correction failed: response=%+v err=%v", response, err)
			}
			var stored map[string]any
			if err := json.Unmarshal(c.GetTaskResult(item.ID).StructuredPayload.Value, &stored); err != nil {
				t.Fatal(err)
			}
			if len(stored["decisions"].([]any)) != 20 {
				t.Fatal("correction changed fixed membership")
			}
		})
	}
}
