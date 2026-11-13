package team

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"testing"
)

// The source is a completed public RunTask, with an actual MCP invocation,
// canonical publication and immutable native action receipt.
func testRuntimeMediaResultInputs(t *testing.T, c *Coordinator, source *TodoItem) {
	t.Helper()
	dir := t.TempDir()
	schema := fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["decision","media"],"properties":{"decision":{"type":"boolean"},"media":{"type":"object","required":["hash","sources"]}},"x-hufu-runtime-inputs":[{"target":"/media","source_task":%q,"source_kind":"runtime_media"}]}`, source.ID)
	writeResultContractSchema(t, dir, "media.json", schema)
	compiled, err := compileResultContractSchema(dir, "media.json")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	consumer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", ResultContract: &ref}})[0]
	consumer.Status = TaskInProgress
	values, err := c.resultRuntimeInputValues(compiled, "")
	if err != nil {
		t.Fatal(err)
	}
	var local *ResultPayload
	for _, route := range []string{"local", "external"} {
		submit := func(raw string) (*ResultPayload, string) {
			if route == "local" {
				return c.structuredPayloadForSubmission(consumer.ID, []byte(raw))
			}
			request := AttemptRequest{Task: taskDefFromTodoItem(consumer), resultContractSchema: compiled,
				resultRuntimeInputs: func() (map[string]any, error) { return c.resultRuntimeInputValues(compiled, "") }}
			payload, err := canonicalExternalStructuredPayload(request, &WorkerResultProposal{StructuredPayloadJSON: new(raw)}, nil)
			if err != nil {
				return nil, err.Error()
			}
			return payload, ""
		}
		exact, err := json.Marshal(map[string]any{"decision": true, "media": values["/media"]})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{`{"decision":true,"media":null}`, string(exact), `{"decision":"invalid"}`} {
			if payload, rejection := submit(raw); payload != nil || rejection == "" {
				t.Fatalf("%s accepted a model override or invalid judgment", route)
			}
		}
		payload, rejection := submit(`{"decision":true}`)
		if payload == nil || rejection != "" {
			t.Fatalf("%s bounded field repair failed: %s", route, rejection)
		}
		if local == nil {
			local = payload
		} else if !bytes.Equal(local.Value, payload.Value) || local.SHA256 != payload.SHA256 {
			t.Fatal("local and external runtime media bindings differ")
		}
		if err := c.validateSubmittedEvidenceInputs(consumer.ID, payload); err != nil {
			t.Fatal(err)
		}
	}
	workspace := t.TempDir()
	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	if err := journal.append(journalRecord{Op: "result", TaskID: consumer.ID, RunID: c.executionRunID,
		TypedResult: &TaskResult{TaskID: consumer.ID, Status: TaskResultStatusSuccess, StructuredPayload: local}}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	var exported bytes.Buffer
	if err := ExportTaskJournal(t.Context(), workspace, &exported); err != nil {
		t.Fatal(err)
	}
	var restored journalRecord
	if err := json.Unmarshal(bytes.TrimSpace(exported.Bytes()), &restored); err != nil {
		t.Fatal(err)
	}
	if restored.TypedResult == nil || restored.TypedResult.StructuredPayload == nil ||
		!bytes.Equal(local.Value, restored.TypedResult.StructuredPayload.Value) || local.SHA256 != restored.TypedResult.StructuredPayload.SHA256 ||
		c.validateSubmittedEvidenceInputs(consumer.ID, restored.TypedResult.StructuredPayload) != nil {
		t.Fatal("runtime media binding changed during durable replay")
	}
	// Recheck the original binding, then reject both stale and substituted
	// receipts without repairing or rewriting the source projection.
	original := cloneTodoItem(source)
	for _, change := range []func(){
		func() { source.ExecutionReceipt.RuntimeMediaHash = "sha256:changed" },
		func() { source.ExecutionReceipt.RunID = "another-run" },
		func() { source.RunInputSnapshotHash = "sha256:other-input" },
	} {
		change()
		for index := range source.ExecutionReceipts {
			source.ExecutionReceipts[index].RuntimeMediaHash = source.ExecutionReceipt.RuntimeMediaHash
			source.ExecutionReceipts[index].RunID = source.ExecutionReceipt.RunID
		}
		items := c.taskTracker.TodoList().Items()
		for index, item := range items {
			if item.ID == source.ID {
				items[index] = cloneTodoItem(source)
			}
		}
		c.taskTracker.TodoList().Restore(items)
		if _, err := c.resultRuntimeInputValues(compiled, ""); err == nil {
			t.Fatal("runtime media binding accepted a stale or substituted source")
		}
		source = cloneTodoItem(original)
	}
}

func TestRuntimeMediaInputDeclaration(t *testing.T) {
	for _, field := range []string{`"runtime_media":null`, `"runtime_media":{}`} {
		if _, err := DecodeWorkerResultProposal([]byte(validProposalJSON(field))); err == nil {
			t.Fatal("external proposal supplied a runtime-owned media envelope")
		}
		if _, err := decodeSubmitResultInput([]byte(validProposalJSON(field)), taskResultSubmissionContract{}); err == nil {
			t.Fatal("local submission supplied a runtime-owned media envelope")
		}
	}
	for _, scenario := range []struct {
		kind, output string
		valid        bool
	}{
		{"runtime_media", "", true}, {"runtime_output", "summary", true}, {"", "summary", true},
		{"runtime_media", "summary", false}, {"runtime_output", "", false}, {"unknown", "", false},
	} {
		root := map[string]any{"type": "object", "properties": map[string]any{"media": map[string]any{"type": "object"}},
			"x-hufu-runtime-inputs": []any{map[string]any{"target": "/media", "source_task": "capture", "source_kind": scenario.kind, "output": scenario.output}}}
		_, err := compileResultRuntimeInputs(root)
		if (err == nil) != scenario.valid {
			t.Fatalf("kind %q output %q: %v", scenario.kind, scenario.output, err)
		}
	}
}
