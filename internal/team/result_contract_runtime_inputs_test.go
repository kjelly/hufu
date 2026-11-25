package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

const runtimeInputTestSchema = `{
 "type":"object","additionalProperties":false,
 "required":["decision","observation"],
 "properties":{
   "decision":{"type":"boolean"},
   "observation":{"type":"object","additionalProperties":false,
     "required":["count","literal"],
     "properties":{"count":{"type":"integer"},"literal":{"type":"string"}}}
 },
 "x-hufu-runtime-inputs":[{"target":"/observation","source_task":"prepare-outputs","output":"observation"}]
}`

func runtimeInputResultFixture(t *testing.T) (*Coordinator, *TodoItem, *TodoItem, *CompiledResultContract) {
	t.Helper()
	c, source := runtimeOutputRefFixture(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "consumer.json"), []byte(runtimeInputTestSchema), 0o600); err != nil {
		t.Fatal(err)
	}
	compiled, err := compileResultContractSchema(dir, "consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	consumer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "consumer", ResultContract: &ref}})[0]
	consumer.Status = TaskInProgress
	return c, source, consumer, compiled
}

func setRuntimeInputObservation(t *testing.T, c *Coordinator, source *TodoItem, observation any) {
	t.Helper()
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"observation": observation})
	if err != nil {
		t.Fatal(err)
	}
	source.TypedResult.RuntimeOutputs = outputs
	source.TypedResult.RuntimeOutputsHash = digest
	source.ExecutionReceipt.RuntimeOutputsHash = digest
	c.storeSubmittedTaskResult(source.ID, source.TypedResult)
}

func TestRuntimeResultInputsSubmissionAndReplay(t *testing.T) {
	t.Parallel()
	for _, route := range []string{"local", "external"} {
		t.Run(route, func(t *testing.T) {
			c, source, consumer, compiled := runtimeInputResultFixture(t)
			literal := strings.Repeat("snapshot failure α\n", 4000) + "{suffix}"
			setRuntimeInputObservation(t, c, source, map[string]any{"count": uint64(9007199254740993), "literal": literal})
			submit := func(raw string) (*ResultPayload, string) {
				if route == "local" {
					return c.structuredPayloadForSubmission(consumer.ID, []byte(raw))
				}
				request := AttemptRequest{Task: taskDefFromTodoItem(consumer), resultContractSchema: compiled,
					resultRuntimeInputs: func(spec *CompiledResultContract) (map[string]any, error) {
						return c.resultRuntimeInputValues(spec, "")
					}}
				payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(raw)}, nil)
				if err != nil {
					return nil, err.Error()
				}
				return payload, ""
			}
			for _, raw := range []string{`{"decision":true,"observation":null}`, `{"decision":true,"observation":{}}`, `{"decision":true,"decision":false}`} {
				if payload, rejection := submit(raw); payload != nil || rejection == "" {
					t.Fatalf("invalid model input accepted: %s", raw)
				}
			}
			payload, rejection := submit(`{"decision":true}`)
			if rejection != "" {
				t.Fatal(rejection)
			}
			value, err := decodeUniqueJSON(payload.Value)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := resolveJSONPointer(value, "/observation/literal")
			if err != nil || actual != literal || !bytes.Contains(payload.Value, []byte(`"count":9007199254740993`)) {
				t.Fatal("runtime binding truncated text, expanded a token, or rounded a number")
			}
			workspace := t.TempDir()
			journal, err := openTaskJournal(workspace)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			if err := journal.append(journalRecord{Op: "result", TaskID: consumer.ID, RunID: c.executionRunID, TypedResult: &TaskResult{TaskID: consumer.ID, Status: TaskResultStatusSuccess, StructuredPayload: payload}}); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			var exported bytes.Buffer
			if err := ExportTaskJournal(t.Context(), workspace, &exported); err != nil {
				t.Fatal(err)
			}
			var record journalRecord
			if err := json.Unmarshal(bytes.TrimSpace(exported.Bytes()), &record); err != nil {
				t.Fatal(err)
			}
			if record.TypedResult == nil || record.TypedResult.StructuredPayload == nil {
				t.Fatal("journal omitted bound payload")
			}
			restored := *record.TypedResult.StructuredPayload
			if !bytes.Equal(payload.Value, restored.Value) || payload.SHA256 != restored.SHA256 || c.validateSubmittedEvidenceInputs(t.Context(), consumer.ID, &restored) != nil {
				t.Fatal("canonical binding did not survive persistence/replay")
			}
			setRuntimeInputObservation(t, c, source, map[string]any{"count": 3, "literal": "different verified output"})
			if c.validateSubmittedEvidenceInputs(t.Context(), consumer.ID, &restored) == nil {
				t.Fatal("historical payload silently accepted a different verified source")
			}
		})
	}
}

