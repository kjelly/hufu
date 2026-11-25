package team

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestResultCollectionRecoveryRejectsGoverningAssumptionsInsideResponseEnvelope(t *testing.T) {
	const claims = `{"status":"success","summary":"complete work report","assumption_checks":[{"assumption_id":"A1","status":"unknown"}]}`
	t.Run("rejected wrapper", func(t *testing.T) {
		issue := &agent.StructuredResponseIssue{Code: "additional_property", Expected: "object", Actual: "object"}
		assertRecoveryRejectsGoverningAssumptions(t, `{"result":`+claims+`,"extra":true}`, issue, "response_envelope_sources")
	})
	t.Run("invalid fenced claims", func(t *testing.T) {
		invalid := strings.TrimSuffix(claims, "}") + `,"summary":false}`
		assertRecoveryRejectsGoverningAssumptions(t, "```json\n"+invalid+"\n```", nil, "claims_context")
	})
}

func assertRecoveryRejectsGoverningAssumptions(t *testing.T, response string, issue *agent.StructuredResponseIssue, checkCode string) {
	t.Helper()
	c, item, ctx, receipt := resultCollectionProviderFixture(t, "http://127.0.0.1:1/v1", true, func(c *Coordinator) {
		c.session.Config.Decision = dispatchConfig()
		c.session.Config.Decision.DefaultProfile = "standard"
		c.session.Config.RequestContract = dispatchRequestContract()
	})
	admission, found, err := c.validateTaskOccurrenceAdmission(ctx, taskDefFromTodoItem(item), item.ID, receipt.Attempt)
	if err != nil || !found || !admission.Enabled {
		t.Fatalf("expected governing admission: %+v %v", admission, err)
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		t.Fatal(err)
	}
	record := &DecisionRecord{
		SchemaVersion: 1, ID: admission.DecisionID, RunID: receipt.RunID, TaskID: item.ID, Profile: admission.Profile,
		Assumptions: []DecisionAssumption{{ID: "A1", Statement: "declared check"}},
	}
	if err := appendDecisionEvent(ctx, journal, agent.EventDecisionFinalized, decisionEvent{
		DecisionID: record.ID, RunID: record.RunID, TaskID: item.ID, Attempt: receipt.Attempt, Profile: record.Profile, Record: record,
	}); err != nil {
		t.Fatal(err)
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "complete work report")
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "collector", []byte(`{"tools":[]}`))
	}
	if err == nil {
		cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(response), ExecutionUsage{}, issue)
	}
	if err != nil {
		t.Fatal(err)
	}
	failure := c.collectionFailure(ctx, cp, RepairFailureInvalidSchema, fmt.Errorf("rejected response envelope"))
	c.recordCollectionStopReceipt(item.ID, failure)
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "collection blocked", NeedsHuman, FailureProtocol, TaskBlocked, "")
	c = reopenRecoveryCoordinator(t, c)
	a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
	if err != nil || a.Disposition != RecoveryBlocked || a.AdditionalTurns != 0 || !slices.ContainsFunc(a.Checks, func(check ResultCollectionRecoveryCheck) bool {
		return check.Code == checkCode && !check.Passed && strings.Contains(check.Diagnostic, "cannot change a governing decision")
	}) {
		t.Fatalf("cold assessment missed the nested governing assumption: %+v %v", a, err)
	}
	if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID)); err == nil {
		t.Fatal("cold operator authorization accepted a governing assumption report")
	}
}

func TestResultCollectionRecoveryRejectsUnreconciledContext(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
