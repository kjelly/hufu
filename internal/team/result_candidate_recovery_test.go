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

type interruptedCandidateCommitSink struct{}

func (interruptedCandidateCommitSink) Submit(context.Context, string, TaskResult) error {
	return errors.New("injected interruption before result commit")
}

func preparedCandidateFixture(t *testing.T) (*Coordinator, *TodoItem) {
	t.Helper()
	c, _ := resumeEvidenceFixture(t, true)
	ctx := WithResultOnlyRepair(withTestProtocolRepairInvocationContext(t.Context()), "6")
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	item := todoByID(t, c, "6")
	ctx = occurrenceTestContext(c, item.ID, item.ExecutionReceipt.Attempt)
	evidence, err := c.restoreProtocolRepairToolEvidence(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	ctx = withProtocolRepairToolEvidence(ctx, evidence)
	ctx = context.WithValue(ctx, protocolRepairExecutionKey{}, true)
	var payload map[string]any
	if err := json.Unmarshal([]byte(evidenceTestPayload), &payload); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"status": "success", "summary": "audit", "findings": payload["findings"], "structured_payload": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := rejectCandidate(t, c, item, ctx, string(raw))
	tool := &submitResultTool{coordinator: c, todoID: item.ID, candidate: candidate, sink: interruptedCandidateCommitSink{}}
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: candidateMove})
	if err != nil || !response.IsError || c.GetTaskResult(item.ID) != nil {
		t.Fatalf("injected interruption: %+v %v", response, err)
	}
	if reason, _ := classifyRepairFailure(failureResponseSteps(response), nil); reason != RepairFailureCommit {
		t.Fatalf("interruption classified %s: %s", reason, response.Content)
	}
	item = todoByID(t, c, item.ID)
	if item.ResultCandidate == nil || !item.ResultCandidate.Prepared || item.ResultCandidate.ParentSHA256 != candidate.SHA256 {
		t.Fatal("complete patched submission was not durable before commit")
	}
	return c, item
}

func TestPreparedCandidateResumesWithoutGeneration(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit"}[committed], func(t *testing.T) {
			c, item := preparedCandidateFixture(t)
			before := c.taskTracker.TodoList().Items()
			if committed {
				response, err := c.commitPreparedResultCandidate(t.Context(), item)
				if err != nil || response.IsError {
					t.Fatalf("commit: %+v %v", response, err)
				}
			}
			events, err := c.eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			items, err := ReplayTodoList(events)
			if err != nil {
				t.Fatal(err)
			}
			// Fresh coordinator, only canonical events and CAS survive the crash.
			next := newTestCoordinator(t)
			next.session.Workspace = c.session.Workspace
			next.session.Agents, next.session.ResultContracts = c.session.Agents, c.session.ResultContracts
			next.executionRunID = c.executionRunID
			session := NewSession()
			session.Tasks = items
			session.RunInputSnapshots = []RunInputSnapshot{*c.RunInputSnapshot()}
			session.ActiveRunInputSnapshotID = c.RunInputSnapshot().ID
			next.SetSessionData(session)
			if err := next.selectRecoveryRunInputSnapshot(c.RunInputSnapshot()); err != nil {
				t.Fatal(err)
			}
			next.eventStore = c.eventStore
			next.SetEventJournal(eventStoreJournal{store: c.eventStore})
			worker, repair := scriptWorker(failTurn(errors.New("worker forbidden"))), scriptWorker(failTurn(errors.New("model forbidden")))
			next.workerAgentOverride, next.repairAgentOverride = worker, repair
			item = todoByID(t, next, "6")
			if _, err := next.resumeProtocolIncompleteTask(t.Context(), taskDefFromTodoItem(item), item); err != nil {
				t.Fatal(err)
			}
			after := todoByID(t, next, "6")
			if after.Status != TaskDone || worker.Calls() != 0 || repair.Calls() != 0 {
				t.Fatalf("status=%s worker=%d repair=%d", after.Status, worker.Calls(), repair.Calls())
			}
			for i, id := range []string{"4", "5"} {
				if !sameCanonicalEvidence(before[i], todoByID(t, next, id)) {
					t.Fatalf("source %s changed", id)
				}
			}
			if after.ExecutionReceipt.RepairProvenance.RepairAttempts != before[2].ExecutionReceipt.RepairProvenance.RepairAttempts || after.ExecutionReceipt.RepairProvenance.PreparedCandidateSHA256 != item.ResultCandidate.SHA256 || after.ExecutionReceipt.RunID != before[2].ExecutionReceipt.RunID || after.ExecutionReceipt.TranscriptRef != before[2].ExecutionReceipt.TranscriptRef || after.TypedResult.StructuredPayload.EvidenceDowngrades != 0 {
				t.Fatal("local completion changed repair budget or lost original evidence")
			}
			hash := after.TypedResult.StructuredPayload.SHA256
			if _, err := next.resumeProtocolIncompleteTask(t.Context(), taskDefFromTodoItem(after), after); err != nil {
				t.Fatal(err)
			}
			if todoByID(t, next, "6").TypedResult.StructuredPayload.SHA256 != hash || repair.Calls() != 0 {
				t.Fatal("repeated completion changed the accepted result")
			}
		})
	}
}