func TestRuntimeResultInputsRejectUntrustedOrMistypedSources(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"pending", "model", "digest", "receipt", "snapshot", "old_run", "missing_output", "wrong_type"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, consumer, _ := runtimeInputResultFixture(t)
			switch scenario {
			case "pending":
				source.Status = TaskInProgress
			case "model":
				source.TypedResult.Source = "submitted"
			case "digest":
				source.TypedResult.RuntimeOutputsHash = "sha256:tampered"
			case "receipt":
				source.ExecutionReceipt.RuntimeOutputsHash = "sha256:other"
			case "snapshot":
				source.RunInputSnapshotID = "other"
			case "old_run":
				source.ExecutionReceipt.RunID = "run-prior"
			case "missing_output":
				delete(source.TypedResult.RuntimeOutputs, "observation")
			case "wrong_type":
				setRuntimeInputObservation(t, c, source, map[string]any{"count": "2", "literal": "native output must not be coerced"})
			}
			if payload, rejection := c.structuredPayloadForSubmission(consumer.ID, []byte(`{"decision":true}`)); payload != nil || rejection == "" {
				t.Fatalf("accepted %s runtime source", scenario)
			}
		})
	}
}

func TestRuntimeResultInputsCompileAndProviderProjection(t *testing.T) {
	t.Parallel()
	c, _, consumer, compiled := runtimeInputResultFixture(t)
	projected := providerVisibleResultPayloadSchema(*consumer.ResultContract, compiled)
	properties, _ := projected["properties"].(map[string]any)
	if _, exposed := properties["observation"]; exposed {
		t.Fatal("provider asked to echo runtime-owned data")
	}
	for _, required := range projected["required"].([]any) {
		if required == "observation" {
			t.Fatal("provider schema requires runtime-owned data")
		}
	}
	prompt := resultContractPromptSection(consumer.ResultContract, compiled, true)
	if !strings.Contains(prompt, "Omit this field entirely") || !strings.Contains(prompt, "prepare-outputs") {
		t.Fatal("external repair instructions omitted runtime binding")
	}
	c.session.Agents = map[string]*agent.AgentDef{"consumer": {Name: "consumer", Role: "worker"}}
	c.session.ContractTasks = []TaskDef{
		{ID: "prepare-outputs", Agent: "consumer", Action: &Action{Type: "prepare"}},
		{ID: "consume", Agent: "consumer", ResultContract: consumer.ResultContract},
	}
	if err := validateRuntimeInputSources(c.session, c.session.ResultContracts, nil); err != nil {
		t.Fatal(err)
	}
	c.session.ContractTasks[0].Action = nil
	if err := validateRuntimeInputSources(c.session, c.session.ResultContracts, nil); err == nil {
		t.Fatal("model-owned output was accepted as a declared native source")
	}
}

func TestRuntimeResultInputDeclarationsFailClosed(t *testing.T) {
	t.Parallel()
	for _, change := range []string{
		`[{"target":"/observation","source_task":"prepare-outputs","output":"observation","ignore_receipt":true}]`,
		`[{"target":"","source_task":"prepare-outputs","output":"observation"}]`,
		`[{"target":"/missing","source_task":"prepare-outputs","output":"observation"}]`,
		`[{"target":"/observation","source_task":"prepare-outputs","output":""}]`,
		`[{"target":"/observation","source_task":"prepare-outputs","output":"observation"},{"target":"/observation/count","source_task":"prepare-outputs","output":"observation"}]`,
		`[]`, `null`,
	} {
		t.Run(change, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(runtimeInputTestSchema), &root); err != nil {
				t.Fatal(err)
			}
			var annotation any
			if err := json.Unmarshal([]byte(change), &annotation); err != nil {
				t.Fatal(err)
			}
			root["x-hufu-runtime-inputs"] = annotation
			if _, err := compileResultRuntimeInputs(root); err == nil {
				t.Fatal("invalid runtime binding declaration accepted")
			}
		})
	}
}

