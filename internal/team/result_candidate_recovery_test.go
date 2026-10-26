package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestCandidateRepairReusesAcceptedSourcesWithoutWorkerReplay(t *testing.T) {
	c, _ := resumeEvidenceFixture(t, true)
	worker := scriptWorker(failTurn(errors.New("worker replay forbidden")))
	c.workerAgentOverride = worker
	before := c.taskTracker.TodoList().Items()
	transcript := readStoredArtifact(t, c.session.Workspace, before[2].ExecutionReceipt.TranscriptRef)
	turn := func(patch bool) scriptedTurn {
		return scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
			if patch {
				for _, source := range before[:2] {
					if !strings.Contains(call.Prompt, source.TypedResult.StructuredPayload.SHA256) {
						t.Fatal("repair model did not receive the original accepted source binding")
					}
				}
				if !strings.Contains(call.Prompt, "the exact words in a document") || !strings.Contains(call.Prompt, "edits only") || strings.Contains(call.Prompt, "Call submit_result with the complete argument object") {
					t.Fatal("repair model lost original observations or received conflicting submission instructions")
				}
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(evidenceTestPayload), &payload); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{"status": "success", "summary": "audit", "findings": payload["findings"], "structured_payload": map[string]any{}})
			if err != nil {
				t.Fatal(err)
			}
			tool := &submitResultTool{coordinator: c, todoID: "6"}
			if patch {
				tool.candidate = cloneResultCandidate(todoByID(t, c, "6").ResultCandidate)
				raw = []byte(candidateMove)
			}
			input := submitResultCall("submit", string(raw))
			if err := call.OnToolCall(input); err != nil {
				return nil, err
			}
			response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: string(raw)})
			if err != nil || response.IsError == patch {
				t.Fatalf("patch=%t response=%+v err=%v", patch, response, err)
			}
			rejection := ""
			if response.IsError {
				rejection = response.Content
			}
			result := submitResultOutcome(input.ToolCallID, rejection)
			if err := call.OnToolResult(result); err != nil {
				return nil, err
			}
			return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{input, result}}}}}, nil
		}}
	}
	repair := scriptWorker(turn(false), turn(true))
	c.repairAgentOverride = repair
	ctx := WithResultOnlyRepair(withTestProtocolRepairInvocationContext(t.Context()), "6")
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	item := todoByID(t, c, "6")
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(item), item); err != nil {
		t.Fatal(err)
	}
	if worker.Calls() != 0 || repair.Calls() != 2 {
		t.Fatalf("worker=%d repair=%d", worker.Calls(), repair.Calls())
	}
	for i, id := range []string{"4", "5"} {
		if !sameCanonicalEvidence(before[i], todoByID(t, c, id)) {
			t.Fatalf("source %s changed", id)
		}
	}
	after := todoByID(t, c, "6")
	if after.Status != TaskDone || after.ExecutionReceipt.RunID != before[2].ExecutionReceipt.RunID || !bytes.Equal(transcript, readStoredArtifact(t, c.session.Workspace, after.ExecutionReceipt.TranscriptRef)) {
		t.Fatal("repair lost original execution evidence")
	}
	if after.TypedResult.StructuredPayload.EvidenceDowngrades != 0 {
		t.Fatal("original tool evidence was unavailable during patch validation")
	}
	if after.ResultCandidate == nil {
		t.Fatal("original rejected candidate was lost")
	}
}

func TestCandidateResetDoesNotReusePriorAttempt(t *testing.T) {
	c, item, ctx, journal := resultCandidateFixture(t)
	rejectCandidate(t, c, item, ctx, candidateMisplaced)
	if err := c.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskError, "failed", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskResetForRetry(ctx, item.ID, "new attempt"); err != nil {
		t.Fatal(err)
	}
	if c.todoItemByID(item.ID).ResultCandidate != nil {
		t.Fatal("new worker attempt inherited prior candidate")
	}
	replayed := ReduceToTodoList(journal.events)
	if len(replayed) != 1 || replayed[0].ResultCandidate != nil {
		t.Fatal("replay resurrected prior candidate")
	}
}
