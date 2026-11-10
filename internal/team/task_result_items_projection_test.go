package team

import (
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"go.yaml.in/yaml/v3"
)

func itemsStatusAssertion(pointer string) TaskResultAssertion {
	return TaskResultAssertion{Pointer: "/status", Op: "equals_by_items", Value: map[string]any{
		"pointer": pointer, "empty": "success", "non_empty": "completed_with_gaps",
	}}
}

func TestSubmitResultItemsProjectionRejectsBeforeCommit(t *testing.T) {
	c := newTestCoordinator(t)
	c.executionRunID = "run-items"
	c.taskTracker = NewTaskTracker()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "report bounded work",
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{itemsStatusAssertion("/facts/notes")}},
	}})[0]
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	ctx := occurrenceTestContext(c, item.ID, 1)
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"reviewed","facts":{"notes":["missing source"]}}`})
	if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil || item.TypedResult != nil {
		t.Fatalf("inconsistent claims committed: response=%+v error=%v", response, err)
	}
	response, err = tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"completed_with_gaps","summary":"reviewed","facts":{"notes":["missing source"]}}`})
	if err != nil || response.IsError || c.GetTaskResult(item.ID) == nil {
		t.Fatalf("consistent correction rejected: response=%+v error=%v", response, err)
	}
}

func TestTaskResultItemsProjectionSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		valid        bool
	}{
		{"empty", `{"status":"success","notes":[]}`, true},
		{"nonempty", `{"status":"completed_with_gaps","notes":["missing source"]}`, true},
		{"success conceals items", `{"status":"success","notes":["missing source","unexecuted check"]}`, false},
		{"gaps without items", `{"status":"completed_with_gaps","notes":[]}`, false},
		{"missing reference", `{"status":"success"}`, false},
		{"null reference", `{"status":"success","notes":null}`, false},
		{"object reference", `{"status":"success","notes":{}}`, false},
		{"string reference", `{"status":"success","notes":""}`, false},
		{"missing target", `{"notes":[]}`, false},
		{"null target", `{"status":null,"notes":[]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document any
			if err := json.Unmarshal([]byte(tc.source), &document); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(document, json.Deterministic(true))
			failures := evaluateTaskResultAssertions(document, []TaskResultAssertion{itemsStatusAssertion("/notes")})
			if (len(failures) == 0) != tc.valid {
				t.Fatalf("valid=%t failures=%v", tc.valid, failures)
			}
			after, _ := json.Marshal(document, json.Deterministic(true))
			if string(before) != string(after) {
				t.Fatal("comparison rewrote submitted claims")
			}
		})
	}
	tooMany := map[string]any{"status": "completed_with_gaps", "notes": make([]any, maxTaskResultAssertionItems+1)}
	if len(evaluateTaskResultAssertions(tooMany, []TaskResultAssertion{itemsStatusAssertion("/notes")})) == 0 {
		t.Fatal("unbounded array accepted")
	}
	// Labels and pointers are configuration-owned, not specific to task status.
	assertion := TaskResultAssertion{Pointer: "/label", Op: "equals_by_items", Value: map[string]any{
		"pointer": "/data/a~1b~0c", "empty": "vacant", "non_empty": "occupied",
	}}
	if failures := evaluateTaskResultAssertions(map[string]any{"label": "occupied", "data": map[string]any{"a/b~c": []any{false}}}, []TaskResultAssertion{assertion}); len(failures) != 0 {
		t.Fatal(failures)
	}
}

func TestTaskResultItemsProjectionRejectsMalformedPolicies(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"missing pointer", func(v map[string]any) { delete(v, "pointer") }},
		{"root reference", func(v map[string]any) { v["pointer"] = "" }},
		{"invalid pointer", func(v map[string]any) { v["pointer"] = "/bad~2key" }},
		{"long pointer", func(v map[string]any) { v["pointer"] = "/" + strings.Repeat("a", maxTaskResultPointerBytes) }},
		{"missing empty label", func(v map[string]any) { delete(v, "empty") }},
		{"null label", func(v map[string]any) { v["non_empty"] = nil }},
		{"numeric label", func(v map[string]any) { v["empty"] = 0 }},
		{"unknown key", func(v map[string]any) { v["guess"] = true }},
		{"oversized value", func(v map[string]any) { v["empty"] = strings.Repeat("a", maxTaskResultValueBytes) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertion := itemsStatusAssertion("/notes")
			tc.edit(assertion.Value.(map[string]any))
			if validateTaskResultAssertion(0, assertion) == nil {
				t.Fatal("malformed policy passed preflight")
			}
		})
	}
}

func TestTaskResultItemsProjectionContractAndPersistence(t *testing.T) {
	const source = `type: task_result_assert
task-result-assertions:
  - pointer: /status
    op: equals_by_items
    value:
      pointer: /structured_payload/value/notes
      empty: success
      non_empty: completed_with_gaps
`
	var spec VerificationSpec
	if err := yaml.Unmarshal([]byte(source), &spec); err != nil {
		t.Fatal(err)
	}
	if err := validateVerificationSpec(spec); err != nil {
		t.Fatal(err)
	}
	contract := taskResultSubmissionContractForTask(TaskDef{VerifySpec: &spec})
	if !slices.Contains(contract.RequiredFields, "status") || !slices.Contains(contract.RequiredFields, "structured_payload") || len(contract.SubmissionTaskResultAssertions) != 1 {
		t.Fatalf("contract lost referenced claims: %+v", contract)
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var restored VerificationSpec
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if verificationSpecCacheKey(&spec) != verificationSpecCacheKey(&restored) || ComputeVerificationFingerprint(spec, nil, "") != ComputeVerificationFingerprint(restored, nil, "") {
		t.Fatal("policy identity changed after persistence")
	}
	clone := cloneVerificationSpec(spec)
	clone.TaskResultAssertions[0].Value.(map[string]any)["non_empty"] = "different"
	if verificationSpecCacheKey(&spec) == verificationSpecCacheKey(&clone) || ComputeVerificationFingerprint(spec, nil, "") == ComputeVerificationFingerprint(clone, nil, "") {
		t.Fatal("labels were not deep-cloned or excluded from policy identity")
	}
	for _, pointer := range []string{"/outputs/notes", "/raw_output_ref/notes"} {
		assertion := itemsStatusAssertion(pointer)
		if !taskResultAssertionUsesTranscriptFinalization(assertion) {
			t.Fatal("runtime transcript source checked before finalization")
		}
	}
}