func TestRuntimeResultInputsExternalRechecksReceipt(t *testing.T) {
	t.Parallel()
	c, source, consumer, compiled := runtimeInputResultFixture(t)
	if _, err := c.resultRuntimeInputValues(compiled, ""); err != nil {
		t.Fatal(err)
	}
	request := AttemptRequest{Task: taskDefFromTodoItem(consumer), resultContractSchema: compiled,
		resultRuntimeInputs: func(spec *CompiledResultContract) (map[string]any, error) {
			return c.resultRuntimeInputValues(spec, "")
		}}
	source.ExecutionReceipt.RuntimeOutputsHash = "sha256:changed-after-preflight"
	if payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(`{"decision":true}`)}, nil); payload != nil || err == nil {
		t.Fatal("external proposal bypassed a changed native receipt")
	}
	request.resultRuntimeInputs = nil
	if payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(`{"decision":true}`)}, nil); payload != nil || err == nil {
		t.Fatal("external proposal supplied its own runtime binding values")
	}
}

func TestRuntimeResultInputsExternalCompletion(t *testing.T) {
	t.Parallel()
	c, source, consumer, _ := runtimeInputResultFixture(t)
	compiled := compileFixtureJSONSchema(t, "completed.json", `{
		"type":"object","additionalProperties":false,"required":["decision","kind","completion"],
		"properties":{"decision":{"type":"boolean"},"kind":{"type":"string","const":"report"},"completion":{
			"type":"object","required":["status","summary"],"properties":{"status":{"type":"string"},"summary":{"type":"string"}}
		}},
		"x-hufu-runtime-inputs":[{"target":"/kind","source_kind":"const"},{"target":"/completion","source_task":"prepare-outputs","output":"observation"}],
		"x-hufu-runtime-completion":"/completion",
		"x-hufu-result-envelope":{"properties":{"status":{"enum":["success","completed_with_gaps","blocked"]}}}
	}`)
	setRuntimeInputObservation(t, c, source, map[string]any{"status": "completed_with_gaps", "summary": "One gap remains"})
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	request := scopedAttemptRequest("codex", consumer.ID)
	request.Task = taskDefFromTodoItem(consumer)
	request.Task.ResultContract = &ref
	request.resultContractSchema = compiled
	request.resultRuntimeInputs = func(spec *CompiledResultContract) (map[string]any, error) {
		return c.resultRuntimeInputValues(spec, "")
	}
	root := t.TempDir()
	submit := func(raw string) (*TaskResult, error) {
		proposal, err := decodeWorkerResultProposal([]byte(raw), true)
		if err != nil {
			return nil, err
		}
		return NewExternalResultCanonicalizer().Canonicalize(t.Context(), request, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	}
	if _, err := decodeWorkerResultProposal([]byte("null"), true); err == nil {
		t.Fatal("accepted a non-object proposal through runtime completion")
	}
	completed, err := submit(`{"status":null,"summary":null,"structured_payload_json":"{\"decision\":true}"}`)
	if err != nil || completed.Status != TaskResultStatusCompletedWithGaps || completed.Summary != "One gap remains" ||
		completed.StructuredPayload == nil || !bytes.Contains(completed.StructuredPayload.Value, []byte(`"kind":"report"`)) {
		t.Fatalf("runtime did not assemble the external completion: %+v, %v", completed, err)
	}
	for _, raw := range []string{
		`{"status":"completed_with_gaps","summary":"One gap remains","structured_payload_json":"{\"decision\":true}"}`,
		`{"structured_payload_json":"{\"decision\":true,\"kind\":\"report\"}"}`,
		`{"structured_payload_json":"{\"decision\":true,\"completion\":null}"}`,
		`{"status":"blocked","summary":"Source unavailable","structured_payload_json":"{\"kind\":\"report\"}"}`,
	} {
		if _, err := submit(raw); err == nil {
			t.Fatalf("accepted a runtime-owned override: %s", raw)
		}
	}
	blocked, err := submit(`{"status":"blocked","summary":"Source unavailable"}`)
	if err != nil || blocked.Status != TaskResultStatusBlocked || blocked.Summary != "Source unavailable" || blocked.StructuredPayload != nil {
		t.Fatalf("lost external blocker: %+v, %v", blocked, err)
	}
	if _, err := submit(`{"status":"failed","summary":"Source unavailable"}`); localResultRejectionReason(err) != RepairFailureInvalidSchema {
		t.Fatalf("external failure bypassed its status envelope: %v", err)
	}
	admission := localResultAdmission{coordinator: c, todoID: consumer.ID}
	rejected := &TaskResult{Status: TaskResultStatusFailed, Summary: "Source unavailable"}
	if err := admission.bindResultEnvelope(t.Context(), compiled, SubmitResultInput{}, rejected, taskResultSubmissionContract{}, false); localResultRejectionReason(err) != RepairFailureInvalidSchema {
		t.Fatalf("local failure bypassed its status envelope: %v", err)
	}
	source.ExecutionReceipt.RuntimeOutputsHash = "changed"
	blocked, err = submit(`{"status":"blocked","summary":"Source unavailable"}`)
	if err != nil || blocked == nil || blocked.Status != TaskResultStatusBlocked || blocked.StructuredPayload != nil {
		t.Fatalf("blocker incorrectly required the successful completion source: %+v, %v", blocked, err)
	}
	for _, raw := range []string{
		`{"structured_payload_json":"{\"decision\":true}"}`,
		`{"status":"completed_with_gaps","summary":"One gap remains","structured_payload_json":"{\"decision\":true}"}`,
	} {
		if _, err := submit(raw); localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("changed native receipt was not a source failure: %v", err)
		}
	}
}

