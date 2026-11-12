package team

import (
	json "encoding/json/v2"
	"io"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
)

func cleanupTaskDistillationFixture(t *testing.T, closer io.Closer) {
	t.Helper()
	t.Cleanup(func() { _ = closer.Close() })
}

func distillationTaskFixture(t *testing.T) (string, *EventStore) {
	t.Helper()
	workspace := t.TempDir()
	sd := NewSession()
	sd.Tasks = []*TodoItem{{ID: "1", Agent: "worker", Desc: "completed work", Status: TaskDone, Output: "A reusable verified procedure."}, {ID: "2", Agent: "worker", Desc: "still working", Status: TaskPending}}
	if err := SaveSession(workspace, sd); err != nil {
		t.Fatal(err)
	}
	st := NewSessionTree()
	st.Branches["main"].State.TaskPlan = sd.Tasks
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	for _, item := range sd.Tasks {
		payload, _ := json.Marshal(map[string]any{"id": item.ID, "agent": item.Agent, "desc": item.Desc, "status": string(item.Status), "output": item.Output})
		if err = es.Append(RunEvent{Type: string(EventTaskCreated), BranchID: "main", TaskID: item.ID, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		if item.Status == TaskDone {
			if err = es.Append(RunEvent{Type: string(EventTaskCompleted), BranchID: "main", TaskID: item.ID, Payload: payload}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return workspace, es
}

func TestTaskDistillationRemovalCommitsEventsCheckpointAndReplay(t *testing.T) {
	workspace, es := distillationTaskFixture(t)
	snapshot, err := SnapshotTasksForDistillation(t.Context(), workspace, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	draft := taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "1", Action: "distill", Reason: "useful"}}, Knowledge: []taskdistill.Knowledge{{Text: "Keep the verified reusable procedure.", SourceIDs: []string{"1"}}}}
	for range 2 {
		if err = RemoveDistilledTasks(t.Context(), workspace, "plan", snapshot, draft); err != nil {
			t.Fatal(err)
		}
	}
	_ = es.Close()
	es, err = OpenEventStoreReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	sd := LoadSession(workspace)
	if len(sd.Tasks) != 1 || sd.Tasks[0].ID != "2" {
		t.Fatalf("checkpoint tasks=%+v", sd.Tasks)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	tasks := ReduceToTodoList(events)
	if len(tasks) != 1 || tasks[0].ID != "2" {
		t.Fatalf("replayed tasks=%+v", tasks)
	}
	st, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = RebuildSessionForBranch(workspace, st, es, "main"); err != nil {
		t.Fatal(err)
	}
	if len(LoadSession(workspace).Tasks) != 1 {
		t.Fatal("branch replay resurrected removed task")
	}
	if len(events) != 4 {
		t.Fatalf("events=%d: retry appended removal twice", len(events))
	}
}

func TestTaskDistillationProtectsDependenciesBranchesAndStaleSources(t *testing.T) {
	for _, mode := range []string{"dependency", "branch", "incomplete", "stale"} {
		t.Run(mode, func(t *testing.T) {
			workspace, es := distillationTaskFixture(t)
			sd := LoadSession(workspace)
			switch mode {
			case "dependency":
				sd.Tasks[1].EvidenceFrom = []string{"1"}
			case "incomplete":
				sd.Tasks[0].Status = TaskError
				payload, _ := json.Marshal(map[string]string{"id": "1", "status": "error", "output": sd.Tasks[0].Output})
				if err := es.Append(RunEvent{Type: string(EventTaskFailed), BranchID: "main", TaskID: "1", Payload: payload}); err != nil {
					t.Fatal(err)
				}
			case "branch":
				st, err := LoadSessionTree(workspace)
				if err != nil {
					t.Fatal(err)
				}
				st.Branches["other"] = &SessionBranch{ID: "other", Name: "other", State: BranchState{TaskPlan: []*TodoItem{sd.Tasks[0]}}}
				if err = SaveSessionTree(workspace, st); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveSession(workspace, sd); err != nil {
				t.Fatal(err)
			}
			snapshot, err := SnapshotTasksForDistillation(t.Context(), workspace, []string{"1"})
			if err != nil {
				t.Fatal(err)
			}
			if mode != "stale" && !snapshot.Sources[0].Protected {
				t.Fatal("unsafe task was not protected")
			}
			if mode == "stale" {
				sd.Tasks[0].Output = "changed"
				if err = SaveSession(workspace, sd); err != nil {
					t.Fatal(err)
				}
			}
			err = RemoveDistilledTasks(t.Context(), workspace, "plan", snapshot, taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "1", Action: "discard", Reason: "no durable value"}}})
			if err == nil {
				t.Fatal("unsafe cleanup succeeded")
			}
			if len(LoadSession(workspace).Tasks) != 2 {
				t.Fatal("failed cleanup changed tasks")
			}
		})
	}
}

func TestTaskDistillationProjectionFailureRollsBackEvent(t *testing.T) {
	workspace, es := distillationTaskFixture(t)
	snapshot, err := SnapshotTasksForDistillation(t.Context(), workspace, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtimestore.Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	cleanupTaskDistillationFixture(t, store)
	if err = store.Exec(t.Context(), `CREATE TRIGGER refuse_checkpoint BEFORE UPDATE ON runtime_documents WHEN OLD.name='session' BEGIN SELECT RAISE(ABORT,'fixture storage failure'); END`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	err = RemoveDistilledTasks(t.Context(), workspace, "plan", snapshot, taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "1", Action: "discard", Reason: "obsolete"}}})
	if err == nil {
		t.Fatal("expected projection failure")
	}
	_ = es.Close()
	es, err = OpenEventStoreReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || len(LoadSession(workspace).Tasks) != 2 {
		t.Fatal("projection failure left a partial removal")
	}
}

func TestTaskDistillationReservesRemovedIDsAfterRestartAndReplay(t *testing.T) {
	workspace := t.TempDir()
	sd := NewSession()
	sd.Tasks = []*TodoItem{{ID: "9", Agent: "worker", Desc: "finished", Status: TaskDone}}
	if err := SaveSession(workspace, sd); err != nil {
		t.Fatal(err)
	}
	es, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	cleanupTaskDistillationFixture(t, es)
	if err = es.Append(RunEvent{Type: string(EventTaskCreated), BranchID: "main", TaskID: "9", Payload: []byte(`{"id":"9","status":"done","agent":"worker"}`)}); err != nil {
		t.Fatal(err)
	}
	_ = es.Close()
	snapshot, err := SnapshotTasksForDistillation(t.Context(), workspace, []string{"9"})
	if err != nil {
		t.Fatal(err)
	}
	if err = RemoveDistilledTasks(t.Context(), workspace, "plan", snapshot, taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "9", Action: "discard", Reason: "transient"}}}); err != nil {
		t.Fatal(err)
	}
	for _, replay := range []bool{false, true} {
		if replay {
			es, err = OpenEventStoreReadOnly(workspace)
			if err != nil {
				t.Fatal(err)
			}
			cleanupTaskDistillationFixture(t, es)
			st, err := LoadSessionTree(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if err = RebuildSessionForBranch(workspace, st, es, st.ActiveBranch); err != nil {
				t.Fatal(err)
			}
			_ = es.Close()
		}
		c := newTestCoordinator(t)
		c.SetSessionData(LoadSession(workspace))
		items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "next"}})
		if items[0].ID != "10" {
			t.Fatalf("replay=%v reused removed identity: %s", replay, items[0].ID)
		}
	}
}

