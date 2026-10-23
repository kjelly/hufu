package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
					resultRuntimeInputs: func() (map[string]any, error) { return c.resultRuntimeInputValues(compiled) }}
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
			if !bytes.Equal(payload.Value, restored.Value) || payload.SHA256 != restored.SHA256 || c.validateSubmittedEvidenceInputs(consumer.ID, &restored) != nil {
				t.Fatal("canonical binding did not survive persistence/replay")
			}
			setRuntimeInputObservation(t, c, source, map[string]any{"count": 3, "literal": "different verified output"})
			if c.validateSubmittedEvidenceInputs(consumer.ID, &restored) == nil {
				t.Fatal("historical payload silently accepted a different verified source")
			}
		})
	}
}

func TestRuntimeResultInputsRejectUntrustedOrMistypedSources(t *testing.T) {
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
	c, source, consumer, compiled := runtimeInputResultFixture(t)
	if _, err := c.resultRuntimeInputValues(compiled); err != nil {
		t.Fatal(err)
	}
	request := AttemptRequest{Task: taskDefFromTodoItem(consumer), resultContractSchema: compiled,
		resultRuntimeInputs: func() (map[string]any, error) { return c.resultRuntimeInputValues(compiled) }}
	source.ExecutionReceipt.RuntimeOutputsHash = "sha256:changed-after-preflight"
	if payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(`{"decision":true}`)}, nil); payload != nil || err == nil {
		t.Fatal("external proposal bypassed a changed native receipt")
	}
	request.resultRuntimeInputs = nil
	if payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(`{"decision":true}`)}, nil); payload != nil || err == nil {
		t.Fatal("external proposal supplied its own runtime binding values")
	}
}

func TestRuntimeResultInputsKeepAggregateBound(t *testing.T) {
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
