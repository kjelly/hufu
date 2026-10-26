package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"testing"

	"charm.land/fantasy"
)

func failureResponseSteps(response fantasy.ToolResponse) []fantasy.StepResult {
	return []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.ToolResultContent{ToolName: submitResultToolName, ClientMetadata: response.Metadata, Result: fantasy.ToolResultOutputContentError{Error: errors.New(response.Content)}},
	}}}}
}

func TestSubmissionFailureClassificationUsesRuntimeMetadata(t *testing.T) {
	for _, reason := range []RepairFailureReason{RepairFailureInvalidSchema, RepairFailureSource, RepairFailureContract, RepairFailureCommit} {
		t.Run(string(reason), func(t *testing.T) {
			response := submissionFailureResponse(reason, "model-controlled prose says invalid_schema")
			steps := failureResponseSteps(response)
			if reason != RepairFailureInvalidSchema {
				steps = append(steps, failureResponseSteps(submissionFailureResponse(RepairFailureInvalidSchema, "later formatting error"))...)
			}
			got, replay := classifyRepairFailure(steps, nil)
			if got != reason || replay || !got.IsProtocolRepairFailure() {
				t.Fatalf("reason=%s replay=%t", got, replay)
			}
		})
	}
}

func TestSubmissionGateClassifiesActualRejections(t *testing.T) {
	for _, mode := range []string{"schema", "receipt", "commit"} {
		t.Run(mode, func(t *testing.T) {
			c, item, ctx, _ := resultCandidateFixture(t)
			input := candidateMisplaced
			want := RepairFailureInvalidSchema
			if mode == "commit" {
				c.SetEventJournal(failingJournal{err: errors.New("storage unavailable")})
				want = RepairFailureCommit
			}
			if mode == "receipt" {
				// A legacy contract allows receipt IDs; well-formed claims still
				// cannot invent an execution receipt.
				c, item, ctx, _ = resultCandidateFixture(t)
				compiled := c.session.ResultContracts[item.ResultContract.ID]
				compiled.submissionFields = nil
				input = `{"status":"success","summary":"audit","receipt_ids":["fabricated"],"structured_payload":{"summary":"audit","findings":[{"claim":"fact","verdict":"unverified"}]}}`
				want = RepairFailureSource
			}
			response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
			if err != nil || !response.IsError {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if got, _ := classifyRepairFailure(failureResponseSteps(response), nil); got != want {
				t.Fatalf("reason=%s want=%s: %s", got, want, response.Content)
			}
		})
	}
}

func TestNonFormatSubmissionDoesNotStartAnotherRepair(t *testing.T) {
	c, _ := resumeEvidenceFixture(t, true)
	worker := scriptWorker(failTurn(errors.New("worker forbidden")))
	c.workerAgentOverride = worker
	repair := scriptWorker(invalidReceiptSubmissionTurn(t, c, "6"))
	c.repairAgentOverride = repair
	ctx := WithResultOnlyRepair(withTestProtocolRepairInvocationContext(t.Context()), "6")
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	item := todoByID(t, c, "6")
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(item), item); err == nil {
		t.Fatal("invalid evidence accepted")
	}
	after := todoByID(t, c, "6")
	if worker.Calls() != 0 || repair.Calls() != 1 || after.Status != TaskBlocked || after.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureSource {
		t.Fatalf("worker=%d repair=%d status=%s receipt=%+v", worker.Calls(), repair.Calls(), after.Status, after.ExecutionReceipt.RepairProvenance)
	}
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(after), after); err == nil || repair.Calls() != 1 {
		t.Fatal("nonformat failure started a later format turn")
	}
}

func invalidReceiptSubmissionTurn(t *testing.T, c *Coordinator, id string) scriptedTurn {
	t.Helper()
	return scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		raw, err := json.Marshal(map[string]any{"status": "success", "summary": "audit", "receipt_ids": []string{"fabricated"}, "structured_payload": jsontext.Value(evidenceTestPayload)})
		if err != nil {
			t.Fatal(err)
		}
		input := submitResultCall("submit", string(raw))
		if err := call.OnToolCall(input); err != nil {
			return nil, err
		}
		response, err := (&submitResultTool{coordinator: c, todoID: id}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input.Input})
		if err != nil || !response.IsError {
			t.Fatalf("response=%+v err=%v", response, err)
		}
		result := submitResultOutcome("submit", response.Content)
		result.ClientMetadata = response.Metadata
		if err := call.OnToolResult(result); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{input, result}}}}}, nil
	}}
}

func TestWorkerNonFormatSubmissionDoesNotStartRepair(t *testing.T) {
	c, id, task := toolEvidenceRepairFixture(t)
	worker := scriptWorker(invalidReceiptSubmissionTurn(t, c, id))
	repair := scriptWorker(failTurn(errors.New("model repair forbidden")))
	c.workerAgentOverride, c.repairAgentOverride = worker, repair
	if _, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), task, id); err == nil {
		t.Fatal("invalid receipt accepted")
	}
	if worker.Calls() != 1 || repair.Calls() != 0 || todoByID(t, c, id).Status != TaskBlocked {
		t.Fatalf("worker=%d repair=%d task=%s", worker.Calls(), repair.Calls(), todoByID(t, c, id).Status)
	}
}
