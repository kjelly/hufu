package team

import (
	"context"
	"testing"
)

func TestIsInterruptedStatus(t *testing.T) {
	interrupted := []TaskStatus{TaskInProgress, TaskVerifying, TaskPaused, TaskPlanned, TaskPending}
	for _, s := range interrupted {
		if !isInterruptedStatus(s) {
			t.Errorf("%q should be treated as interrupted", s)
		}
	}
	terminal := []TaskStatus{TaskDone, TaskSkipped, TaskError, TaskBlocked}
	for _, s := range terminal {
		if isInterruptedStatus(s) {
			t.Errorf("%q should NOT be treated as interrupted", s)
		}
	}
}

func TestTodoIDLess(t *testing.T) {
	if !todoIDLess("2", "10") {
		t.Error("numeric IDs must order numerically: 2 < 10")
	}
	if todoIDLess("10", "2") {
		t.Error("10 should not be < 2")
	}
	if !todoIDLess("a", "b") {
		t.Error("non-numeric IDs fall back to string order")
	}
}

func TestResetInterruptedTasks_SelectionAndReset(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", Agent: "a", Desc: "done task", Status: TaskDone, Output: "out"},
		{ID: "2", Agent: "a", Desc: "in-flight task", Status: TaskInProgress},
		{ID: "2v", Agent: "a", Desc: "verifying task", Status: TaskVerifying},
		{ID: "3", Agent: "b", Desc: "never started", Status: TaskPending},
		{ID: "4", Agent: "b", Desc: "failed task", Status: TaskError},
		{ID: "5", Agent: "c", Desc: "planned task", Status: TaskPlanned},
		{ID: "6", Agent: "c", Desc: "skipped task", Status: TaskSkipped},
	})

	got := c.resetInterruptedTasks()

	// Only in-flight / verifying / pending / planned are selected (2,2v,3,5); done/error/skipped excluded.
	wantIDs := map[string]bool{"2": true, "2v": true, "3": true, "5": true}
	if len(got) != len(wantIDs) {
		t.Fatalf("expected %d interrupted tasks, got %d", len(wantIDs), len(got))
	}
	for _, it := range got {
		if !wantIDs[it.ID] {
			t.Errorf("unexpected task selected for resume: %s (%s)", it.ID, it.Desc)
		}
	}

	// Ascending-ID order (deps run first).
	if got[0].ID != "2" || got[1].ID != "2v" || got[2].ID != "3" || got[3].ID != "5" {
		t.Errorf("interrupted tasks not in ascending ID order: %s,%s,%s,%s", got[0].ID, got[1].ID, got[2].ID, got[3].ID)
	}

	// Selected tasks are reset to pending; terminal tasks untouched.
	byID := map[string]*TodoItem{}
	for _, it := range c.taskTracker.TodoList().Items() {
		byID[it.ID] = it
	}
	for id := range wantIDs {
		if byID[id].Status != TaskPending {
			t.Errorf("task %s should be reset to pending, got %s", id, byID[id].Status)
		}
	}
	if byID["1"].Status != TaskDone {
		t.Errorf("done task must remain done, got %s", byID["1"].Status)
	}
	if byID["4"].Status != TaskError {
		t.Errorf("error task must remain error, got %s", byID["4"].Status)
	}
	if byID["6"].Status != TaskSkipped {
		t.Errorf("skipped task must remain skipped, got %s", byID["6"].Status)
	}
}

func TestResumeInterruptedTasks_NoopWhenNothingInterrupted(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", Agent: "a", Desc: "done", Status: TaskDone, Output: "x"},
		{ID: "2", Agent: "a", Desc: "skipped", Status: TaskSkipped},
	})
	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-driven, got %d", n)
	}
}

func TestResumeInterruptedTasks_EmptyTodoList(t *testing.T) {
	c := newBudgetCoordinator(t)
	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil || n != 0 {
		t.Errorf("fresh run should be a no-op, got n=%d err=%v", n, err)
	}
}
