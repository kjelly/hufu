package team

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestResultCollectionResumeCompletesDurableClaimsWithoutWorkerOrProvider(t *testing.T) {
	for _, state := range []ResultCollectionState{ResultCollectionReceived, ResultCollectionPrepared, ResultCollectionCommitted} {
		t.Run(string(state), func(t *testing.T) {
			c, item, ctx, checkpoint := beginCollectionFixture(t)
			worker := scriptWorker(textTurn("must never execute"))
			c.workerAgentOverride = worker
			var err error
			checkpoint, err = c.reserveResultCollection(ctx, checkpoint, "collector-1", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"analysis complete","files_read":["source.go"],"files_modified":["result.go"]}`))
			if err != nil {
				t.Fatal(err)
			}
			if state != ResultCollectionReceived {
				checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
				if err != nil {
					t.Fatal(err)
				}
			}
			if state == ResultCollectionCommitted {
				if err := c.commitPreparedCollection(ctx, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			events, err := c.EventJournal().ReadEvents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			c.revokeTaskOccurrence(item.ID)
			c.taskTracker = NewTaskTracker()
			c.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
			restored := c.todoItemByID(item.ID)
			output, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(restored), restored.ID)
			if err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			if current.Status != TaskDone || current.TypedResult == nil || current.TypedResult.Source != "collected" || output == "" {
				t.Fatalf("completion=%+v output=%q", current, output)
			}
			if worker.Calls() != 0 || current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 {
				t.Fatal("resume repeated worker execution or spent another model turn")
			}
			if len(current.TypedResult.FilesRead) != 1 || current.TypedResult.FilesRead[0].Path != "source.go" || len(current.TypedResult.FilesModified) != 1 || current.TypedResult.FilesModified[0].Path != "result.go" {
				t.Fatal("resume lost canonical file entries")
			}
		})
	}
}

func TestResultCollectionBlockedResumeCannotReplayWork(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	worker := scriptWorker(textTurn("must never execute"))
	c.workerAgentOverride = worker
	if _, err := c.blockResultCollection(ctx, checkpoint, RepairFailureSource); err != nil {
		t.Fatal(err)
	}
	_, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(item), item.ID)
	if err == nil || worker.Calls() != 0 || c.todoItemByID(item.ID).Status != TaskBlocked {
		t.Fatalf("blocked collection was dispatched: calls=%d err=%v", worker.Calls(), err)
	}
}

func TestResultCollectionFanoutResumesPreparedLeafWithoutWorker(t *testing.T) {
	parent, item, _, receipt := resultCollectionFixture(t)
	leafWorkspace, err := os.MkdirTemp(filepath.Join(parent.session.Workspace), "leaf-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leafWorkspace, "delivery.txt"), []byte("durable leaf delivery"), 0o600); err != nil {
		t.Fatal(err)
	}
	leaf := cloneCoordinator(parent, cloneSession(parent.session, leafWorkspace))
	leaf.modelFanout = &modelFanoutBinding{parent: parent, taskID: item.ID, index: 0, target: receipt.ExecutionTarget}
	leaf.taskTracker = NewTaskTracker()
	leaf.taskTracker.TodoList().AddReserved([]*TodoItem{cloneTodoItem(item)})
	leafCtx := occurrenceTestContext(leaf, item.ID, receipt.Attempt)
	checkpoint, err := leaf.beginResultCollection(leafCtx, item.ID, receipt, "complete work report")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = leaf.reserveResultCollection(leafCtx, checkpoint, "leaf-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = leaf.recordCollectionResponse(leafCtx, checkpoint, []byte(`{"status":"success","summary":"analysis complete","artifacts":[{"path":"delivery.txt"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.prepareCollectionClaims(leafCtx, checkpoint); err != nil {
		t.Fatal(err)
	}
	events, err := parent.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parent.revokeTaskOccurrence(item.ID)
	parent.taskTracker = NewTaskTracker()
	parent.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
	task := taskDefFromTodoItem(parent.todoItemByID(item.ID))
	def := cloneAgentDef(parent.session.Agents["worker"])
	output, err := parent.executeSingleAgentWithModel(WithSessionResume(t.Context()), "worker", def, task, item.ID, "main", 0)
	if err != nil {
		t.Fatal(err)
	}
	current := parent.todoItemByID(item.ID)
	if output == "" || current.Status != TaskInProgress || current.TypedResult != nil {
		t.Fatalf("leaf completed parent: status=%s output=%q", current.Status, output)
	}
	if current.ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted || current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 {
		t.Fatal("leaf recovery spent another model turn")
	}
	accepted, err := parent.acceptedCollectionResult(t.Context(), current, current.ExecutionReceipt.ResultCollection)
	if err != nil || len(accepted.Artifacts) != 1 {
		t.Fatalf("leaf artifact admission: %v", err)
	}
	if err := os.RemoveAll(leafWorkspace); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileArtifactStoreReadOnly(parent.artifactStoreRootPath(), parent.artifactStoreRootPath())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.Open(t.Context(), accepted.Artifacts[0].ID)
	if err != nil {
		t.Fatalf("accepted leaf artifact depends on disposable workspace: %v", err)
	}
	body, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || string(body) != "durable leaf delivery" {
		t.Fatalf("durable delivery=%q read=%v close=%v", body, err, closeErr)
	}

}

func TestResultCollectionGrantCanContinueEarlierDurableProgress(t *testing.T) {
	for _, reason := range []RepairFailureReason{repairFailureCollectionUnavailable, RepairFailureCommit, RepairFailureSource} {
		t.Run(string(reason), func(t *testing.T) {
			c, item, ctx, checkpoint := beginCollectionFixture(t)
			c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: reason, Checkpoint: checkpoint, Cause: errors.New("reporting interrupted")})
			c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "reporting interrupted", NeedsHuman, FailureProtocol, TaskBlocked, "complete work report")
			c.executionRunID = "resumed-reporting"
			ctx = WithResultOnlyRepair(ctx, item.ID)
			err := c.authorizeOperatorResultRepair(ctx)
			if reason == RepairFailureSource {
				if err == nil {
					t.Fatal("operator grant bypassed a source rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			granted := current.ExecutionReceipt.ResultCollection
			if granted.Work != checkpoint.Work || granted.TurnsUsed != 0 || granted.TurnLimit != 2 {
				t.Fatalf("earlier progress lost or reset: %+v", granted)
			}
			provenance := current.ExecutionReceipt.RepairProvenance
			if reason == RepairFailureCommit && provenance.OperatorGrants[0].AdditionalAttempts != 0 {
				t.Fatal("storage continuation added model budget")
			}
			if _, err := c.restoreCollectionContext(ctx, granted); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResultCollectionCommittedGrantFinalizesWithoutReadmission(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	checkpoint, err := c.reserveResultCollection(ctx, checkpoint, "collector-before-commit", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"analysis complete"}`))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = c.prepareCollectionClaims(ctx, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.commitPreparedCollection(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureCommit, Cause: errors.New("projection interrupted after commit")})
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "projection interrupted", NeedsHuman, FailureProtocol, TaskBlocked, "complete work report")
	c.executionRunID = "finalize-accepted-result"
	ctx = WithResultOnlyRepair(t.Context(), item.ID)
	if err := c.authorizeOperatorResultRepair(ctx); err != nil {
		t.Fatal(err)
	}
	current := c.todoItemByID(item.ID)
	if current.ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted {
		t.Fatal("grant demoted accepted bytes to a fresh submission")
	}
	if _, err := c.executeTask(WithSessionResume(ctx), taskDefFromTodoItem(current), item.ID); err != nil {
		t.Fatal(err)
	}
	current = c.todoItemByID(item.ID)
	if current.Status != TaskDone || current.ExecutionReceipt.ResultCollection.TurnsUsed != 1 {
		t.Fatal("accepted result was not finalized locally")
	}
}

func TestCurrentResultCollectionMatchesLifecycleOccurrence(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		retries, work, occurrence int
		current                   bool
	}{
		{"initial", 0, 1, 1, true}, {"execution retry", 1, 1, 1, false}, {"new dispatch", 1, 1, 2, true},
		{"in-dispatch retry", 0, 2, 1, true}, {"legacy current", 0, 1, 0, true}, {"legacy unknown", 1, 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := &TodoItem{Retries: tc.retries, ExecutionReceipt: &ExecutionReceipt{Attempt: tc.work, OccurrenceAttempt: tc.occurrence, ResultCollection: &ResultCollectionCheckpoint{Attempt: tc.work}}}
			if (currentResultCollection(item) != nil) != tc.current {
				t.Fatal("collection selected across lifecycle occurrences")
			}
		})
	}
}

func TestResultCollectionDAGRetryExecutesFreshWork(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprint(replay), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","capabilities":["completion","tools"]}`)
					return
				}
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "data: {\"id\":\"fresh\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"{\\\"status\\\":\\\"success\\\",\\\"summary\\\":\\\"corrected work\\\"}\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			c, item, ctx, receipt := resultCollectionTaskFixture(t, server.URL+"/v1", true, func(spec *TodoSpec) {
				spec.SideEffect = SideEffectNone
				spec.Recovery = RecoveryRetry
				spec.VerifySpec = &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{{Pointer: "/summary", Op: "non_empty"}}}
			})
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original work")
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.reserveResultCollection(ctx, cp, "first-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"original work"}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.prepareCollectionClaims(ctx, cp)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.commitPreparedCollection(ctx, cp); err != nil {
				t.Fatal(err)
			}
			if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(item), item.ID); err != nil {
				t.Fatal(err)
			}
			task := taskDefFromTodoItem(item)
			scheduler := mustNewDAGScheduler(t, c, []TaskDef{task}, []*TodoItem{item}, nil)
			scheduler.states[0] = TaskDone
			if err := scheduler.resetTaskWithRemediation(t.Context(), 0, "verifier requires corrected work", &RemediationContext{FailureClass: FailureVerify, Summary: "correct the report"}); err != nil {
				t.Fatal(err)
			}
			item = c.todoItemByID(item.ID)
			if item.Retries != 1 || currentResultCollection(item) != nil || item.ExecutionReceipt.ResultCollection.State != ResultCollectionCommitted {
				t.Fatal("reset reused or erased historical collection")
			}
			if replay {
				events, err := c.EventJournal().ReadEvents(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				c.taskTracker = NewTaskTracker()
				c.taskTracker.TodoList().AddReserved(ReduceToTodoList(events))
				item = c.todoItemByID(item.ID)
			}
			worker := scriptWorker(textTurn("corrected work after verifier feedback"))
			c.workerAgentOverride = worker
			c.providerBoundaryStart = func(context.Context, string) error { return nil }
			c.delegatedTasks = make(map[string]int)
			testTaskCache(c).entries = make(map[string][]cachedTaskEntry)
			if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
				t.Fatal(err)
			}
			current := c.todoItemByID(item.ID)
			fresh := currentResultCollection(current)
			if worker.Calls() != 1 || calls.Load() != 1 || current.Status != TaskDone || fresh == nil || fresh.Work == cp.Work || current.ExecutionReceipt.OccurrenceAttempt != 2 || current.TypedResult.Summary != "corrected work" {
				t.Fatalf("work=%d collection=%d status=%s", worker.Calls(), calls.Load(), current.Status)
			}
		})
	}
}
