package team

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"charm.land/fantasy"
)

func TestStructuredScalarConversionKeepsCanonicalHashAcrossRoutes(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	quoted := `{"verdict":"approve","max_tokens":"9007199254740993"}`
	typed := `{"verdict":"approve","max_tokens":9007199254740993}`
	canonical, err := validateStructuredResultPayload(compiled, ref, []byte(typed))
	if err != nil {
		t.Fatal(err)
	}
	local, err := validateStructuredResultPayload(compiled, ref, []byte(quoted))
	if err != nil {
		t.Fatal(err)
	}
	external, err := canonicalExternalStructuredPayload(AttemptRequest{Task: TaskDef{ResultContract: &ref}, resultContractSchema: compiled}, &WorkerResultProposal{StructuredPayloadJSON: new(quoted)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []*ResultPayload{local, external} {
		if !bytes.Equal(payload.Value, canonical.Value) || payload.SHA256 != canonical.SHA256 {
			t.Fatal("conversion changed numeric precision or canonical hash")
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		redacted, err := redactJSONPreservingRuntimeOutputs(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var restored ResultPayload
		// Event-store appends compact the indented redaction projection.
		compact := jsontext.Value(redacted)
		if err := compact.Compact(); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(compact, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.SHA256 != canonical.SHA256 || !bytes.Equal(restored.Value, canonical.Value) {
			t.Fatalf("persisted canonical result changed: %s (%s), want %s (%s)", restored.Value, restored.SHA256, canonical.Value, canonical.SHA256)
		}
	}
}

func TestProtocolRepairConvertsScalarStringsBeforeTypedHandler(t *testing.T) {
	type input struct {
		Count   int  `json:"count"`
		Enabled bool `json:"enabled"`
	}
	calls := 0
	base := fantasy.NewAgentTool("probe", "typed probe", func(_ context.Context, value input, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		calls++
		if value.Count != 42 || value.Enabled {
			t.Fatalf("wrong typed input: %#v", value)
		}
		return fantasy.NewTextResponse("ok"), nil
	})
	wrapper := (&Coordinator{}).wrapWithProtocolRepair([]fantasy.AgentTool{base})[0]
	response, err := wrapper.Run(t.Context(), fantasy.ToolCall{Name: "probe", Input: `{"count":"42","enabled":"yes"}`})
	if err != nil || !response.IsError || calls != 0 {
		t.Fatal("invalid scalar reached typed handler")
	}
	response, err = wrapper.Run(t.Context(), fantasy.ToolCall{Name: "probe", Input: `{"count":"42","enabled":"false"}`})
	if err != nil || response.IsError || calls != 1 {
		t.Fatalf("repair did not execute exactly once: %#v %v calls=%d", response, err, calls)
	}
}

func TestStructuredScalarConversionStillEnforcesSchema(t *testing.T) {
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "scalar.json", `{"type":"object","required":["done","count"],"properties":{"done":{"const":true},"count":{"type":"integer","minimum":1,"maximum":3},"flag":{"type":"boolean"},"ratio":{"type":"number","maximum":1}},"additionalProperties":false}`)
	compiled, err := compileResultContractSchema(dir, "scalar.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"done":"false","count":"2"}`, `{"done":"true","count":"4"}`, `{"done":"true","count":"0"}`,
		`{"done":"true","count":"2.5"}`, `{"done":"yes","count":"2"}`, `{"done":"true","count":"02"}`,
		`{"done":"true","count":"2","ratio":"1.5"}`, `{"done":"true","count":"2","flag":"1"}`,
	} {
		if _, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(raw)); err == nil {
			t.Fatalf("converted input bypassed validation: %s", raw)
		}
	}
	if _, err := validateStructuredResultPayload(compiled, compiled.ref(true), []byte(`{"done":"true","count":"2","flag":"false","ratio":"0.5"}`)); err != nil {
		t.Fatal(err)
	}
}
