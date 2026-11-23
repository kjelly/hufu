package team

import (
	"testing"
	"time"
)

func TestDurableReceiptClaimsRejectUnscopedMemoryEvidence(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	id := addTodo(t, c, "inspect")
	c.setCurrentTaskAttempt(id, 1)
	if err := c.executionStepReceiptRegistry().Record(ExecutionStepReceipt{ID: "unscoped", TaskID: id, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.validateTaskResultReceiptClaims(id, &TaskResult{Attempt: 1, ReceiptIDs: []string{"unscoped"}}); err != nil {
		t.Fatalf("standalone compatibility: %v", err)
	}
	c.initEventStore()
	if err := c.validateTaskResultReceiptClaims(id, &TaskResult{Attempt: 1, ReceiptIDs: []string{"unscoped"}}); err == nil {
		t.Fatal("durable run borrowed unscoped memory evidence")
	}
}

func TestOrdinaryToolReceiptRetainsRuntimeOccurrence(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	c.initEventStore()
	id := addTodo(t, c, "inspect")
	c.setCurrentTaskAttempt(id, 1)
	ctx := occurrenceTestContext(c, id, 1)
	if err := c.recordActualToolReceipt(ctx, id, 1, "actual-call", "view", "{}", "observed", false, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	receipt, ok := c.executionStepReceiptRegistry().Get("actual-call")
	if !ok || receipt.RunID != c.executionRunID || receipt.BranchID != c.EventStore().BranchID() || receipt.OccurrenceRevision != c.todoItemByID(id).OccurrenceRevision || receipt.ExecutionKind != "tool_call" {
		t.Fatalf("ordinary tool lost occurrence: %+v", receipt)
	}
}
