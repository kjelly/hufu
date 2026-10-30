package team

import (
	"encoding/json/v2"
	"errors"
	"reflect"
	"testing"
)

func setTaskInputSnapshot(t *testing.T, c *Coordinator, typed bool) *RunInputSnapshot {
	t.Helper()
	if typed {
		c.session.RunInputDefinitions = []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "string"}, Default: []byte(`"original"`)}}
	}
	snapshot, err := ResolveRunInputSnapshot(c.session.RunInputDefinitions, nil, c.executionRunID, c.executionRunID+":invocation", c.session.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(&SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID})
	return snapshot
}

func TestTaskRunInputsBindEveryOccurrence(t *testing.T) {
	for _, kind := range []string{"worker", "sidecar", "action", "materialized_action", "empty_inputs"} {
		t.Run(kind, func(t *testing.T) {
			c := newTestCoordinator(t)
			snapshot := setTaskInputSnapshot(t, c, kind != "empty_inputs")
			task := TaskDef{Agent: "worker", Goal: "inspect"}
			if kind == "sidecar" {
				task.Sidecar = true
			}
			if kind == "action" || kind == "materialized_action" {
				task.Action = &Action{Capability: "inspect", Type: "read", Payload: `{"scope":"static"}`}
				if kind == "materialized_action" {
					task.Action.InputBindings = []ActionInputBinding{{Input: "scope", Target: "/scope"}}
				}
			}
			materialized, err := c.materializeTaskActions([]TaskDef{task})
			if err != nil {
				t.Fatal(err)
			}
			bound, err := c.canonicalizeTaskOccurrence(materialized[0], c.session.Agents["worker"], task.Model)
			if err != nil {
				t.Fatal(err)
			}
			if bound.RunInputSnapshotID != snapshot.ID || bound.RunInputSnapshotHash != snapshot.SnapshotHash {
				t.Fatalf("unbound %s: %+v", kind, bound)
			}
			if task.RunInputSnapshotID != "" || (kind != "materialized_action" && len(bound.BoundInputs) != 0) {
				t.Fatal("binding mutated the caller or invented materialized action inputs")
			}
			// Every canonical task field must survive projection and event replay.
			item := todoItemFromSpec(todoSpecForTestTask(bound), "1")
			body, err := json.Marshal(item, collectionDurationFormat)
			if err != nil {
				t.Fatal(err)
			}
			items := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: body}})
			if len(items) != 1 || items[0].RunInputSnapshotID != snapshot.ID || items[0].RunInputSnapshotHash != snapshot.SnapshotHash {
				t.Fatalf("replay lost input binding: %+v", items)
			}
		})
	}
}

func TestTaskRunInputsRejectDriftBeforeCreation(t *testing.T) {
	for _, kind := range []string{"missing_snapshot", "invalid_snapshot", "foreign_team", "different_schema", "partial_binding", "different_binding", "missing_occurrence_binding"} {
		t.Run(kind, func(t *testing.T) {
			c := newTestCoordinator(t)
			snapshot := setTaskInputSnapshot(t, c, true)
			c.eventJournal = &memoryJournal{}
			task := TaskDef{Agent: "worker", Goal: "inspect", RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
			switch kind {
			case "missing_snapshot":
				c.SetSessionData(NewSession())
			case "invalid_snapshot":
				c.sessionData.RunInputSnapshots[0].SnapshotHash = "sha256:invalid"
			case "foreign_team":
				c.session.Config.Name = "other"
			case "different_schema":
				c.session.RunInputDefinitions = nil
			case "partial_binding":
				task.RunInputSnapshotHash = ""
			case "different_binding":
				task.RunInputSnapshotID = "other"
			case "missing_occurrence_binding":
				task.RunInputSnapshotID, task.RunInputSnapshotHash = "", ""
			}
			before, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.CommitTaskCreationResolved(t.Context(), []TodoSpec{todoSpecForTestTask(task)}, []string{"1"})
			if !errors.Is(err, errTaskInputDrift) {
				t.Fatalf("creation error = %v", err)
			}
			after, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil || !reflect.DeepEqual(before, after) || len(c.taskTracker.TodoList().Items()) != 0 {
				t.Fatalf("invalid binding escaped creation: %v", err)
			}
			// An old marker or loaded task must not authorize execution after the
			// active input identity changes, or repair its own missing binding.
			c.taskTracker.TodoList().AddReserved([]*TodoItem{todoItemFromSpec(todoSpecForTestTask(task), "1")})
			if _, _, err := c.validateTaskOccurrenceAdmission(t.Context(), task, "1", 1); !errors.Is(err, errTaskInputDrift) {
				t.Fatalf("execution admission error = %v", err)
			}
		})
	}
}

func TestInterruptedRunInputsPreserveLegacyNoInputReplay(t *testing.T) {
	for _, mode := range []string{"legacy_no_inputs", "legacy_completed_no_inputs", "typed_inputs", "explicit_resume"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.SetSessionData(NewSession())
			if mode == "typed_inputs" {
				c.session.RunInputDefinitions = []RunInputDefinition{{Name: "scope", Schema: RunInputSchema{Type: "string"}}}
			}
			c.taskTracker.TodoList().SetRunID("legacy-run")
			item := todoItemFromSpec(TodoSpec{Agent: "worker", Goal: "interrupted"}, "1")
			item.Status = TaskInProgress
			if mode == "legacy_completed_no_inputs" {
				item.Status = TaskDone
			}
			c.taskTracker.TodoList().AddReserved([]*TodoItem{item})
			ctx := t.Context()
			if mode == "explicit_resume" {
				ctx = WithSessionResume(ctx)
			}
			err := c.resolveRunInputsForInvocation(ctx, "continue")
			legacy := mode == "legacy_no_inputs" || mode == "legacy_completed_no_inputs"
			if (err == nil) != legacy || c.RunInputSnapshot() != nil || c.todoItemByID(item.ID).RunInputSnapshotID != "" {
				t.Fatalf("legacy input resolution manufactured identity or admitted typed recovery: %v", err)
			}
		})
	}
}

func TestEmptyRunInputsRetainOriginalContextOnFollowUp(t *testing.T) {
	c := newTestCoordinator(t)
	original := setTaskInputSnapshot(t, c, false)
	c.executionRunID = "follow-up-run"
	if err := c.resolveRunInputsForInvocation(t.Context(), "inspect another document"); err != nil {
		t.Fatal(err)
	}
	snapshot, origin := c.terminalRunInputs(c.executionRunID)
	if snapshot == nil || snapshot.ID != original.ID || origin != original.RunID || len(c.sessionData.RunInputSnapshots) != 1 {
		t.Fatalf("follow-up replaced empty input context: snapshot=%+v origin=%s", snapshot, origin)
	}
}
