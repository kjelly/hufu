package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func toolEvidenceRepairFixture(t *testing.T) (*Coordinator, string, TaskDef) {
	t.Helper()
	c := newTestCoordinator(t, withMaxRetries(1))
	c.session.Agents["worker"].SideEffect = string(SideEffectNone)
	dir := t.TempDir()
	schema := strings.ReplaceAll(evidenceTestSchema, `"tool":"read_document"`, `"tool":"view"`)
	schema = strings.ReplaceAll(schema, `"input_pointer":"/key"`, `"input_pointer":"/file_path"`)
	writeResultContractSchema(t, dir, "evidence.json", schema)
	compiled, err := compileResultContractSchema(dir, "evidence.json")
	if err != nil {
		t.Fatal(err)
	}
	c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
	ref := compiled.ref(true)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "read the document", SideEffect: SideEffectNone,
		ResultContract: &ref, Execution: ExecutionContract{RequiresResult: true}}})
	task := taskDefFromTodoItem(items[0])
	return c, items[0].ID, task
}

func observedDocumentTurn(t *testing.T) scriptedTurn {
	t.Helper()
	return scriptedTurn{stream: func(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		observation := fantasy.ToolCallContent{ToolCallID: "original-read", ToolName: "view", Input: `{"file_path":"document-a"}`}
		result := fantasy.ToolResultContent{ToolCallID: observation.ToolCallID, ToolName: observation.ToolName,
			Result: fantasy.ToolResultOutputContentText{Text: `{"text":"` + strings.Repeat("navigation ", maxRepairObservationRunes/10+1) + `the exact words in a document"}`}}
		if err := call.OnToolCall(observation); err != nil {
			return nil, err
		}
		if err := call.OnToolResult(result); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{
			Steps:    []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{observation, result}}}},
			Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Ready to submit the report."}}},
		}, nil
	}}
}

func submitEvidenceRepairTurn(t *testing.T, c *Coordinator, id string) scriptedTurn {
	t.Helper()
	return scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if recorder, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript); recorder != nil {
			t.Fatal("result repair can append to the original transcript")
		}
		toolCall := submitResultCall("repair-submit", `{"status":"success","summary":"Document checked","structured_payload":`+evidenceTestPayload+`}`)
		if err := call.OnToolCall(toolCall); err != nil {
			return nil, err
		}
		response, err := (&submitResultTool{coordinator: c, todoID: id}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: toolCall.Input})
		if err != nil || response.IsError {
			t.Fatalf("submit_result: response=%#v error=%v", response, err)
		}
		result := submitResultOutcome(toolCall.ToolCallID, "")
		if err := call.OnToolResult(result); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{toolCall, result}}}}}, nil
	}}
}

