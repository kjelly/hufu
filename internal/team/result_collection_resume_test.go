package team

import (
	"errors"
	"io"
	"os"
	"path/filepath"
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
			checkpoint, err = c.recordCollectionResponse(ctx, checkpoint, []byte(`{"status":"success","summary":"analysis complete"}`))
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
