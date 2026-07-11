package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestTodoListAddBatch(t *testing.T) {
	tl := &TodoList{}

	items := tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
		{Agent: "writer", Desc: "write docs"},
		{Agent: "checker", Desc: "verify tests"},
	})

	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}

	if items[0].ID != "1" || items[1].ID != "2" || items[2].ID != "3" {
		t.Errorf("expected IDs 1,2,3, got %s,%s,%s", items[0].ID, items[1].ID, items[2].ID)
	}

	for _, item := range items {
		if item.Status != TaskPending {
			t.Errorf("expected status pending, got %s", item.Status)
		}
	}

	all := tl.Items()
	if len(all) != 3 {
		t.Fatalf("expected 3 total items, got %d", len(all))
	}
}

func TestTodoListUpdateStatus(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
		{Agent: "writer", Desc: "write docs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	all := tl.Items()

	if all[0].Status != TaskInProgress {
		t.Errorf("expected item 1 in_progress, got %s", all[0].Status)
	}
	if all[1].Status != TaskPending {
		t.Errorf("expected item 2 pending, got %s", all[1].Status)
	}

	tl.UpdateStatus("1", TaskDone, "")
	tl.UpdateStatus("2", TaskError, "failed badly")

	all = tl.Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 done, got %s", all[0].Status)
	}
	if all[1].Status != TaskError {
		t.Errorf("expected item 2 error, got %s", all[1].Status)
	}
	if all[1].Detail != "failed badly" {
		t.Errorf("expected detail 'failed badly', got %q", all[1].Detail)
	}
}

func TestTodoListClear(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "a", Desc: "task a"},
		{Agent: "b", Desc: "task b"},
	})

	if len(tl.Items()) != 2 {
		t.Fatalf("expected 2 items before clear, got %d", len(tl.Items()))
	}

	tl.Clear()

	if len(tl.Items()) != 0 {
		t.Errorf("expected 0 items after clear, got %d", len(tl.Items()))
	}

	items := tl.AddBatch([]TodoSpec{
		{Agent: "c", Desc: "task c"},
	})

	if items[0].ID != "1" {
		t.Errorf("expected ID to reset to 1 after clear, got %s", items[0].ID)
	}
}

func TestTodoListIDsAutoIncrement(t *testing.T) {
	tl := &TodoList{}

	batch1 := tl.AddBatch([]TodoSpec{
		{Agent: "a", Desc: "first"},
	})
	batch2 := tl.AddBatch([]TodoSpec{
		{Agent: "b", Desc: "second"},
		{Agent: "c", Desc: "third"},
	})

	if batch1[0].ID != "1" {
		t.Errorf("expected first batch item ID=1, got %s", batch1[0].ID)
	}
	if batch2[0].ID != "2" {
		t.Errorf("expected second batch item ID=2, got %s", batch2[0].ID)
	}
	if batch2[1].ID != "3" {
		t.Errorf("expected second batch item ID=3, got %s", batch2[1].ID)
	}
}

func TestTodoListDeleteIDs(t *testing.T) {
	tl := &TodoList{}

	items := tl.AddBatch([]TodoSpec{
		{Agent: "a", Desc: "first"},
		{Agent: "b", Desc: "second"},
		{Agent: "c", Desc: "third"},
	})

	tl.DeleteIDs(items[1].ID)

	all := tl.Items()
	if len(all) != 2 {
		t.Fatalf("expected 2 items after delete, got %d", len(all))
	}
	if all[0].ID != "1" || all[1].ID != "3" {
		t.Fatalf("unexpected remaining IDs: %s, %s", all[0].ID, all[1].ID)
	}
}

func TestTaskTrackerTodoList(t *testing.T) {
	tr := NewTaskTracker()

	if tr.TodoList() == nil {
		t.Fatal("expected TodoList to be non-nil")
	}

	tr.TodoList().AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	items := tr.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("expected 1 todo item, got %d", len(items))
	}
	if items[0].ID != "1" {
		t.Errorf("expected ID=1, got %s", items[0].ID)
	}
}

func TestTodoListUpdateStatusPreventDoneToInProgress(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	tl.UpdateStatus("1", TaskDone, "")

	all := tl.Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 done, got %s", all[0].Status)
	}

	tl.UpdateStatus("1", TaskInProgress, "")

	all = tl.Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 to remain done (not revert to in_progress), got %s", all[0].Status)
	}
}

