package team

import (
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func TestResultContractNullNormalization(t *testing.T) {
	const document = `{
	 "$schema":"https://json-schema.org/draft/2020-12/schema",
	 "type":"object","additionalProperties":false,"required":["rows"],
	 "properties":{"rows":{"type":"array","items":{"$ref":"#/$defs/row"}}},
	 "$defs":{"row":{"type":"object","additionalProperties":false,
	  "required":["kind","index","text","choice","digest"],"properties":{
	   "kind":{"type":"string","enum":["empty","finding"]},
	   "index":{"type":["integer","null"]},
	   "text":{"type":["string","null"]},
	   "choice":{"type":["integer","null"],"enum":[0,null]},
	   "digest":{"type":["string","null"],"pattern":"^[a-f0-9]{64}$"}
	  },"allOf":[{"if":{"properties":{"kind":{"const":"finding"}}},
	   "then":{"properties":{"index":{"type":"integer"}}},
	   "else":{"properties":{"index":{"type":"null"}}}}]}}
	}`
	compiled, err := CompileResultContractDocument("nullable.json", []byte(document))
	if err != nil {
		t.Fatal(err)
	}
	const native = `{"rows":[{"kind":"empty","index":null,"text":"null","choice":null,"digest":null}]}`
	baseline, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(native))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, input string
		valid       bool
	}{
		{"native", native, true},
		{"serialized null", strings.Replace(native, `"index":null`, `"index":"null"`, 1), true},
		{"capitalized", strings.Replace(native, `"index":null`, `"index":"NULL"`, 1), false},
		{"whitespace", strings.Replace(native, `"index":null`, `"index":" null "`, 1), false},
		{"numeric string", strings.Replace(native, `"index":null`, `"index":"0"`, 1), false},
		{"enum unchanged", strings.Replace(native, `"choice":null`, `"choice":"null"`, 1), false},
		{"hash unchanged", strings.Replace(native, `"digest":null`, `"digest":"null"`, 1), false},
		{"conditional still enforced", strings.ReplaceAll(strings.Replace(native, `"index":null`, `"index":"null"`, 1), `"empty"`, `"finding"`), false},
		{"missing unchanged", strings.Replace(native, `"index":null,`, "", 1), false},
		{"duplicate nullable field", strings.Replace(native, `"index":null`, `"index":0,"index":"null"`, 1), false},
		{"trailing JSON", native + ` {}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(test.input))
			if (err == nil) != test.valid {
				t.Fatalf("payload=%#v error=%v", payload, err)
			}
			if test.valid && (payload.SHA256 != baseline.SHA256 || string(payload.Value) != string(baseline.Value)) {
				t.Fatalf("normalization changed canonical value/hash: %#v", payload)
			}
		})
	}
}

func TestSubmitResultNormalizesNullWithOriginalContract(t *testing.T) {
	for _, gated := range []bool{false, true} {
		c, item := resultContractCoordinator(t, true)
		compiled, err := CompileResultContractDocument("nullable.json", []byte(`{
		 "$schema":"https://json-schema.org/draft/2020-12/schema",
		 "type":"object","required":["index"],"additionalProperties":false,
		 "properties":{"index":{"type":["integer","null"]}}
		}`))
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
		ctx := tools.SetToolsAllowed(occurrenceTestContext(c, item.ID, 1), []string{submitResultToolName})
		for _, input := range []string{
			`{"status":"success","summary":"reviewed","structured_payload":{"index":"NULL"}}`,
			`{"status":"success","summary":"reviewed","structured_payload":{"index":0,"index":"null"}}`,
			`{"status":"success","summary":"reviewed","structured_payload":{"index":0},"structured_payload":{"index":"null"}}`,
			`{"status":"success","summary":"reviewed","structured_payload":{"index":"null","extra":1}}`,
			`{"status":"success","summary":"reviewed","raw_output_ref":"null","structured_payload":{"index":"null"}}`,
		} {
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
			if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
				t.Fatalf("invalid/owned input accepted (gated=%v): %#v %v", gated, response, err)
			}
		}
		response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"reviewed","structured_payload":{"index":"null"}}`})
		if err != nil || response.IsError {
			t.Fatalf("valid nullable result rejected (gated=%v): %#v %v", gated, response, err)
		}
		got := c.GetTaskResult(item.ID)
		if got == nil || got.StructuredPayload == nil || string(got.StructuredPayload.Value) != `{"index":null}` {
			t.Fatalf("noncanonical result stored: %#v", got)
		}
	}
}

func TestResultNullNormalizationDoesNotTurnAbsentPayloadIntoPresent(t *testing.T) {
	compiled, err := CompileResultContractDocument("nullable-root.json", []byte(`{"type":["object","null"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if payload, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(`"null"`)); err == nil {
		t.Fatalf("serialized absence accepted as a required payload: %#v", payload)
	}
}