func TestTaskDistillationRejectsCheckpointBehindCanonicalEvents(t *testing.T) {
	workspace, es := distillationTaskFixture(t)
	if err := es.Append(RunEvent{Type: string(EventTaskCreated), BranchID: "main", TaskID: "1", Payload: []byte(`{"id":"1","status":"pending","reset_for_retry":true}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotTasksForDistillation(t.Context(), workspace, []string{"1"}); err == nil {
		t.Fatal("stale done checkpoint authorized cleanup of a restarted task")
	}
}

func TestTaskDistillationProtectsCanonicalDependencyMissingFromCheckpoint(t *testing.T) {
	workspace, es := distillationTaskFixture(t)
	if err := es.Append(RunEvent{Type: string(EventTaskCreated), BranchID: "main", TaskID: "3", Payload: []byte(`{"id":"3","status":"pending","depends_on":["1"]}`)}); err != nil {
		t.Fatal(err)
	}
	sd := LoadSession(workspace)
	sd.Tasks = append(sd.Tasks, &TodoItem{ID: "3", Status: TaskPending})
	if err := SaveSession(workspace, sd); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotTasksForDistillation(t.Context(), workspace, []string{"1"})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Sources[0].Protected {
		t.Fatal("canonical dependency lost protection through a stale checkpoint")
	}
}
