package team

import (
	"context"
	"testing"
	"time"
)

func TestTerminalHandoffPausesAndResumesTaskRound(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interactive task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{session: &TeamSession{}, taskTracker: tracker, reportStatus: func(StatusEvent) {}}

	cancelled := make(chan struct{})
	coord.registerTerminalRound(item.ID, func() { close(cancelled) })
	session := TerminalSession{OwnerTaskID: item.ID}
	coord.pauseTerminalTask(session)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("takeover did not cancel active model round")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskPaused {
		t.Fatalf("status after takeover = %s, want %s", got, TaskPaused)
	}

	resumed := make(chan bool, 1)
	go func() { resumed <- coord.waitForTerminalResume(context.Background(), item.ID) }()
	select {
	case <-resumed:
		t.Fatal("model round resumed before terminal was released")
	case <-time.After(25 * time.Millisecond):
	}

	coord.resumeTerminalTask(session)
	select {
	case ok := <-resumed:
		if !ok {
			t.Fatal("waitForTerminalResume returned false")
		}
	case <-time.After(time.Second):
		t.Fatal("model round did not resume after terminal release")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskInProgress {
		t.Fatalf("status after release = %s, want %s", got, TaskInProgress)
	}
}