func TestRuntimeResultInputsExternalCompletionRequiresWorksetSourceBeforeRepair(t *testing.T) {
	t.Parallel()
	compiled := compileFixtureJSONSchema(t, "external-completion-workset", `{
		"type":"object","required":["completion","location"],
		"properties":{
			"completion":{"type":"object","required":["status","summary"],
				"properties":{"status":{"type":"string"},"summary":{"type":"string"}},
				"const":{"status":"success","summary":"Verified"}},
			"location":{"type":"string"}
		},
		"x-hufu-runtime-inputs":[{"target":"/completion","source_kind":"const"}],
		"x-hufu-runtime-completion":"/completion",
		"x-hufu-workset-paths":{"pointers":["/location"],"format":"path_line"}
	}`)
	request := scopedAttemptRequest("codex", "workset-consumer")
	request.Task.ResultContract = new(compiled.ref(true))
	request.resultContractSchema = compiled
	c := newTestCoordinator(t)
	request.resultRuntimeInputs = func(spec *CompiledResultContract) (map[string]any, error) {
		return c.resultRuntimeInputValues(spec, "")
	}
	root := t.TempDir()
	for _, tc := range []struct {
		status   string
		required bool
	}{
		{TaskResultStatusSuccess, true},
		{TaskResultStatusBlocked, true},
		{TaskResultStatusFailed, true},
		{TaskResultStatusPartial, true},
		{TaskResultStatusBlocked, false},
	} {
		request.Task.ResultContract = new(compiled.ref(tc.required))
		proposal := &WorkerResultProposal{Status: tc.status, Summary: "workset unavailable"}
		result, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
		if result != nil || err == nil || localResultRejectionReason(err) != RepairFailureSource {
			t.Fatalf("status %s: result=%+v error=%v", tc.status, result, err)
		}
	}
}

func TestExternalOptionalPayloadRechecksIndependentNativeSource(t *testing.T) {
	t.Parallel()
	c, source, consumer, compiled := runtimeInputResultFixture(t)
	if _, err := c.resultRuntimeInputValues(compiled, ""); err != nil {
		t.Fatal(err)
	}
	request := scopedAttemptRequest("codex", consumer.ID)
	request.Task = taskDefFromTodoItem(consumer)
	request.Task.ResultContract = new(compiled.ref(false))
	request.resultContractSchema = compiled
	request.resultRuntimeInputs = func(spec *CompiledResultContract) (map[string]any, error) {
		return c.resultRuntimeInputValues(spec, "")
	}
	root := t.TempDir()
	proposal := AttemptResult{ResultProposal: &WorkerResultProposal{Status: TaskResultStatusSuccess, Summary: "No payload needed"}}
	canonicalizer := NewExternalResultCanonicalizer()
	if result, err := canonicalizer.Canonicalize(t.Context(), request, proposal, WorkspaceDelta{}, root); err != nil || result == nil || result.StructuredPayload != nil {
		t.Fatalf("optional envelope with a valid source was rejected: %+v, %v", result, err)
	}
	source.ExecutionReceipt.RuntimeOutputsHash = "changed"
	if result, err := canonicalizer.Canonicalize(t.Context(), request, proposal, WorkspaceDelta{}, root); result != nil || err == nil || localResultRejectionReason(err) != RepairFailureSource {
		t.Fatalf("optional envelope bypassed the changed source: %+v, %v", result, err)
	}
}

