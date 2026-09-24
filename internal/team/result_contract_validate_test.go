package team

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/execution"
)

func compiledReviewContract(t *testing.T, requireStructured bool) (*CompiledResultContract, ResultContractRef) {
	t.Helper()
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "schemas/review.json", reviewResultSchema)
	compiled, err := compileResultContractSchema(dir, "schemas/review.json")
	if err != nil {
		t.Fatal(err)
	}
	return compiled, compiled.ref(requireStructured)
}

func TestValidateStructuredResultPayload(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	valid, err := validateStructuredResultPayload(compiled, ref, []byte(`{"verdict":"approve","max_tokens":7,"findings":[{"summary":"ok"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := validateStructuredResultPayload(compiled, ref, []byte(`{ "findings":[{"summary":"ok"}], "max_tokens":7, "verdict":"approve" }`))
	if err != nil {
		t.Fatal(err)
	}
	if valid.SHA256 != reordered.SHA256 || string(valid.Value) != string(reordered.Value) || valid.Contract != ref {
		t.Fatalf("canonical payloads differ: %#v vs %#v", valid, reordered)
	}

	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "missing required", payload: `{"findings":[]}`, want: "verdict"},
		{name: "enum mismatch", payload: `{"verdict":"maybe"}`, want: "/verdict"},
		{name: "type mismatch", payload: `{"verdict":"approve","max_tokens":"seven"}`, want: "/max_tokens"},
		{name: "additional property", payload: `{"verdict":"approve","extra":1}`, want: "extra"},
		{name: "malformed JSON", payload: `{"verdict":`, want: "not a single JSON value"},
		{name: "trailing value", payload: `{"verdict":"approve"} {}`, want: "not a single JSON value"},
		{name: "secret-like content", payload: `{"verdict":"approve","findings":[{"summary":"Authorization: Bearer sk-live-abcdef123456"}]}`, want: "secret-like content"},
		{name: "oversized", payload: `{"verdict":"approve","findings":[{"summary":"` + strings.Repeat("x", resultPayloadMaxBytes) + `"}]}`, want: "byte limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateStructuredResultPayload(compiled, ref, []byte(tt.payload))
			if err == nil || !strings.Contains(err.Error(), structuredPayloadInvalidCode) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %s mentioning %q", err, structuredPayloadInvalidCode, tt.want)
			}
		})
	}
	if _, err := validateStructuredResultPayload(nil, ref, []byte(`{}`)); err == nil || !strings.Contains(err.Error(), resultContractDriftCode) {
		t.Fatalf("missing schema error = %v, want drift", err)
	}
}

func TestValidationErrorsAreBounded(t *testing.T) {
	dir := t.TempDir()
	properties := make([]string, 0, 30)
	for i := range 30 {
		properties = append(properties, `"p`+string(rune('a'+i%26))+strings.Repeat("x", i/26)+`":{"type":"integer"}`)
	}
	writeResultContractSchema(t, dir, "wide.json", `{"type":"object","properties":{`+strings.Join(properties, ",")+`}}`)
	compiled, err := compileResultContractSchema(dir, "wide.json")
	if err != nil {
		t.Fatal(err)
	}
	values := make([]string, 0, 30)
	for i := range 30 {
		values = append(values, `"p`+string(rune('a'+i%26))+strings.Repeat("x", i/26)+`":"not an integer"`)
	}
	_, err = validateStructuredResultPayload(compiled, compiled.ref(true), []byte(`{`+strings.Join(values, ",")+`}`))
	if err == nil {
		t.Fatal("invalid payload accepted")
	}
	lines := strings.Count(err.Error(), "\n- ")
	if lines > resultPayloadMaxErrors+1 || !strings.Contains(err.Error(), "more") {
		t.Fatalf("validation error has %d lines, want at most %d plus a summary: %v", lines, resultPayloadMaxErrors, err)
	}
}

// A validated payload survives durable event redaction byte-for-byte, so
// its hash still matches after a round trip through the event store.
func TestStructuredPayloadIsStableAcrossEventRedaction(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(`{"verdict":"approve","max_tokens":3,"findings":[{"summary":"password handling is correct"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]any{"typed_result": &TaskResult{Status: "success", Summary: "done", StructuredPayload: payload}})
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := redactJSONPreservingRuntimeOutputs(encoded)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror the event store append path, which compacts after redaction.
	var compact bytes.Buffer
	if err := json.Compact(&compact, redacted); err != nil {
		t.Fatal(err)
	}
	redacted = compact.Bytes()
	var decoded struct {
		TypedResult TaskResult `json:"typed_result"`
	}
	if err := json.Unmarshal(redacted, &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.TypedResult.StructuredPayload
	if got == nil || string(got.Value) != string(payload.Value) || got.SHA256 != payload.SHA256 {
		t.Fatalf("payload after redaction = %#v, want %#v", got, payload)
	}
}

func resultContractCoordinator(t *testing.T, requireStructured bool) (*Coordinator, *TodoItem) {
	t.Helper()
	compiled, ref := compiledReviewContract(t, requireStructured)
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		session:     &TeamSession{ResultContracts: map[string]*CompiledResultContract{compiled.ID: compiled}},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "review", ResultContract: &ref,
		ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "m"},
	}})[0]
	return c, item
}

func TestStructuredPayloadForSubmission(t *testing.T) {
	c, item := resultContractCoordinator(t, true)
	if _, msg := c.structuredPayloadForSubmission(item.ID, nil); !strings.Contains(msg, structuredPayloadMissingCode) {
		t.Fatalf("missing required payload message = %q", msg)
	}
	if _, msg := c.structuredPayloadForSubmission(item.ID, json.RawMessage(`{"verdict":"nope"}`)); !strings.Contains(msg, structuredPayloadInvalidCode) {
		t.Fatalf("invalid payload message = %q", msg)
	}
	if got := c.takeResultValidationFailures(item.ID); got != 1 {
		t.Fatalf("validation failures = %d, want 1", got)
	}
	payload, msg := c.structuredPayloadForSubmission(item.ID, json.RawMessage(`{"verdict":"approve"}`))
	if msg != "" || payload == nil {
		t.Fatalf("valid payload = %#v, %q", payload, msg)
	}

	optional, optionalItem := resultContractCoordinator(t, false)
	if payload, msg := optional.structuredPayloadForSubmission(optionalItem.ID, json.RawMessage(`null`)); payload != nil || msg != "" {
		t.Fatalf("optional contract without a payload = %#v, %q", payload, msg)
	}

	plain := &Coordinator{taskTracker: NewTaskTracker(), session: &TeamSession{}}
	plainItem := plain.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "plain"}})[0]
	if _, msg := plain.structuredPayloadForSubmission(plainItem.ID, json.RawMessage(`{"verdict":"approve"}`)); !strings.Contains(msg, "no result contract") {
		t.Fatalf("payload without a contract message = %q", msg)
	}

	drifted, driftedItem := resultContractCoordinator(t, true)
	for _, compiled := range drifted.session.ResultContracts {
		compiled.SchemaSHA256 = "changed"
	}
	if _, msg := drifted.structuredPayloadForSubmission(driftedItem.ID, json.RawMessage(`{"verdict":"approve"}`)); !strings.Contains(msg, resultContractDriftCode) {
		t.Fatalf("drifted contract message = %q", msg)
	}
}

