package team

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func TestCanonicalToolArgumentObjects(t *testing.T) {
	info := fantasy.ToolInfo{Parameters: map[string]any{"payload": map[string]any{"type": "object", "properties": map[string]any{"counter": map[string]any{"type": "integer"}}}}}
	for _, tc := range []struct {
		input   string
		changed bool
	}{
		{`{"payload":"{\"counter\":9007199254740993}"}`, true},
		{`{"payload":{"counter":1}}`, false},
		{`{"payload":"{\"counter\":1,\"counter\":2}"}`, false},
		{`{"payload":"{\"counter\":1} {}"}`, false},
		{`{"payload":"[]"}`, false},
		{`{"payload":"null"}`, false},
		{`{"payload":"false"}`, false},
		{`{"payload":"{\"counter\":1}","payload":{}}`, false},
		{`{"free_text":"{\"counter\":1}"}`, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got := canonicalToolArgumentObjects(tc.input, info)
			if (got != tc.input) != tc.changed {
				t.Fatalf("got %s", got)
			}
			if tc.changed && (!strings.Contains(got, "9007199254740993") || validateToolArguments(got, info) != nil) {
				t.Fatalf("lost numeric precision or object schema: %s", got)
			}
		})
	}
	info.Parameters["payload"] = map[string]any{"type": []any{"string", "object"}}
	input := `{"payload":"{}"}`
	if got := canonicalToolArgumentObjects(input, info); got != input {
		t.Fatal("changed legitimate union string")
	}
}

func TestSubmitResultUnwrapsObjectWithoutWeakeningValidation(t *testing.T) {
	for _, gated := range []bool{false, true} {
		t.Run(fmt.Sprint(gated), func(t *testing.T) {
			c, item := resultContractCoordinator(t, true)
			var tool fantasy.AgentTool = &submitResultTool{coordinator: c, todoID: item.ID}
			if gated {
				tool = c.gatePolicyTools([]fantasy.AgentTool{tool})[0]
			}
			ctx := tools.SetToolsAllowed(occurrenceTestContext(c, item.ID, 1), []string{submitResultToolName})
			bad, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"reviewed","structured_payload":"{\"verdict\":\"maybe\"}"}`})
			if err != nil || !bad.IsError || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("invalid object accepted: %#v %v", bad, err)
			}
			accepted, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"reviewed","structured_payload":"{\"verdict\":\"approve\"}"}`})
			if err != nil || accepted.IsError {
				t.Fatalf("lossless object rejected: %#v %v", accepted, err)
			}
			got := c.GetTaskResult(item.ID)
			if got == nil || got.StructuredPayload == nil || string(got.StructuredPayload.Value) != `{"verdict":"approve"}` {
				t.Fatalf("lost canonical payload: %#v", got)
			}
		})
	}
}