func TestTodoListUpdateStatusAllowErrorToInProgressForRetry(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	tl.UpdateStatus("1", TaskError, "failed")

	all := tl.Items()
	if all[0].Status != TaskError {
		t.Errorf("expected item 1 error, got %s", all[0].Status)
	}

	// Retry is allowed: TaskError -> TaskInProgress
	tl.UpdateStatus("1", TaskInProgress, "retrying")

	all = tl.Items()
	if all[0].Status != TaskInProgress {
		t.Errorf("expected item 1 to be in_progress (retry allowed), got %s", all[0].Status)
	}
	if all[0].Detail != "retrying" {
		t.Errorf("expected detail 'retrying', got %q", all[0].Detail)
	}
}

func TestCanTransitionAndTryUpdateStatus(t *testing.T) {
	if !CanTransition(TaskInProgress, TaskVerifying) {
		t.Fatal("expected in_progress -> verifying to be allowed")
	}
	if !CanTransition(TaskError, TaskInProgress) {
		t.Fatal("expected error -> in_progress to be allowed")
	}
	if !CanTransition(TaskVerifying, TaskDone) {
		t.Fatal("expected verifying -> done to be allowed")
	}
	if CanTransition(TaskDone, TaskInProgress) {
		t.Fatal("expected done -> in_progress to be blocked")
	}

	tl := &TodoList{}
	tl.AddBatch([]TodoSpec{{Agent: "researcher", Desc: "find bugs"}})
	if err := tl.TryUpdateStatusAndOutput("1", TaskVerifying, "verifying", ""); err == nil {
		t.Fatal("expected invalid transition from pending to verifying to fail")
	}
	if err := tl.TryUpdateStatusAndOutput("1", TaskInProgress, "start", ""); err != nil {
		t.Fatalf("expected pending -> in_progress to succeed, got %v", err)
	}
	if err := tl.TryUpdateStatusAndOutput("1", TaskVerifying, "verify", ""); err != nil {
		t.Fatalf("expected in_progress -> verifying to succeed, got %v", err)
	}
	if err := tl.TryUpdateStatusAndOutput("1", TaskDone, "done", "out"); err != nil {
		t.Fatalf("expected verifying -> done to succeed, got %v", err)
	}
}

func TestTodoToolHandleUpdatePreventDoneToInProgress(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	c.taskTracker.TodoList().UpdateStatus("1", TaskInProgress, "")
	c.taskTracker.TodoList().UpdateStatus("1", TaskDone, "")

	tool := &todoTool{coordinator: c}

	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "researcher" })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = "1" })

	resp, err := tool.handleUpdate("researcher", "1", "in_progress", "")
	if err != nil {
		t.Fatalf("handleUpdate returned error: %v", err)
	}
	if resp.IsError != true {
		t.Fatal("expected error response when updating done task to in_progress")
	}

	all := c.taskTracker.TodoList().Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 to remain done, got %s", all[0].Status)
	}
}

func TestTodoToolHandleUpdatePreventErrorToDone(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "test-team"},
		},
		reportStatus: func(event StatusEvent) {},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	c.taskTracker.TodoList().UpdateStatus("1", TaskInProgress, "")
	c.taskTracker.TodoList().UpdateStatus("1", TaskError, "failed")

	tool := &todoTool{coordinator: c}

	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "researcher" })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = "1" })

	// TaskError -> TaskDone should be blocked (not a valid retry)
	resp, err := tool.handleUpdate("researcher", "1", "done", "")
	if err != nil {
		t.Fatalf("handleUpdate returned error: %v", err)
	}
	if resp.IsError != true {
		t.Fatal("expected error response when updating error task to done")
	}

	all := c.taskTracker.TodoList().Items()
	if all[0].Status != TaskError {
		t.Errorf("expected item 1 to remain error, got %s", all[0].Status)
	}
}

func TestTodoListUpdateStatusPreventDoneToPlanned(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	tl.UpdateStatus("1", TaskDone, "")

	tl.UpdateStatus("1", TaskPlanned, "")

	all := tl.Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 to remain done (not revert to planned), got %s", all[0].Status)
	}
}

func TestTodoListUpdateStatusPreventDoneToPaused(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	tl.UpdateStatus("1", TaskDone, "")

	tl.UpdateStatus("1", TaskPaused, "")

	all := tl.Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 to remain done (not revert to paused), got %s", all[0].Status)
	}
}

