package team

import (
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResultCollectionWorkflowRecoveryRequiresFailureAttribution(t *testing.T) {
	for _, kind := range []string{"matching", "ordinary", "completed", "structural", "different_task", "different_phase", "different_failure", "repair_progress", "other_unresolved", "uncertain_effect"} {
		t.Run(kind, func(t *testing.T) {
			c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"saved work"}`, false, RepairFailureInvalidSchema)
			item := cloneTodoItem(c.todoItemByID(id))
			item.Phase = PhaseVerify
			failure := ExecutionError{Phase: PhaseVerify, Category: "TASK_BLOCKED", Component: item.Agent, Source: item.Agent, Message: item.Detail, Cause: item.Detail}
			source := &SessionData{WorkflowState: PhaseFailed, Tasks: []*TodoItem{item}, PhaseResults: map[Phase]PhaseResult{PhaseVerify: {Status: PhaseStatusBlocked, Errors: []ExecutionError{failure}}}}
			switch kind {
			case "ordinary":
				source.WorkflowState = PhaseVerify
			case "completed":
				source.WorkflowState = PhaseDone
			case "structural":
				failure.Category = "CONFIGURATION"
			case "different_task":
				id = "unrelated"
			case "different_phase":
				failure.Phase = PhasePrepare
			case "different_failure":
				failure.Component = "another owner"
			case "repair_progress":
				failure.Message = "previous format rejection"
				failure.Cause = failure.Message
			case "other_unresolved":
				other := cloneTodoItem(item)
				other.ID = "other"
				source.Tasks = append(source.Tasks, other)
			case "uncertain_effect":
				item.RecoveryState = RecoveryStateUnknown
			}
			source.PhaseResults[PhaseVerify] = PhaseResult{Status: PhaseStatusBlocked, Errors: []ExecutionError{failure}}
			before, err := collectionDigest(source)
			if err != nil {
				t.Fatal(err)
			}
			phase, err := resultRepairWorkflowPhase(source, id)
			wantAllowed := kind == "matching" || kind == "ordinary" || kind == "repair_progress"
			if (err == nil) != wantAllowed || (kind == "matching" && phase != PhaseVerify) {
				t.Fatalf("phase=%s error=%v", phase, err)
			}
			after, err := collectionDigest(source)
			if err != nil || before != after {
				t.Fatal("workflow assessment mutated canonical evidence")
			}
		})
	}
}

func reopenCollectedWorkflowCoordinator(t *testing.T, previous *Coordinator) *Coordinator {
	t.Helper()
	c := reopenRecoveryCoordinator(t, previous)
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w, err := newRuntimeWorkflow(c.session)
	if err != nil {
		t.Fatal(err)
	}
	c.phaseWorkflow = w
	c.SetSessionData(ReduceToSessionData(events))
	return c
}

func TestResultCollectionWorkflowRecoverySurvivesCompletionCheckpoint(t *testing.T) {
	for _, boundary := range []string{"completed", "workflow_changed", "result_changed"} {
		t.Run(boundary, func(t *testing.T) {
			c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.Phase, spec.ContractID = PhaseVerify, "review"
			}, func(c *Coordinator) {
				c.session.Config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "verify"}}
				c.session.Config.Policies.AllowPhaseSkip = true
				c.session.ContractTasks = []TaskDef{{ID: "prepare", Agent: "worker", Phase: PhasePrepare}, {ID: "review", Agent: "worker", Phase: PhaseVerify}}
				var err error
				c.phaseWorkflow, err = newRuntimeWorkflow(c.session)
				if err != nil {
					t.Fatal(err)
				}
				c.SetSessionData(NewSession())
			})
			if err := c.phaseWorkflow.restore(PhaseVerify, map[Phase]PhaseResult{PhasePrepare: {Status: PhaseStatusSuccess}}, "", NewRetryState()); err != nil {
				t.Fatal(err)
			}
			cp, err := c.beginResultCollection(ctx, item.ID, receipt, "completed original work")
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"retained result"}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.blockResultCollection(ctx, cp, RepairFailureInvalidSchema)
			if err != nil {
				t.Fatal(err)
			}
			c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: cp, Cause: errors.New("historical rejection")})
			c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "historical rejection", NeedsHuman, FailureProtocol, TaskBlocked, "saved work")
			if err := c.phaseWorkflow.observe(c.taskTracker.TodoList().Items()); err == nil {
				t.Fatal("fixture workflow did not fail on its blocked collection")
			}
			c.saveCheckpoint()
			c = reopenCollectedWorkflowCoordinator(t, c)
			c.executionRunID = "operator-recovery"
			repair := WithResultOnlyRepair(t.Context(), item.ID)
			if _, err := c.beginFollowUpWorkflow(repair, "recover saved result"); err != nil || c.phaseWorkflow.State() != PhaseFailed {
				t.Fatalf("repair admission reopened the phase prematurely: %v", err)
			}
			if err := c.authorizeOperatorResultRepair(repair); err != nil {
				t.Fatal(err)
			}
			if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID); err != nil {
				t.Fatal(err)
			}
			if c.todoItemByID(item.ID).Status != TaskDone || c.phaseWorkflow.State() != PhaseFailed {
				t.Fatal("fixture did not stop between task completion and phase settlement")
			}
			c = reopenCollectedWorkflowCoordinator(t, c)
			if boundary == "workflow_changed" {
				c.phaseWorkflow.results[PhaseVerify] = PhaseResult{Status: PhaseStatusFailure, Errors: []ExecutionError{{Category: "CONFIGURATION"}}}
			}
			if boundary == "result_changed" {
				changed := cloneTodoItem(c.todoItemByID(item.ID))
				changed.TypedResult.Summary = "forged result"
				c.taskTracker.TodoList().Restore([]*TodoItem{changed})
			}
			_, err = c.beginFollowUpWorkflow(WithSessionResume(t.Context()), "resume saved result")
			if boundary != "completed" {
				if err == nil || c.phaseWorkflow.State() != PhaseFailed {
					t.Fatal("changed workflow or noncanonical result reopened a failed phase")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.reconcileRecoveredWorkflow(t.Context()); err != nil || c.phaseWorkflow.State() != PhaseDone {
				t.Fatalf("completed repair did not settle workflow after restart: %v", err)
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if ReduceToSessionData(events).WorkflowState != PhaseDone {
				t.Fatal("workflow completion did not survive canonical replay")
			}
		})
	}
}
