package team

import (
	"testing"
)

func TestTodoListAddBatch(t *testing.T) {
	tl := &TodoList{}

	items := tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
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

	tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
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

	tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
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

	items := tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
		{Agent: "c", Desc: "task c"},
	})

	if items[0].ID != "1" {
		t.Errorf("expected ID to reset to 1 after clear, got %s", items[0].ID)
	}
}

func TestTodoListIDsAutoIncrement(t *testing.T) {
	tl := &TodoList{}

	batch1 := tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
		{Agent: "a", Desc: "first"},
	})
	batch2 := tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
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

func TestTaskTrackerTodoList(t *testing.T) {
	tr := NewTaskTracker()

	if tr.TodoList() == nil {
		t.Fatal("expected TodoList to be non-nil")
	}

	tr.TodoList().AddBatch([]struct {
		Agent string
		Desc  string
	}{
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