func TestTodoListUpdateStatusPreventErrorToPlanned(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	tl.UpdateStatus("1", TaskError, "failed")

	tl.UpdateStatus("1", TaskPlanned, "")

	all := tl.Items()
	if all[0].Status != TaskError {
		t.Errorf("expected item 1 to remain error (not revert to planned), got %s", all[0].Status)
	}
}

func TestTodoListUpdateStatusAllowErrorToInProgress(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskInProgress, "")
	tl.UpdateStatus("1", TaskError, "failed")

	// Retry should be allowed: TaskError -> TaskInProgress
	tl.UpdateStatus("1", TaskInProgress, "retrying")

	all := tl.Items()
	if all[0].Status != TaskInProgress {
		t.Errorf("expected item 1 to be in_progress (retry allowed), got %s", all[0].Status)
	}
	if all[0].Detail != "retrying" {
		t.Errorf("expected detail 'retrying', got %q", all[0].Detail)
	}
}

func TestTodoListUpdateStatusPreventSkippedToPending(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskSkipped, "not needed")

	tl.UpdateStatus("1", TaskPending, "")

	all := tl.Items()
	if all[0].Status != TaskSkipped {
		t.Errorf("expected item 1 to remain skipped (not revert to pending), got %s", all[0].Status)
	}
}

func TestTodoListUpdateStatusPreventSkippedToInProgress(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	tl.UpdateStatus("1", TaskSkipped, "not needed")

	tl.UpdateStatus("1", TaskInProgress, "")

	all := tl.Items()
	if all[0].Status != TaskSkipped {
		t.Errorf("expected item 1 to remain skipped (not revert to in_progress), got %s", all[0].Status)
	}
}

func TestTodoToolHandleUpdatePreventDoneToAny(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "test-team"},
		},
		reportStatus: func(event StatusEvent) {},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	c.taskTracker.TodoList().UpdateStatus("1", TaskInProgress, "")
	c.taskTracker.TodoList().UpdateStatus("1", TaskDone, "")

	tool := &todoTool{coordinator: c}

	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "researcher" })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = "1" })

	resp, err := tool.handleUpdate("researcher", "1", "in_progress", "")
	if err != nil {
		t.Fatalf("handleUpdate returned error: %v", err)
	}
	if resp.IsError != true {
		t.Fatal("expected error response when updating done task")
	}

	all := c.taskTracker.TodoList().Items()
	if all[0].Status != TaskDone {
		t.Errorf("expected item 1 to remain done, got %s", all[0].Status)
	}
}

func TestTodoToolHandleUpdatePreventErrorToDoneDup(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "test-team"},
		},
		reportStatus: func(event StatusEvent) {},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	c.taskTracker.TodoList().UpdateStatus("1", TaskInProgress, "")
	c.taskTracker.TodoList().UpdateStatus("1", TaskError, "failed")

	tool := &todoTool{coordinator: c}

	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "researcher" })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = "1" })

	// TaskError -> TaskDone should be blocked (not a valid retry)
	resp, err := tool.handleUpdate("researcher", "1", "done", "")
	if err != nil {
		t.Fatalf("handleUpdate returned error: %v", err)
	}
	if resp.IsError != true {
		t.Fatal("expected error response when updating error task to done")
	}

	all := c.taskTracker.TodoList().Items()
	if all[0].Status != TaskError {
		t.Errorf("expected item 1 to remain error, got %s", all[0].Status)
	}
}

func TestTodoToolHandleUpdatePreventSkippedToAny(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "test-team"},
		},
		reportStatus: func(event StatusEvent) {},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "researcher", Desc: "find bugs"},
	})

	c.taskTracker.TodoList().UpdateStatus("1", TaskSkipped, "cancelled")

	tool := &todoTool{coordinator: c}

	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = "researcher" })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = "1" })

	resp, err := tool.handleUpdate("researcher", "1", "in_progress", "")
	if err != nil {
		t.Fatalf("handleUpdate returned error: %v", err)
	}
	if resp.IsError != true {
		t.Fatal("expected error response when updating skipped task")
	}

	all := c.taskTracker.TodoList().Items()
	if all[0].Status != TaskSkipped {
		t.Errorf("expected item 1 to remain skipped, got %s", all[0].Status)
	}
}
