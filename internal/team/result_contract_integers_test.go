package team

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func TestResultContractIntegerNormalization(t *testing.T) {
	compiled, err := CompileResultContractDocument("integers.json", []byte(`{
	 "type":"object","additionalProperties":false,"required":["rows"],
	 "properties":{"rows":{"type":"array","items":{"$ref":"#/$defs/row"}}},
	 "$defs":{"row":{"type":"object","additionalProperties":false,
	  "required":["index"],"properties":{"index":{"type":["integer","null"]}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, integer := range []string{"0", "1", "-1", "9007199254740993", "18446744073709551616"} {
		t.Run(integer, func(t *testing.T) {
			native := fmt.Sprintf(`{"rows":[{"index":%s}]}`, integer)
			baseline, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(native))
			if err != nil {
				t.Fatal(err)
			}
			serialized := fmt.Sprintf(`{"rows":[{"index":%q}]}`, integer)
			got, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(serialized))
			if err != nil || got == nil || string(got.Value) != string(baseline.Value) || got.SHA256 != baseline.SHA256 {
				t.Fatalf("canonical integer mismatch: %#v, %v", got, err)
			}
		})
	}
	for _, invalid := range []string{"", " 0", "0 ", "+0", "00", "01", "-01", "0.0", "1e0", "NaN", "０", "0x10", "null "} {
		t.Run("invalid/"+invalid, func(t *testing.T) {
			input := fmt.Sprintf(`{"rows":[{"index":%q}]}`, invalid)
			if result, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(input)); err == nil {
				t.Fatalf("invalid integer representation accepted: %#v", result)
			}
		})
	}
}

func TestResultContractIntegerNormalizationPreservesRestrictions(t *testing.T) {
	for _, test := range []struct {
		name, field, input string
		valid              bool
	}{
		{"bounded valid", `{"type":"integer","minimum":0,"maximum":31}`, `"0"`, true},
		{"above bound", `{"type":"integer","minimum":0,"maximum":31}`, `"32"`, false},
		{"below bound", `{"type":"integer","minimum":0}`, `"-1"`, false},
		{"enum", `{"type":"integer","enum":[0,1]}`, `"0"`, false},
		{"const", `{"type":"integer","const":0}`, `"0"`, false},
		{"number permits exact numeric strings", `{"type":"number"}`, `"0"`, true},
		{"union branches not guessed", `{"anyOf":[{"type":"integer"},{"type":"null"}]}`, `"0"`, false},
		{"ref sibling not guessed", `{"$ref":"#/$defs/index","type":"integer"}`, `"0"`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf(`{"type":"object","properties":{"index":%s},"$defs":{"index":{"type":"integer"}}}`, test.field)
			compiled, err := CompileResultContractDocument("bounded.json", []byte(document))
			if err != nil {
				t.Fatal(err)
			}
			got, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(`{"index":`+test.input+`}`))
			if (err == nil) != test.valid {
				t.Fatalf("payload=%#v error=%v", got, err)
			}
		})
	}
	compiled, err := CompileResultContractDocument("strings.json", []byte(`{
	 "type":"object","properties":{
	 "text":{"type":["integer","string"]},"hash":{"type":"string","pattern":"^[0-9]{64}$"},
	 "tuple":{"type":"array","prefixItems":[{"type":"integer"}]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	input := `{"text":"0","hash":"` + strings.Repeat("0", 64) + `","tuple":["0"]}`
	got, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(input))
	if err != nil || !strings.Contains(string(got.Value), `"text":"0"`) || !strings.Contains(string(got.Value), `"hash":"`+strings.Repeat("0", 64)+`"`) || !strings.Contains(string(got.Value), `"tuple":[0]`) {
		t.Fatalf("strings or tuple changed incorrectly: %#v %v", got, err)
	}
}

func TestSubmitResultNormalizesIntegerWithOriginalContract(t *testing.T) {
	for _, gated := range []bool{false, true} {
		for _, repair := range []bool{false, true} {
			t.Run(fmt.Sprintf("gated=%v/repair=%v", gated, repair), func(t *testing.T) {
				c, item := resultContractCoordinator(t, true)
				compiled, err := CompileResultContractDocument("integer.json", []byte(`{
				 "type":"object","required":["index"],"additionalProperties":false,
				 "properties":{"index":{"type":"integer","minimum":0,"maximum":31}}}`))
				if err != nil {
					t.Fatal(err)
				}
				ref := compiled.ref(true)
				item.ResultContract = &ref
				c.session.ResultContracts[compiled.ID] = compiled
				var tool fantasy.AgentTool = &submitResultTool{coordinator: c, todoID: item.ID}
				if gated {
					tool = c.gatePolicyTools([]fantasy.AgentTool{tool})[0]
				}
				for _, value := range []string{`"seven"`, `""`, `"0.0"`} {
					input := `{"status":"success","summary":"reviewed","structured_payload":{"index":` + value + `}}`
					if err := validateToolArguments(input, tool.Info()); err == nil {
						t.Fatalf("preflight accepted a non-integer string: %s", value)
					}
				}
				ctx := tools.SetToolsAllowed(occurrenceTestContext(c, item.ID, 1), []string{submitResultToolName})
				if repair {
					ctx = context.WithValue(ctx, protocolRepairExecutionKey{}, true)
				}
				for _, value := range []string{`""`, `"0.0"`, `"32"`, `"0","index":0`} {
					input := `{"status":"success","summary":"reviewed","structured_payload":{"index":` + value + `}}`
					response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
					if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
						t.Fatalf("unsafe input accepted: %#v %v", response, err)
					}
				}
				if gated {
					tool = c.wrapWithProtocolRepair([]fantasy.AgentTool{tool})[0]
				}
				response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"reviewed","structured_payload":{"index":"0"}}`})
				got := c.GetTaskResult(item.ID)
				if err != nil || response.IsError || got == nil || got.StructuredPayload == nil || string(got.StructuredPayload.Value) != `{"index":0}` {
					t.Fatalf("canonical integer not accepted: %#v %v", response, err)
				}
			})
		}
	}
}