func TestSubmitResultToolValidatesStructuredPayload(t *testing.T) {
	c, item := resultContractCoordinator(t, true)
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	info := tool.Info()
	if _, ok := info.Parameters["structured_payload"]; !ok || !slices.Contains(info.Required, "structured_payload") {
		t.Fatalf("submit_result info lacks a required structured_payload: %#v / %v", info.Parameters["structured_payload"], info.Required)
	}

	rejected, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name: "submit_result", Input: `{"status":"success","summary":"reviewed"}`,
	})
	if err != nil || !rejected.IsError || !strings.Contains(rejected.Content, structuredPayloadMissingCode) {
		t.Fatalf("submission without a payload = %#v, %v", rejected, err)
	}
	forged, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name: "submit_result", Input: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve"},"contract":{"id":"forged"}}`,
	})
	if err != nil || !forged.IsError {
		t.Fatalf("submission with a forged contract field = %#v, %v", forged, err)
	}
	accepted, err := tool.Run(occurrenceTestContext(c, item.ID, 1), fantasy.ToolCall{
		Name: "submit_result", Input: `{"status":"success","summary":"reviewed","structured_payload":{"verdict":"approve"}}`,
	})
	if err != nil || accepted.IsError {
		t.Fatalf("valid submission = %#v, %v", accepted, err)
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.StructuredPayload == nil || stored.StructuredPayload.Contract != *item.ResultContract || string(stored.StructuredPayload.Value) != `{"verdict":"approve"}` {
		t.Fatalf("stored result = %#v", stored)
	}
	if !strings.Contains(stored.FormatForContext(), `Structured Payload (contract schemas/review.json`) {
		t.Fatalf("coordinator context omits the payload: %q", stored.FormatForContext())
	}
}