func TestRuntimeResultInputsExternalCompletionPreservesCanonicalSummary(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"long summary", "downgraded tool evidence"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, consumer, _ := runtimeInputResultFixture(t)
			schema := strings.Replace(evidenceTestSchema, `"properties":{"findings"`, `"properties":{
				"completion":{"type":"object","required":["status","summary"],"properties":{"status":{"type":"string"},"summary":{"type":"string"}}},
				"findings"`, 1)
			schema = strings.Replace(schema, `"required":["findings"]`, `"required":["completion","findings"],
				"x-hufu-runtime-inputs":[{"target":"/completion","source_task":"prepare-outputs","output":"observation"}],
				"x-hufu-runtime-completion":"/completion"`, 1)
			compiled := compileFixtureJSONSchema(t, "completion.json", schema)
			summary := "Receipt-backed completion with gaps"
			wantDowngrades := 1
			claim := evidenceTestPayload
			if scenario == "long summary" {
				summary = strings.Repeat("界", workerResultProposalMaxTextRunes+1)
				wantDowngrades = 0
				claim = `{"findings":[]}`
			}
			setRuntimeInputObservation(t, c, source, map[string]any{"status": "completed_with_gaps", "summary": summary})
			ref := compiled.ref(true)
			request := scopedAttemptRequest("codex", consumer.ID)
			request.Task = taskDefFromTodoItem(consumer)
			request.Task.ResultContract = &ref
			request.resultContractSchema = compiled
			request.resultRuntimeInputs = func(spec *CompiledResultContract) (map[string]any, error) {
				return c.resultRuntimeInputValues(spec, "")
			}
			proposal := &WorkerResultProposal{StructuredPayloadJSON: &claim}
			result, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary != summary || result.StructuredPayload == nil || result.StructuredPayload.EvidenceDowngrades != wantDowngrades {
				t.Fatalf("canonical envelope summary has %d bytes, want %d; evidence summary matched=%t, payload=%v",
					len(result.Summary), len(summary), result.Summary == summary, result.StructuredPayload != nil)
			}
			if err := validateResultEnvelope(compiled, result); err != nil {
				t.Fatalf("canonical result failed envelope replay validation: %v", err)
			}
		})
	}
}

func TestRuntimeResultInputsKeepAggregateBound(t *testing.T) {
	t.Parallel()
	c, source, consumer, _ := runtimeInputResultFixture(t)
	setRuntimeInputObservation(t, c, source, map[string]any{"count": 1, "literal": strings.Repeat("a", 128<<10)})
	var root map[string]any
	if err := json.Unmarshal([]byte(runtimeInputTestSchema), &root); err != nil {
		t.Fatal(err)
	}
	properties := root["properties"].(map[string]any)
	observation := properties["observation"]
	delete(properties, "observation")
	var bindings []any
	for i := range 20 {
		name := fmt.Sprintf("bound_%d", i)
		properties[name] = observation
		bindings = append(bindings, map[string]any{"target": "/" + name, "source_task": "prepare-outputs", "output": "observation"})
	}
	root["required"] = []string{"decision"}
	root["x-hufu-runtime-inputs"] = bindings
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "consumer.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	compiled, err := compileResultContractSchema(dir, "consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts[compiled.ID] = compiled
	ref := compiled.ref(true)
	consumer.ResultContract = &ref
	if payload, rejection := c.structuredPayloadForSubmission(consumer.ID, []byte(`{"decision":true}`)); payload != nil || !strings.Contains(rejection, "byte limit") {
		t.Fatalf("oversized runtime-bound result was accepted: %s", rejection)
	}
}

func TestRuntimeResultInputsAllowLocalFailureWithoutSuccessfulReceipt(t *testing.T) {
	const schema = `{
		"type":"object","additionalProperties":false,"required":["completion"],
		"properties":{"completion":{"type":"object","required":["status","summary"],"properties":{"status":{"type":"string"},"summary":{"type":"string"}}}},
		"x-hufu-runtime-inputs":[{"target":"/completion","source_task":"missing-action","output":"report"}],
		"x-hufu-runtime-completion":"/completion"
	}`
	for _, status := range []string{TaskResultStatusBlocked, TaskResultStatusPartial, TaskResultStatusFailed} {
		t.Run(status, func(t *testing.T) {
			raw := fmt.Sprintf(`{"status":%q,"summary":"Native action did not complete"}`, status)
			c, item, ctx, cp := receivedCollectionClaimsFixture(t, schema, raw)
			prepared, err := c.prepareCollectionClaims(ctx, cp)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.commitPreparedCollection(ctx, prepared); err != nil {
				t.Fatal(err)
			}
			if result := c.GetTaskResult(item.ID); result == nil || result.Status != status || result.StructuredPayload != nil {
				t.Fatalf("collected result = %+v", result)
			}
			c, item, ctx, _ = receivedCollectionClaimsFixture(t, schema, raw)
			tool := &submitResultTool{coordinator: c, todoID: item.ID}
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: raw})
			if err != nil || response.IsError {
				t.Fatalf("direct result = %+v, %v", response, err)
			}
			if result := c.GetTaskResult(item.ID); result == nil || result.Status != status || result.StructuredPayload != nil {
				t.Fatalf("submitted result = %+v", result)
			}
		})
	}
}
