package team

import (
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestHistoricalResultIDIsStableAndTerminalOnly(t *testing.T) {
	event := RunEvent{ID: "evt-saved", RunID: "run-1", SessionID: "session-1", TaskID: "1", Type: string(EventTaskCompleted)}
	id := HistoricalResultID(event)
	if !strings.HasPrefix(id, "res-") || len(id) != 68 {
		t.Fatalf("result ID = %q", id)
	}
	event.BranchID = "inherited-branch"
	if HistoricalResultID(event) != id {
		t.Fatal("branch inheritance changed result identity")
	}
	event.ID = "evt-second"
	if HistoricalResultID(event) == id {
		t.Fatal("different persisted result reused identity")
	}
	event.Type = string(EventTaskStarted)
	if HistoricalResultID(event) != "" {
		t.Fatal("running task claims a saved result")
	}
}

func TestResultReadyIsEmittedOnlyAfterDurableCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "failed"}[fail], func(t *testing.T) {
			c := &Coordinator{taskTracker: NewTaskTracker(), session: &TeamSession{Config: agent.TeamConfig{Name: "review"}}}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "helper", Desc: "result"}})[0]
			journal := &uniqueRecordingJournal{}
			c.SetEventJournal(journal)
			if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "", "", nil); err != nil {
				t.Fatal(err)
			}
			var ready []StatusEvent
			c.SetStatusReporter(func(event StatusEvent) {
				if event.Type == "task_result_ready" {
					ready = append(ready, event)
				}
			})
			if fail {
				c.SetEventJournal(failingJournal{err: errors.New("disk failed")})
			}
			err := c.CommitTaskTransition(t.Context(), item.ID, TaskInProgress, TaskDone, "", "complete", nil)
			if fail {
				if err == nil || len(ready) != 0 {
					t.Fatalf("failure emitted result: %v %#v", err, ready)
				}
				return
			}
			if err != nil || len(ready) != 1 {
				t.Fatalf("missing result identity: %v %#v", err, ready)
			}
			terminal := journal.events[len(journal.events)-1]
			if ready[0].Data["result_id"] != HistoricalResultID(terminal) || ready[0].TeamName != "review" || ready[0].TodoID != item.ID {
				t.Fatalf("identity drift: %#v", ready[0])
			}
		})
	}
}