func TestProviderVisibleResultPayloadSchema(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	// The review schema uses $defs/$ref, which the provider keyword allowlist
	// excludes, so the property stays open and names the contract.
	open := providerVisibleResultPayloadSchema(ref, compiled)
	if open["type"] != "object" || !strings.Contains(open["description"].(string), ref.ID) || open["properties"] != nil {
		t.Fatalf("open property = %#v", open)
	}
	dir := t.TempDir()
	writeResultContractSchema(t, dir, "simple.json", `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["verdict"],"properties":{"verdict":{"type":"string","enum":["approve","reject"]}}}`)
	simple, err := compileResultContractSchema(dir, "simple.json")
	if err != nil {
		t.Fatal(err)
	}
	embedded := providerVisibleResultPayloadSchema(simple.ref(true), simple)
	if embedded["properties"] == nil || embedded["$schema"] != nil {
		t.Fatalf("embedded property = %#v", embedded)
	}
}

func TestResultContractPromptSection(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	local := resultContractPromptSection(&ref, compiled, false)
	external := resultContractPromptSection(&ref, compiled, true)
	if !strings.Contains(local, "`structured_payload`") || !strings.Contains(local, "is required") || !strings.Contains(local, `"verdict"`) {
		t.Fatalf("local prompt = %q", local)
	}
	if !strings.Contains(external, "`structured_payload_json`") {
		t.Fatalf("external prompt = %q", external)
	}
	if resultContractPromptSection(nil, nil, false) != "" {
		t.Fatal("a task without a contract got a result contract prompt")
	}
}

func TestCanonicalExternalStructuredPayload(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	text := func(s string) *string { return &s }
	contracted := AttemptRequest{Task: TaskDef{ResultContract: &ref}, resultContractSchema: compiled}
	tests := []struct {
		name     string
		request  AttemptRequest
		proposal *WorkerResultProposal
		want     string
		payload  bool
	}{
		{name: "valid payload", request: contracted, proposal: &WorkerResultProposal{StructuredPayloadJSON: text(`{"verdict":"reject"}`)}, payload: true},
		{name: "required payload missing", request: contracted, proposal: &WorkerResultProposal{}, want: structuredPayloadMissingCode},
		{name: "invalid payload", request: contracted, proposal: &WorkerResultProposal{StructuredPayloadJSON: text(`{"verdict":1}`)}, want: structuredPayloadInvalidCode},
		{name: "payload without a contract", request: AttemptRequest{}, proposal: &WorkerResultProposal{StructuredPayloadJSON: text(`{}`)}, want: "must be null"},
		{name: "null without a contract", request: AttemptRequest{}, proposal: &WorkerResultProposal{}},
		{name: "drifted schema", request: AttemptRequest{Task: TaskDef{ResultContract: &ref}}, proposal: &WorkerResultProposal{StructuredPayloadJSON: text(`{"verdict":"reject"}`)}, want: resultContractDriftCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, err := canonicalExternalStructuredPayload(tt.request, tt.proposal)
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want %q", err, tt.want)
				}
				return
			}
			if err != nil || (payload != nil) != tt.payload {
				t.Fatalf("payload = %#v, err = %v", payload, err)
			}
		})
	}
	if _, ok := codexWorkerResultProposalSchema()["properties"].(map[string]any)["structured_payload_json"]; !ok {
		t.Fatal("codex proposal schema lacks structured_payload_json")
	}
}

