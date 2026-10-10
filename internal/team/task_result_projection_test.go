package team

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"gopkg.in/yaml.v3"
)

func projectionAssertion() TaskResultAssertion {
	return TaskResultAssertion{Pointer: "/findings", Op: "equals_projection", Value: map[string]any{
		"pointer": "/facts/rows", "fields": []any{"summary", "detail", "severity"}, "allow_missing": true,
	}}
}

func TestTaskResultProjectionSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		valid        bool
	}{
		{"matching with extra metadata", `{"findings":[{"summary":"a","detail":"b","severity":"info","category":"x"}],"facts":{"rows":[{"summary":"a","detail":"b","severity":"info","evidence":["ref"]}]}}`, true},
		{"both empty", `{"findings":[],"facts":{"rows":[]}}`, true},
		{"omitempty empty", `{"facts":{"rows":[]}}`, true},
		{"extra outer finding", `{"findings":[{"summary":"gap","detail":"unverified","severity":"info"}],"facts":{"rows":[]}}`, false},
		{"missing outer finding", `{"facts":{"rows":[{"summary":"a","detail":"b","severity":"info"}]}}`, false},
		{"missing source never empty", `{"findings":[],"facts":{}}`, false},
		{"null is not empty", `{"findings":null,"facts":{"rows":[]}}`, false},
		{"source null is not empty", `{"findings":[],"facts":{"rows":null}}`, false},
		{"wrong source type", `{"findings":[],"facts":{"rows":{}}}`, false},
		{"missing field", `{"findings":[{"summary":"a","severity":"info"}],"facts":{"rows":[{"summary":"a","severity":"info"}]}}`, false},
		{"wrong field type", `{"findings":[{"summary":"a","detail":1,"severity":"info"}],"facts":{"rows":[{"summary":"a","detail":"1","severity":"info"}]}}`, false},
		{"different severity", `{"findings":[{"summary":"a","detail":"b","severity":"info"}],"facts":{"rows":[{"summary":"a","detail":"b","severity":"warning"}]}}`, false},
		{"reordered", `{"findings":[{"summary":"a","detail":"b","severity":"info"},{"summary":"c","detail":"d","severity":"info"}],"facts":{"rows":[{"summary":"c","detail":"d","severity":"info"},{"summary":"a","detail":"b","severity":"info"}]}}`, false},
		{"non object item", `{"findings":["a"],"facts":{"rows":["a"]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document any
			if err := json.Unmarshal([]byte(tc.source), &document); err != nil {
				t.Fatal(err)
			}
			failures := evaluateTaskResultAssertions(document, []TaskResultAssertion{projectionAssertion()})
			if (len(failures) == 0) != tc.valid {
				t.Fatalf("valid=%t failures=%v", tc.valid, failures)
			}
		})
	}
}

func TestTaskResultProjectionPolicyValidation(t *testing.T) {
	for _, value := range []any{
		nil, "pointer", map[string]any{"pointer": "invalid", "fields": []string{"summary"}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary", "summary"}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{""}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{strings.Repeat("x", 129)}},
		map[string]any{"pointer": "/facts/rows", "fields": make([]string, 17)},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "unknown": true},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "allow_missing": "true"},
		map[string]any{"pointer": "/" + strings.Repeat("x", 512), "fields": []string{"summary"}},
	} {
		assertion := projectionAssertion()
		assertion.Value = value
		if err := validateTaskResultAssertion(0, assertion); err == nil {
			t.Fatalf("invalid projection accepted: %#v", value)
		}
	}
	if err := validateTaskResultAssertion(0, projectionAssertion()); err != nil {
		t.Fatal(err)
	}
	assertion := projectionAssertion()
	delete(assertion.Value.(map[string]any), "allow_missing")
	if failures := evaluateTaskResultAssertions(map[string]any{"facts": map[string]any{"rows": []any{}}}, []TaskResultAssertion{assertion}); len(failures) == 0 {
		t.Fatal("missing left array accepted without allow_missing")
	}
}

func TestSubmitResultProjectionRejectsBeforeCommitAndAllowsCorrection(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), executionRunID: "run-projection"}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "produce matched rows",
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{projectionAssertion()}},
	}})[0]
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	ctx := occurrenceTestContext(c, item.ID, 1)
	invalid := `{"status":"completed_with_gaps","summary":"coverage gap","findings":[{"summary":"gap","detail":"missing implementation evidence","severity":"info"}],"facts":{"rows":[]}}`
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: invalid})
	if err != nil || !response.IsError || !strings.Contains(response.Content, "exactly 0 items") {
		t.Fatalf("invalid result response=%#v error=%v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil || item.TypedResult != nil {
		t.Fatal("inconsistent result was committed")
	}
	response, err = tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"completed_with_gaps","summary":"coverage gap","findings":[],"facts":{"rows":[]}}`})
	if err != nil || response.IsError || c.GetTaskResult(item.ID) == nil {
		t.Fatalf("corrected result response=%#v error=%v", response, err)
	}
}

func TestTaskResultProjectionContractAndPersistence(t *testing.T) {
	const source = `type: task_result_assert
task-result-assertions:
  - pointer: /findings
    op: equals_projection
    value:
      pointer: /facts/rows
      fields: [summary, detail, severity]
      allow_missing: true
`
	var spec VerificationSpec
	if err := yaml.Unmarshal([]byte(source), &spec); err != nil {
		t.Fatal(err)
	}
	if err := validateVerificationSpec(spec); err != nil {
		t.Fatal(err)
	}
	contract := taskResultSubmissionContractForTask(TaskDef{VerifySpec: &spec})
	if !slices.Contains(contract.RequiredFields, "findings") || !slices.Contains(contract.RequiredFields, "facts") {
		t.Fatalf("required fields=%v", contract.RequiredFields)
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
		t.Fatal("projection policy changed across persistence")
	}
	clone := cloneVerificationSpec(spec)
	clone.TaskResultAssertions[0].Value.(map[string]any)["fields"].([]any)[0] = "different"
	if verificationSpecCacheKey(&spec) == verificationSpecCacheKey(&clone) || ComputeVerificationFingerprint(spec, nil, "") == ComputeVerificationFingerprint(clone, nil, "") {
		t.Fatal("projection fields do not affect policy identity or clone aliases source")
	}
	assertion := projectionAssertion()
	assertion.Value.(map[string]any)["pointer"] = "/outputs/rows"
	late := taskResultSubmissionContractForTask(TaskDef{OutputMode: TaskOutputModeVerbatim, VerifySpec: &VerificationSpec{
		Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{assertion},
	}})
	if len(late.SubmissionTaskResultAssertions) != 0 || len(late.TaskResultAssertions) != 1 {
		t.Fatalf("runtime-owned reference not deferred: %#v", late)
	}
}