func TestPreparedCandidateFailsClosed(t *testing.T) {
	for _, mode := range []string{"branch", "source", "transcript", "policy"} {
		t.Run(mode, func(t *testing.T) {
			c, item := preparedCandidateFixture(t)
			worker, repair := scriptWorker(failTurn(errors.New("worker forbidden"))), scriptWorker(failTurn(errors.New("model forbidden")))
			c.workerAgentOverride, c.repairAgentOverride = worker, repair
			switch mode {
			case "branch":
				item.ResultCandidate.BranchID = "wrong-branch"
			case "source":
				source := todoByID(t, c, "4")
				source.TypedResult.StructuredPayload.SHA256 = "changed"
				if err := c.taskTracker.TodoList().TryApplyProjectedItem(source); err != nil {
					t.Fatal(err)
				}
			case "transcript":
				store := mustArtifactStore(t, c.session.Workspace)
				if err := os.Remove(filepath.Join(store.root, "data", item.ExecutionReceipt.TranscriptRef)); err != nil {
					t.Fatal(err)
				}
			case "policy":
				c.session.Config.ToolsDenied = append(c.session.Config.ToolsDenied, submitResultToolName)
			}
			if _, err := c.resumeProtocolIncompleteTask(t.Context(), taskDefFromTodoItem(item), item); err == nil {
				t.Fatal("invalid candidate completed")
			}
			if c.GetTaskResult("6") != nil || worker.Calls() != 0 || repair.Calls() != 0 {
				t.Fatal("invalid candidate started generation or accepted a result")
			}
		})
	}
}

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

func TestPreparedCandidateWithProvisionalResultFinishesLocally(t *testing.T) {
	c, item := preparedCandidateFixture(t)
	item.TypedResult = &TaskResult{Status: TaskResultStatusPartial, Source: "free_text", Summary: "unaccepted progress"}
	if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
		t.Fatal(err)
	}
	worker, repair := scriptWorker(failTurn(errors.New("worker forbidden"))), scriptWorker(failTurn(errors.New("model forbidden")))
	c.workerAgentOverride, c.repairAgentOverride = worker, repair
	if _, err := c.resumeProtocolIncompleteTask(t.Context(), taskDefFromTodoItem(item), item); err != nil {
		t.Fatal(err)
	}
	if todoByID(t, c, "6").Status != TaskDone || worker.Calls() != 0 || repair.Calls() != 0 {
		t.Fatal("provisional output hid the prepared result")
	}
}

func TestPreparedCandidateOperatorContinuationGrantsNoModelTurns(t *testing.T) {
	c, item := preparedCandidateFixture(t)
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	receipt.RepairProvenance.FailureReason = RepairFailureCommit
	if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
		t.Fatal(err)
	}
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "storage failed", NeedsHuman, FailureProtocol, TaskBlocked, item.Output)
	c.executionRunID = "local-commit-invocation"
	before := todoByID(t, c, item.ID)
	ctx := WithResultOnlyRepair(t.Context(), item.ID)
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	authorized := todoByID(t, c, item.ID)
	grants := authorized.ExecutionReceipt.RepairProvenance.OperatorGrants
	if grants[len(grants)-1].AdditionalAttempts != 0 || taskProtocolRepairAttemptLimit(authorized) != taskProtocolRepairAttemptLimit(before) {
		t.Fatal("local completion extended the model budget")
	}
	worker, repair := scriptWorker(failTurn(errors.New("worker forbidden"))), scriptWorker(failTurn(errors.New("model forbidden")))
	c.workerAgentOverride, c.repairAgentOverride = worker, repair
	if _, err := c.resumeProtocolIncompleteTask(ctx, taskDefFromTodoItem(authorized), authorized); err != nil {
		t.Fatal(err)
	}
	if todoByID(t, c, item.ID).Status != TaskDone || repair.Calls() != 0 || worker.Calls() != 0 {
		t.Fatal("local continuation dispatched a model")
	}
}

func TestPreparedPartialCandidateCannotCompleteTask(t *testing.T) {
	c, item := preparedCandidateFixture(t)
	value, err := c.loadResultCandidate(t.Context(), item, item.ResultCandidate)
	if err != nil {
		t.Fatal(err)
	}
	value["status"] = "partial"
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx := occurrenceTestContext(c, item.ID, item.ExecutionReceipt.Attempt)
	if _, err := c.saveResultCandidate(ctx, item.ID, string(raw), nil, false, item.ResultCandidate); err != nil {
		t.Fatal(err)
	}
	item = todoByID(t, c, item.ID)
	repair := scriptWorker(failTurn(errors.New("model forbidden")))
	c.repairAgentOverride = repair
	if _, err := c.resumeProtocolIncompleteTask(t.Context(), taskDefFromTodoItem(item), item); err == nil {
		t.Fatal("partial candidate completed")
	}
	if todoByID(t, c, item.ID).Status != TaskBlocked || repair.Calls() != 0 {
		t.Fatal("partial candidate was promoted or regenerated")
	}
}

func TestPreparedCandidateInterruptedWorkerDoesNotReplay(t *testing.T) {
	c, item := preparedCandidateFixture(t)
	// Model a crash before the result-only lifecycle checkpoint was published.
	item.Status = TaskInProgress
	item.Retries = item.ExecutionReceipt.Attempt - 1
	if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
		t.Fatal(err)
	}
	worker := scriptWorker(failTurn(errors.New("worker replay forbidden")))
	repair := scriptWorker(failTurn(errors.New("repair generation forbidden")))
	c.workerAgentOverride, c.repairAgentOverride = worker, repair
	if err := c.migrateLegacyExecutionTarget(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	item = todoByID(t, c, item.ID)
	if _, err := c.admitTaskOccurrence(t.Context(), item, item.ID, item.Retries+1); err != nil {
		t.Fatal(err)
	}
	count, err := c.ResumeInterruptedTasks(withTestProtocolRepairInvocationContext(t.Context()))
	if err != nil || count != 1 {
		t.Fatalf("resume count=%d err=%v", count, err)
	}
	if todoByID(t, c, item.ID).Status != TaskDone || worker.Calls() != 0 || repair.Calls() != 0 {
		t.Fatal("interrupted submission was dispatched as new work")
	}
}
