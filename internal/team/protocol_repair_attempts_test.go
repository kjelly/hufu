package team

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func rejectedEvidenceRepairTurn(t *testing.T, c *Coordinator, id string) scriptedTurn {
	t.Helper()
	return scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		input := `{"status":"success","summary":"existing evidence","risk":"undeclared","structured_payload":` + evidenceTestPayload + `}`
		toolCall := submitResultCall("rejected-repair", input)
		if err := call.OnToolCall(toolCall); err != nil {
			return nil, err
		}
		response, err := (&submitResultTool{coordinator: c, todoID: id}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
		if err != nil || !response.IsError {
			t.Fatalf("undeclared repair field was accepted: response=%+v err=%v", response, err)
		}
		result := fantasy.ToolResultContent{ToolCallID: toolCall.ToolCallID, ToolName: submitResultToolName,
			Result: fantasy.ToolResultOutputContentError{Error: errors.New(response.Content)}}
		if err := call.OnToolResult(result); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{Content: fantasy.ResponseContent{toolCall, result}}}}}, nil
	}}
}

func TestResultRepairKeepsOneSchemaCorrectionAfterMissingCall(t *testing.T) {
	for _, mode := range []string{"live", "preparation", "step_budget", "resume", "resume_first_rejected", "resume_rejected", "two_invalid", "two_missing", "correction_failed", "exhausted_resume"} {
		t.Run(mode, func(t *testing.T) {
			c, id, task := toolEvidenceRepairFixture(t)
			worker := scriptWorker(observedDocumentTurn(t))
			c.workerAgentOverride = worker
			rejected := rejectedEvidenceRepairTurn(t, c, id)
			accepted := submitEvidenceRepairTurn(t, c, id)
			turns := []scriptedTurn{textTurn("ready to submit"), rejected, accepted}
			wantCalls, wantErr := 3, false
			wantAttempts := 3
			var original []byte
			var item *TodoItem
			resumed := mode == "resume" || mode == "resume_first_rejected" || mode == "resume_rejected" || mode == "exhausted_resume"
			if resumed {
				item = interruptedToolEvidenceFixture(t, c, id, task, 2)
				original = readStoredArtifact(t, c.session.Workspace, item.ExecutionReceipt.TranscriptRef)
				if mode != "resume" {
					history := []RepairAttemptProvenance{{Attempt: 1, FailureReason: RepairFailureNoToolCall},
						{Attempt: 2, FailureReason: RepairFailureInvalidSchema, Error: "$.risk: undeclared property"}}
					if mode == "resume_first_rejected" {
						history = []RepairAttemptProvenance{{Attempt: 1, FailureReason: RepairFailureInvalidSchema, Error: "$.risk: undeclared property"}}
						wantAttempts = 2
					}
					if mode == "exhausted_resume" {
						history = append(history, RepairAttemptProvenance{Attempt: 3, FailureReason: RepairFailureInvalidSchema})
						wantCalls, wantErr = 0, true
					} else {
						turns, wantCalls = []scriptedTurn{accepted}, 1
					}
					receipt := cloneExecutionReceipt(item.ExecutionReceipt)
					receipt.RepairProvenance = &RepairProvenance{Attempted: true, RepairAttempts: len(history), History: history, FailureReason: RepairFailureInvalidSchema}
					if err := c.setAttemptReceipt(id, &receipt); err != nil {
						t.Fatal(err)
					}
					item = todoByID(t, c, id)
				}
				c.SetSessionData(&SessionData{Tasks: c.taskTracker.TodoList().Items()})
				store, err := NewEventStore(c.session.Workspace, c.executionRunID, "repair-test")
				if err != nil {
					t.Fatal(err)
				}
				c.eventStore = store
				c.SetEventJournal(eventStoreJournal{store: store})
				if mode == "resume" {
					third := accepted
					turns[2] = scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
						loaded, exists, err := LoadSessionReadOnly(c.session.Workspace)
						if err != nil || !exists || loaded == nil || len(loaded.Tasks) != 1 {
							t.Fatalf("prior repair not checkpointed: exists=%v err=%v", exists, err)
						}
						history := loaded.Tasks[0].ExecutionReceipt.RepairProvenance.History
						if len(history) != 2 || history[1].FailureReason != RepairFailureInvalidSchema {
							t.Fatal("next generation started before prior rejection was durable")
						}
						events, err := store.ReadEvents()
						if err != nil {
							t.Fatal(err)
						}
						projected := ReduceToTodoList(events)
						if len(projected) != 1 || len(projected[0].ExecutionReceipt.RepairProvenance.History) != 2 {
							t.Fatal("canonical replay lost the repair budget")
						}
						return third.stream(ctx, call)
					}}
				}
			}
			if mode == "step_budget" {
				c.session.Agents["worker"].MaxSteps = 1
			}
			switch mode {
			case "preparation":
				turns[0] = scriptedTurn{err: errors.New("repair stream unavailable")}
			case "two_invalid":
				turns, wantCalls, wantErr = []scriptedTurn{rejected, rejected}, 2, true
			case "two_missing":
				turns, wantCalls, wantErr = []scriptedTurn{textTurn("ready")}, 2, true
			case "correction_failed":
				turns, wantCalls, wantErr = []scriptedTurn{textTurn("ready"), rejected, rejected}, 3, true
			}
			repair := scriptWorker(turns...)
			c.repairAgentOverride = repair
			ctx := withTestProtocolRepairInvocationContext(t.Context())
			var err error
			if resumed {
				_, err = c.resumeProtocolIncompleteTask(ctx, task, item)
			} else {
				_, err = c.executeTask(ctx, task, id)
			}
			if (err != nil) != wantErr || repair.Calls() != wantCalls {
				t.Fatalf("repair calls=%d error=%v, want calls=%d failure=%v", repair.Calls(), err, wantCalls, wantErr)
			}
			wantWorkers := 1
			if resumed {
				wantWorkers = 0
			}
			if worker.Calls() != wantWorkers {
				t.Fatal("schema correction replayed the worker")
			}
			got := todoByID(t, c, id)
			if !wantErr {
				if got.Status != TaskDone || got.ExecutionReceipt.RepairProvenance.RepairAttempts != wantAttempts || got.TypedResult.StructuredPayload.EvidenceDowngrades != 0 {
					t.Fatalf("schema correction lost evidence or its budget: %+v", got)
				}
				last := repair.Prompt(t, wantCalls)
				if !strings.Contains(last, "Schema-only repair") || !strings.Contains(last, "risk") || !strings.Contains(last, "the exact words in a document") {
					t.Fatal("schema correction lost its rejection or original observations")
				}
			}
			if resumed && !bytes.Equal(original, readStoredArtifact(t, c.session.Workspace, got.ExecutionReceipt.TranscriptRef)) {
				t.Fatal("result correction changed the original tool transcript")
			}
		})
	}
}