func TestProtocolRepairPreservesToolEvidence(t *testing.T) {
	for _, initialFailure := range []string{"none", "schema", "preparation", "exhausted", "no_tool_call", "no_tool_call_exhausted", "empty_output", "empty_output_exhausted"} {
		t.Run(initialFailure, func(t *testing.T) {
			c, id, task := toolEvidenceRepairFixture(t)
			workerTurn := observedDocumentTurn(t)
			if strings.HasPrefix(initialFailure, "empty_output") {
				observe := workerTurn.stream
				workerTurn.stream = func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
					result, err := observe(ctx, call)
					if result != nil {
						result.Response.Content = nil
					}
					return result, err
				}
			}
			worker := scriptWorker(workerTurn)
			c.workerAgentOverride = worker
			turn := submitEvidenceRepairTurn(t, c, id)
			var repair *scriptedWorker
			switch initialFailure {
			case "schema":
				repair = scriptWorker(scriptedTurn{stream: func(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
					return &fantasy.AgentResult{Steps: invalidSchemaRepairSteps()}, nil
				}}, turn)
			case "preparation":
				repair = scriptWorker(scriptedTurn{err: errors.New("repair stream failed")}, turn)
			case "exhausted":
				repair = scriptWorker(scriptedTurn{err: errors.New("repair stream failed")})
			case "no_tool_call", "empty_output":
				repair = scriptWorker(textTurn("The report is ready."), turn)
			case "no_tool_call_exhausted", "empty_output_exhausted":
				repair = scriptWorker(textTurn("The report is ready."))
			default:
				repair = scriptWorker(turn)
			}
			c.repairAgentOverride = repair
			ctx := context.WithValue(withTestProtocolRepairInvocationContext(t.Context()), taskTranscriptKey{}, &taskTranscript{todoID: "parent-task"})
			_, err := c.executeTask(ctx, task, id)
			if worker.Calls() != 1 {
				t.Fatalf("worker was replayed %d times", worker.Calls())
			}
			if !strings.Contains(repair.Prompt(t, 1), "the exact words in a document") {
				t.Fatal("repair saw only the navigation prefix instead of the source quote")
			}
			if strings.HasSuffix(initialFailure, "exhausted") {
				item := todoByID(t, c, id)
				if err == nil || item.Status != TaskBlocked || repair.Calls() != 2 || item.ExecutionReceipt.TranscriptRef == "" {
					t.Fatalf("exhaustion lost its recovery boundary: task=%#v calls=%d error=%v", item, repair.Calls(), err)
				}
				if item.FailureEvent == nil || item.FailureEvent.RetryDisposition != ReconcileOnly {
					t.Fatal("exhausted result repair did not require reconciliation")
				}
				if item.TypedResult == nil || item.TypedResult.Status != TaskResultStatusPartial || item.TypedResult.RawOutputRef == nil || strings.Contains(item.TypedResult.Summary, "no usable execution evidence") {
					t.Fatal("exhausted repair discarded observed evidence because the final text was empty")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			item := todoByID(t, c, id)
			payload := item.TypedResult.StructuredPayload
			if item.Status != TaskDone || payload == nil || payload.EvidenceDowngrades != 0 || !bytes.Contains(payload.Value, []byte(`"call_id":"original-read"`)) {
				t.Fatalf("repair lost the original tool evidence: %#v", item.TypedResult)
			}
			if item.ExecutionReceipt.Attempt != 1 || len(item.ExecutionReceipts) != 1 || item.ExecutionReceipt.ResultPayloadSHA256 != payload.SHA256 || item.ExecutionReceipt.ResultValidation != ResultValidationValid {
				t.Fatal("result repair created a new execution occurrence")
			}
			original := readStoredArtifact(t, c.session.Workspace, item.ExecutionReceipt.TranscriptRef)
			if bytes.Contains(original, []byte("repair-submit")) || !bytes.Contains(original, []byte("original-read")) {
				t.Fatal("repair rewrote the original execution evidence")
			}
			// Serialize through the same canonical projections persisted by the
			// event store; the receipt and full result must retain their bindings.
			data, err := json.Marshal(taskTransitionPayload(item))
			if err != nil {
				t.Fatal(err)
			}
			replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: data}})
			if len(replayed) != 1 {
				t.Fatal("task replay lost the repaired occurrence")
			}
			restored := replayed[0]
			evidence, err := c.restoreProtocolRepairToolEvidence(t.Context(), restored)
			if err != nil || len(evidence.evidenceRecords()) == 0 || restored.TypedResult.StructuredPayload.SHA256 != payload.SHA256 {
				t.Fatalf("persisted repair lost evidence: error=%v", err)
			}
		})
	}
}

func interruptedToolEvidenceFixture(t *testing.T, c *Coordinator, id string, task TaskDef, attempt int) *TodoItem {
	t.Helper()
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, id, c.executionRunID, attempt, "worker")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordToolCall("original-read", "view", `{"file_path":"document-a"}`); err != nil {
		t.Fatal(err)
	}
	if err := transcript.RecordToolResult("original-read", "view", `{"text":"`+strings.Repeat("navigation ", maxRepairObservationRunes/10+1)+`the exact words in a document"}`, false); err != nil {
		t.Fatal(err)
	}
	ref, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	receipt := &ExecutionReceipt{RunID: c.executionRunID, TaskID: id, Attempt: attempt, ProducerID: "worker", TranscriptRef: ref.ID,
		ResultContractID: task.ResultContract.ID, ResultContractSHA256: task.ResultContract.SchemaSHA256}
	if err := c.setAttemptReceipt(id, receipt); err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), id, TaskProtocolIncomplete, "missing result", "Ready to submit the report.", nil); err != nil {
		t.Fatal(err)
	}
	return todoByID(t, c, id)
}

func TestResumedProtocolRepairPreservesToolEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		attempt    int
		empty      bool
		prior      RepairFailureReason
		firstFails bool
	}{
		{name: "first attempt", attempt: 1},
		{name: "later attempt", attempt: 3},
		{name: "empty output", attempt: 1, empty: true},
		{name: "prior missing call", attempt: 3, empty: true, prior: RepairFailureNoToolCall},
		{name: "missing call retry", attempt: 3, firstFails: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, id, task := toolEvidenceRepairFixture(t)
			item := interruptedToolEvidenceFixture(t, c, id, task, scenario.attempt)
			if scenario.empty {
				if err := c.commitTaskTransitionFromCurrent(t.Context(), id, TaskProtocolIncomplete, "missing result", "", nil); err != nil {
					t.Fatal(err)
				}
				item = todoByID(t, c, id)
			}
			if scenario.prior != "" {
				receipt := cloneExecutionReceipt(item.ExecutionReceipt)
				receipt.RepairProvenance = &RepairProvenance{Attempted: true, RepairAttempts: 1, FailureReason: scenario.prior,
					History: []RepairAttemptProvenance{{Attempt: 1, FailureReason: scenario.prior}}}
				if err := c.setAttemptReceipt(id, &receipt); err != nil {
					t.Fatal(err)
				}
				item = todoByID(t, c, id)
			}
			before := readStoredArtifact(t, c.session.Workspace, item.ExecutionReceipt.TranscriptRef)
			worker := scriptWorker(textTurn("worker must not run"))
			c.workerAgentOverride = worker
			turns := []scriptedTurn{submitEvidenceRepairTurn(t, c, id)}
			if scenario.firstFails {
				turns = append([]scriptedTurn{textTurn("Ready to submit.")}, turns...)
			}
			repair := scriptWorker(turns...)
			c.repairAgentOverride = repair
			if _, err := c.resumeProtocolIncompleteTask(withTestProtocolRepairInvocationContext(t.Context()), task, item); err != nil {
				t.Fatal(err)
			}
			item = todoByID(t, c, id)
			if worker.Calls() != 0 || repair.Calls() != len(turns) || item.Status != TaskDone || item.TypedResult.StructuredPayload.EvidenceDowngrades != 0 {
				t.Fatalf("resumed repair lost evidence: %#v", item)
			}
			if !strings.Contains(repair.Prompt(t, 1), "the exact words in a document") || item.ExecutionReceipt.Attempt != scenario.attempt || item.ExecutionReceipt.ResultValidation != ResultValidationValid || item.ExecutionReceipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
				t.Fatal("resumed repair lost original facts or attempt identity")
			}
			after := readStoredArtifact(t, c.session.Workspace, item.ExecutionReceipt.TranscriptRef)
			if !bytes.Equal(before, after) {
				t.Fatal("resumed repair modified the original transcript")
			}
		})
	}
}

func TestProtocolRepairRejectsUnboundToolEvidence(t *testing.T) {
	for _, mismatch := range []string{"run", "task", "agent", "snapshot", "schema", "scope", "missing", "tampered"} {
		t.Run(mismatch, func(t *testing.T) {
			c, id, task := toolEvidenceRepairFixture(t)
			item := interruptedToolEvidenceFixture(t, c, id, task, 1)
			switch mismatch {
			case "run":
				item.ExecutionReceipt.RunID = "other-run"
			case "task":
				item.ExecutionReceipt.TaskID = "other-task"
			case "agent":
				item.ExecutionReceipt.ProducerID = "other-worker"
			case "snapshot":
				item.ExecutionReceipt.RunInputSnapshotHash = "other-snapshot"
			case "schema":
				item.ExecutionReceipt.ResultContractSHA256 = "other-schema"
			case "scope":
				item.ExecutionReceipt.ArtifactScope = &ArtifactAccessScope{RunID: c.executionRunID, TaskID: id, Attempt: 2}
			case "missing":
				item.ExecutionReceipt.TranscriptRef = "sha256-" + strings.Repeat("0", 64)
			case "tampered":
				store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.session.Workspace)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := store.Get(t.Context(), item.ExecutionReceipt.TranscriptRef)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.root, "data", ref.ID), []byte("forged transcript"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if evidence, err := c.restoreProtocolRepairToolEvidence(t.Context(), item); err == nil || evidence != nil {
				t.Fatalf("admitted %s evidence: %#v error=%v", mismatch, evidence, err)
			}
			repair := scriptWorker(textTurn("must not run"))
			c.repairAgentOverride = repair
			if _, err := c.resumeProtocolIncompleteTask(withTestProtocolRepairInvocationContext(t.Context()), task, item); err == nil || repair.Calls() != 0 || todoByID(t, c, id).Status != TaskBlocked {
				t.Fatalf("unbound evidence reached a repair model: calls=%d error=%v", repair.Calls(), err)
			}
		})
	}
}

func TestSubmissionRepairEvidenceScope(t *testing.T) {
	for _, mismatch := range []string{"none", "run", "task", "agent", "attempt", "ordinary_submission"} {
		t.Run(mismatch, func(t *testing.T) {
			c, id, _ := toolEvidenceRepairFixture(t)
			ctx := occurrenceTestContext(c, id, 1)
			transcript := &taskTranscript{todoID: id, runID: c.executionRunID, agent: "worker", attempt: 1, evidence: evidenceTestRecords()}
			switch mismatch {
			case "run":
				transcript.runID = "other-run"
			case "task":
				transcript.todoID = "other-task"
			case "agent":
				transcript.agent = "other-agent"
			case "attempt":
				transcript.attempt = 2
			}
			ctx = context.WithValue(ctx, protocolRepairToolEvidenceKey{}, transcript)
			ctx = context.WithValue(ctx, protocolRepairExecutionKey{}, mismatch != "ordinary_submission")
			records, err := submissionToolEvidence(ctx, c, id)
			if err != nil || (len(records) > 0) != (mismatch == "none") {
				t.Fatalf("scope %s: records=%d error=%v", mismatch, len(records), err)
			}
		})
	}
}