func TestApplyResultContractReceipt(t *testing.T) {
	c, item := resultContractCoordinator(t, true)
	c.recordResultValidationFailure(item.ID)
	receipt := ExecutionReceipt{}
	c.applyResultContractReceipt(item.ID, nil, &receipt)
	if receipt.ResultContractID != item.ResultContract.ID || receipt.ResultValidation != ResultValidationInvalid || receipt.ResultValidationFailures != 1 {
		t.Fatalf("receipt after a rejected payload = %#v", receipt)
	}
	payload := &ResultPayload{Contract: *item.ResultContract, Value: json.RawMessage(`{}`), SHA256: "abc"}
	c.applyResultContractReceipt(item.ID, &TaskResult{StructuredPayload: payload}, &receipt)
	if receipt.ResultValidation != ResultValidationValid || receipt.ResultPayloadSHA256 != "abc" || receipt.ResultValidationFailures != 1 {
		t.Fatalf("receipt after a repaired payload = %#v", receipt)
	}
	metrics := RunMetrics{}
	accumulateResultContractMetrics(&metrics, receipt, "")
	if metrics.StructuredResultValidationFailures != 1 {
		t.Fatalf("metrics = %d, want 1", metrics.StructuredResultValidationFailures)
	}

	plain := &Coordinator{taskTracker: NewTaskTracker()}
	plainItem := plain.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "plain"}})[0]
	plainReceipt := ExecutionReceipt{}
	plain.applyResultContractReceipt(plainItem.ID, nil, &plainReceipt)
	if plainReceipt.ResultContractID != "" || plainReceipt.ResultValidation != "" {
		t.Fatalf("receipt for a task without a contract = %#v", plainReceipt)
	}
}

// A require-structured task never completes from promoted free text.
func TestRequireStructuredTaskIsNeverPromotedFromFreeText(t *testing.T) {
	ref := ResultContractRef{ID: "schemas/review.json", SchemaSHA256: "x", RequireStructured: true}
	task := TaskDef{Agent: "worker", Goal: "review", ResultContract: &ref}
	if promoted := promoteValidatedReadOnlyHandoff(task, "1", "worker", "## Review\nLooks good."); promoted != nil {
		t.Fatalf("require-structured task was promoted: %#v", promoted)
	}
	optional := ref
	optional.RequireStructured = false
	task.ResultContract = &optional
	if promoted := promoteValidatedReadOnlyHandoff(task, "1", "worker", "## Review\nLooks good."); promoted == nil {
		t.Fatal("an optional-contract task lost free-text promotion")
	}
}

// The resumed TaskDef keeps the admitted contract identity.
func TestTaskDefFromTodoItemKeepsResultContract(t *testing.T) {
	ref := &ResultContractRef{ID: "schemas/review.json", SchemaSHA256: "x", RequireStructured: true}
	task := taskDefFromTodoItem(&TodoItem{ID: "1", Agent: "worker", Goal: "g", ResultContract: ref})
	if task.ResultContract == nil || *task.ResultContract != *ref || task.ResultContract == ref {
		t.Fatalf("resumed task contract = %#v", task.ResultContract)
	}
}

func TestResultProtocolInstructionsIncludeResultContract(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	c := &Coordinator{session: &TeamSession{ResultContracts: map[string]*CompiledResultContract{compiled.ID: compiled}}}
	task := TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}, ResultContract: &ref}
	got := c.resultProtocolInstructions(task, map[string]bool{"submit_result": true})
	if !strings.Contains(got, "## Result Contract") {
		t.Fatalf("instructions lack the result contract section: %q", got)
	}
	task.Execution.RequiresResult = false
	if got := c.resultProtocolInstructions(task, map[string]bool{"submit_result": true}); got != "" {
		t.Fatalf("a task without a result protocol got instructions: %q", got)
	}
}
