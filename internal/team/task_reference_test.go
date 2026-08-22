package team

import (
	"strings"
	"testing"
)

func TestResolveTaskReferenceUsesLogicalPlanIDAfterCompletion(t *testing.T) {
	tracker := NewTaskTracker()
	producer := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "produce-workset", Agent: "producer", Desc: "produce"}})[0]
	producer.Status = TaskDone
	c := &Coordinator{taskTracker: tracker, taskResults: map[string]*TaskResult{producer.ID: {
		TaskID: producer.ID, Status: TaskResultStatusSuccess, Summary: "ready",
	}}}
	runtimeID, err := c.resolveTaskReference("produce-workset")
	if err != nil {
		t.Fatalf("resolve logical task ID: %v", err)
	}
	if runtimeID != producer.ID || c.GetTaskResult(runtimeID) == nil {
		t.Fatalf("resolved runtime ID = %q, producer runtime ID = %q", runtimeID, producer.ID)
	}
}

func TestResolveTaskReferenceRejectsAmbiguousLogicalID(t *testing.T) {
	tracker := NewTaskTracker()
	tracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "producer", Agent: "one", Desc: "one"},
		{PlanTaskID: "producer", Agent: "two", Desc: "two"},
	})
	c := &Coordinator{taskTracker: tracker}
	if _, err := c.resolveTaskReference("producer"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous logical ID error = %v", err)
	}
}
