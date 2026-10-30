package team

import (
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestResultCollectionRecoveryRejectsUnreconciledContext(t *testing.T) {
	for _, kind := range []string{"unknown_side_effect", "partial_side_effect", "checkpoint_pause", "decision_discipline", "terminal_running", "terminal_unknown", "terminal_failed"} {
		t.Run(kind, func(t *testing.T) {
			c, id, cp := blockedRecoveryFixture(t, `{"status":"success","summary":"report"}`, true, RepairFailureSource)
			item := c.todoItemByID(id)
			switch kind {
			case "unknown_side_effect":
				item.RecoveryState = RecoveryStateUnknown
			case "partial_side_effect":
				item.RecoveryState = RecoveryStatePartial
			case "checkpoint_pause":
				item.CheckpointPause = true
			case "decision_discipline":
				item.DecisionProfile = "careful"
			default:
				session := TerminalSession{ID: "original-terminal", RunID: cp.SourceRunID, OwnerTaskID: id, StartedAt: time.Now(), ExitCode: new(1), State: TerminalSessionExited}
				if kind == "terminal_running" {
					session.Running = true
				}
				if kind == "terminal_unknown" {
					session.ExitCode = nil
				}
				body, err := json.Marshal([]TerminalSession{session})
				if err != nil {
					t.Fatal(err)
				}
				putRuntimeDocument(t, c.session.Workspace, runtimestore.DocumentTerminalSessions, body)
			}
			if err := c.taskTracker.TodoList().TryApplyProjectedItem(item); err != nil {
				t.Fatal(err)
			}
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil || a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 {
				t.Fatalf("assessment=%+v err=%v", a, err)
			}
		})
	}
}

func TestResultCollectionRecoveryValidatesUnrelatedRetainedSources(t *testing.T) {
	for _, valid := range []bool{false, true} {
		t.Run(fmt.Sprint("valid_", valid), func(t *testing.T) {
			c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"report"}`, true, RepairFailureSource)
			source := &TodoItem{ID: "retained-source", Agent: "worker", Status: TaskDone}
			if valid {
				source.TypedResult = &TaskResult{TaskID: source.ID, Agent: source.Agent, Status: TaskResultStatusSuccess, Source: "submitted", Summary: "accepted source"}
			}
			c.taskTracker.TodoList().AddReserved([]*TodoItem{source})
			for _, kind := range []EventType{EventTaskCreated, EventTaskCompleted} {
				body, err := json.Marshal(source, collectionDurationFormat)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(kind), BranchID: "main", RunID: "run-test", TaskID: source.ID, Actor: source.Agent, Payload: body}); err != nil {
					t.Fatal(err)
				}
			}
			a, err := c.AssessResultCollectionRecovery(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if valid {
				if a.Disposition != RecoveryCommitLocal || a.AcceptedTasks == nil || *a.AcceptedTasks != 1 {
					t.Fatalf("assessment=%+v", a)
				}
			} else if a.Disposition != RecoveryBlocked || a.AcceptedTasks != nil {
				t.Fatalf("invalid retained source was presented as reusable: %+v", a)
			}
		})
	}
}
