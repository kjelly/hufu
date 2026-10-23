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

func TestTaskResultProjectionOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		valid             bool
	}{
		{"absent", `{}`, `{}`, true},
		{"matching index zero", `{"link":0}`, `{"link":0}`, true},
		{"index zero versus omitted", `{"link":0}`, `{}`, false},
		{"omitted versus index zero", `{}`, `{"link":0}`, false},
		{"different index", `{"link":0}`, `{"link":1}`, false},
		{"empty array omitted", `{"notes":[]}`, `{}`, true},
		{"omitted array empty", `{}`, `{"notes":[]}`, true},
		{"matching evidence", `{"notes":["source"]}`, `{"notes":["source"]}`, true},
		{"different evidence", `{"notes":["source"]}`, `{"notes":["other"]}`, false},
		{"omitted evidence", `{}`, `{"notes":["source"]}`, false},
		{"null is not omitted", `{"link":null}`, `{}`, false},
		{"false is not omitted", `{"link":false}`, `{}`, false},
		{"empty string is not omitted", `{"link":""}`, `{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var left, right map[string]any
			if err := json.Unmarshal([]byte(tc.left), &left); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.right), &right); err != nil {
				t.Fatal(err)
			}
			left["label"], right["label"] = "a", "a"
			policy := map[string]any{"pointer": "/rows", "fields": []string{"label"}, "optional_fields": []string{"link", "notes"}}
			err := evaluateTaskResultProjection(map[string]any{"rows": []any{right}}, []any{left}, nil, policy)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
		})
	}
}

func TestTaskResultProjectionPolicyValidation(t *testing.T) {
	for _, value := range []any{
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "optional_fields": []string{"summary"}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "optional_fields": []string{"index", "index"}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "optional_fields": []string{""}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "optional_fields": make([]string, 16)},
		nil, "pointer", map[string]any{"pointer": "invalid", "fields": []string{"summary"}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary", "summary"}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{""}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{strings.Repeat("x", 129)}},
		map[string]any{"pointer": "/facts/rows", "fields": make([]string, 17)},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "unknown": true},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "allow_missing": "true"},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "field_defaults": []any{}},
		map[string]any{"pointer": "/facts/rows", "fields": []string{"summary"}, "field_defaults": map[string]any{"uncompared": nil}},
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

func TestTaskResultProjectionExplicitOptionalFieldDefaults(t *testing.T) {
	policy := map[string]any{"pointer": "/right", "fields": []string{"id", "position", "missing"}, "field_defaults": map[string]any{"position": nil, "missing": []any{}}}
	assertion := TaskResultAssertion{Pointer: "/left", Op: "equals_projection", Value: policy}
	for _, tc := range []struct {
		name, source string
		valid        bool
	}{
		{"omitted on both sides", `{"left":[{"id":"a"}],"right":[{"id":"a"}]}`, true},
		{"omitted versus empty", `{"left":[{"id":"a"}],"right":[{"id":"a","missing":[]}]}`, true},
		{"null versus omitted", `{"left":[{"id":"a","position":null,"missing":null}],"right":[{"id":"a"}]}`, true},
		{"zero remains a real index", `{"left":[{"id":"a"}],"right":[{"id":"a","position":0}]}`, false},
		{"zero matches zero", `{"left":[{"id":"a","position":0}],"right":[{"id":"a","position":0}]}`, true},
		{"numeric strings remain distinct", `{"left":[{"id":"a","position":"0"}],"right":[{"id":"a","position":0}]}`, false},
		{"missing evidence cannot be discarded", `{"left":[{"id":"a","missing":["file"]}],"right":[{"id":"a","missing":[]}]}`, false},
		{"evidence order still matches", `{"left":[{"id":"a","missing":["x","y"]}],"right":[{"id":"a","missing":["y","x"]}]}`, false},
		{"required fields remain required", `{"left":[{}],"right":[{}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document any
			if err := json.Unmarshal([]byte(tc.source), &document); err != nil {
				t.Fatal(err)
			}
			failures := evaluateTaskResultAssertions(document, []TaskResultAssertion{assertion})
			if (len(failures) == 0) != tc.valid {
				t.Fatalf("valid=%t failures=%v", tc.valid, failures)
			}
		})
	}
	encoded, err := json.Marshal(assertion)
	if err != nil {
		t.Fatal(err)
	}
	var restored TaskResultAssertion
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{"left": []any{map[string]any{"id": "a"}}, "right": []any{map[string]any{"id": "a", "missing": []any{}}}}
	if failures := evaluateTaskResultAssertions(document, []TaskResultAssertion{restored}); len(failures) != 0 {
		t.Fatal(failures)
	}
	delete(policy, "field_defaults")
	if failures := evaluateTaskResultAssertions(document, []TaskResultAssertion{assertion}); len(failures) == 0 {
		t.Fatal("optional fields became implicitly optional without configuration")
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
      optional_fields: [link, notes]
      allow_missing: true
      field_defaults: {detail: ""}
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
	clone = cloneVerificationSpec(spec)
	clone.TaskResultAssertions[0].Value.(map[string]any)["optional_fields"].([]any)[0] = "changed"
	if verificationSpecCacheKey(&spec) == verificationSpecCacheKey(&clone) || ComputeVerificationFingerprint(spec, nil, "") == ComputeVerificationFingerprint(clone, nil, "") {
		t.Fatal("optional projection fields do not affect policy identity or clone aliases source")
	}
	defaultsClone := cloneVerificationSpec(spec)
	defaultsClone.TaskResultAssertions[0].Value.(map[string]any)["field_defaults"].(map[string]any)["detail"] = "changed"
	if verificationSpecCacheKey(&spec) == verificationSpecCacheKey(&defaultsClone) || ComputeVerificationFingerprint(spec, nil, "") == ComputeVerificationFingerprint(defaultsClone, nil, "") {
		t.Fatal("projection defaults do not affect policy identity or clone aliases source")
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

func TestSubmitResultOptionalProjectionCanBeCorrected(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), executionRunID: "run-optional-projection"}
	assertion := TaskResultAssertion{Pointer: "/facts/first", Op: "equals_projection", Value: map[string]any{
		"pointer": "/facts/second", "fields": []string{"label"}, "optional_fields": []string{"link", "notes"},
	}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "match rows", VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{assertion}}}})[0]
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	ctx := occurrenceTestContext(c, item.ID, 1)
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"matched","facts":{"first":[{"label":"a"}],"second":[{"label":"a","link":0}]}}`})
	if err != nil || !response.IsError || !strings.Contains(response.Content, `optional field "link"`) {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if c.GetTaskResult(item.ID) != nil || item.TypedResult != nil {
		t.Fatal("mismatched optional field committed")
	}
	response, err = tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: `{"status":"success","summary":"matched","facts":{"first":[{"label":"a","link":0}],"second":[{"label":"a","link":0,"notes":[]}]}}`})
	if err != nil || response.IsError || c.GetTaskResult(item.ID) == nil {
		t.Fatalf("correction response=%#v err=%v", response, err)
	}
}
