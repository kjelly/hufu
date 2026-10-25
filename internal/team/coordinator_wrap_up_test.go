package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func newDrainedWorkflowWrapUp(t *testing.T) (*Coordinator, *EventStore) {
	t.Helper()
	c, store := failureEventCoordinator(t, "graceful-workflow", "")
	c.sessionData = NewSession()
	c.delegatedTasks = make(map[string]int)
	c.maxConcurrent = 1
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "ollama", kind: execution.BackendKindLLM}}); err != nil {
		t.Fatal(err)
	}
	c.SetExecutionRegistry(registry)
	c.session.Config.AllowFreeTextResults = true
	c.session.Agents["worker"].SideEffect = string(SideEffectNone)
	c.session.Config.Workflow.Phases = []string{"prepare", "verify"}
	c.session.Config.Policies.AllowPhaseSkip = true
	c.session.ContractTasks = []TaskDef{
		{ID: "prepare", Agent: "worker", Goal: "prepare inputs", Phase: PhasePrepare},
		{ID: "verify", Agent: "worker", Goal: "verify inputs", Phase: PhaseVerify},
	}
	if err := c.SetAcceptance("touch " + filepath.Join(c.session.Workspace, "acceptance-must-not-run")); err != nil {
		t.Fatal(err)
	}
	w, err := newRuntimeWorkflow(c.session)
	if err != nil {
		t.Fatal(err)
	}
	c.phaseWorkflow = w
	w.setEventEmitter(func(kind string, phase Phase, details LifecycleEventPayload) {
		details.Phase = string(phase)
		if err := c.emitEvent(kind, "runtime", "", details); err != nil {
			t.Error(err)
		}
	})
	c.terminalLifecycleRunID = c.executionRunID
	c.terminalLifecycleDone = make(chan struct{})
	c.terminalLifecyclePrepareDone = make(chan struct{})
	c.workerAgentOverride = scriptWorker(textTurn("prepared immutable evidence"))
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "start" && event.TodoID != CoordTodoID {
			c.SetWrapUp()
		}
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	c.saveCheckpoint()
	if _, err := c.ExecuteTasks(t.Context(), []TaskDef{{Agent: "worker", ContractID: "prepare", Goal: "prepare inputs", Phase: PhasePrepare}}); err != nil {
		t.Fatalf("active prepare task did not drain: %v", err)
	}
	if !c.IsWrapUp() || w.State() != PhaseVerify || len(pendingTodoItems(c.taskTracker.TodoList().Items())) != 0 {
		t.Fatalf("fixture did not reach drained VERIFY during wrap-up: state=%s wrap=%t", w.State(), c.IsWrapUp())
	}
	return c, store
}

func TestGracefulWorkflowFinishPersistsCancellationAndCompletedEvidence(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator_request", true: "acknowledged"}[acknowledged], func(t *testing.T) {
			c, store := newDrainedWorkflowWrapUp(t)
			before := c.taskTracker.TodoList().Items()[0]
			marker := filepath.Join(c.session.Workspace, "acceptance-must-not-run")
			input, err := json.Marshal(map[string]any{"response": "everything passed", "acknowledge_failed_tasks": acknowledged})
			if err != nil {
				t.Fatal(err)
			}
			response, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: string(input)})
			if err != nil || response.IsError || !c.finishCalled.Load() {
				t.Fatalf("graceful finish rejected: %#v %v", response, err)
			}
			result := c.LastRunResult()
			if result == nil || result.Outcome != RunOutcomeCancelled || result.StopReason != StopReasonCancelled || result.GoalSatisfied || result.ExitCode != 130 || result.Acceptance != nil {
				t.Fatalf("unfinished verification was not reported as cancelled: %#v", result)
			}
			if strings.Contains(result.Response, "everything passed") || !strings.Contains(result.Response, "prepared immutable evidence") {
				t.Fatalf("terminal response lost durable evidence or trusted the model's claim: %s", result.Response)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("acceptance ran for an incomplete workflow: %v", err)
			}
			c.FinalizeRun(t.Context(), result, nil)
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			finished := 0
			for _, event := range events {
				if event.Type == string(EventRunFinished) {
					finished++
				}
			}
			projected := ReduceToSessionData(events)
			if finished != 1 || projected.WorkflowState != PhaseFailed || projected.PhaseResults[PhaseVerify].Status != PhaseStatusCancelled || projected.PhaseResults[PhasePrepare].Status != PhaseStatusSuccess {
				t.Fatalf("cancellation did not replay: finished=%d state=%s phases=%#v", finished, projected.WorkflowState, projected.PhaseResults)
			}
			if projected.RunResult == nil || projected.RunResult.Outcome != RunOutcomeCancelled || projected.PendingWrapUp != nil || len(projected.Tasks) != 1 {
				t.Fatalf("terminal projection is inconsistent: %#v", projected)
			}
			after := projected.Tasks[0]
			if after.ID != before.ID || after.Status != TaskDone || after.Output != before.Output || !reflect.DeepEqual(after.TypedResult, before.TypedResult) || !reflect.DeepEqual(after.VerifyResult, before.VerifyResult) {
				t.Fatalf("completed prepare evidence changed: before=%#v after=%#v", before, after)
			}
			if saved := LoadSession(c.session.Workspace); saved == nil || saved.WorkflowState != projected.WorkflowState || saved.RunResult == nil || saved.RunResult.Outcome != RunOutcomeCancelled {
				t.Fatalf("checkpoint differs from replay: %#v", saved)
			}
			cancelled := projected.PhaseResults[PhaseVerify]
			if len(cancelled.Errors) != 1 || cancelled.Errors[0].Category != CategoryCancelled || cancelled.Errors[0].Retryable {
				t.Fatalf("phase cancellation lost its typed cause: %#v", cancelled)
			}
			restored, err := newRuntimeWorkflow(c.session)
			if err != nil {
				t.Fatal(err)
			}
			if err := restored.restore(projected.WorkflowState, projected.PhaseResults, projected.RuntimeWorkspace, projected.RetryState); err != nil {
				t.Fatalf("restore cancelled workflow: %v", err)
			}
			if err := restored.Start(); err == nil {
				t.Fatal("cancelled workflow silently reopened on resume")
			}
		})
	}
}

func TestGracefulWorkflowWrapUpFinalizesEveryCoordinatorBoundary(t *testing.T) {
	for _, name := range []string{"before_model", "model_entry", "continuation_entry", "stream_eof", "recovery", "direct_agent", "cancelled_context"} {
		t.Run(name, func(t *testing.T) {
			c, _ := newDrainedWorkflowWrapUp(t)
			switch name {
			case "before_model":
				if _, _, err := c.runOrchestratorWithNoProgressGuard(t.Context(), nil, "wrap up"); err != nil {
					t.Fatal(err)
				}
			case "model_entry":
				if _, _, err := c.runOrchestrator(t.Context(), nil, "wrap up"); err != nil {
					t.Fatal(err)
				}
			case "continuation_entry":
				c.continueCoordinatorUntilFinished(t.Context(), nil, "unverified narration", nil)
			case "stream_eof":
				c.ensureFinished(t.Context(), nil, "unverified narration", nil)
			case "recovery":
				if _, _, finished := c.attemptWrapUpRecovery(t.Context(), nil, errors.New("coordinator stopped")); !finished {
					t.Fatal("wrap-up recovery requested another model turn")
				}
			case "direct_agent":
				c.finalizeDirectRun(t.Context(), "1", true, "unverified narration")
			case "cancelled_context":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				c.ensureFinished(ctx, nil, "unverified narration", nil)
			}
			if result := c.LastRunResult(); result == nil || result.Outcome != RunOutcomeCancelled || result.GoalSatisfied || !c.finishCalled.Load() {
				t.Fatalf("%s did not finalize cancellation: %#v", name, result)
			}
		})
	}
}

func TestGracefulWorkflowWrapUpDuringCoordinatorContinuation(t *testing.T) {
	for _, stopOnCall := range []int{1, 6} {
		t.Run(map[int]string{1: "continuation", 6: "final_summary"}[stopOnCall], func(t *testing.T) {
			c, store := newDrainedWorkflowWrapUp(t)
			c.wrapUp.Store(0)
			calls := 0
			lastSteps := []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("last admitted coordinator turn")}}}
			c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
				calls++
				if calls == stopOnCall {
					c.SetWrapUp()
					return "step complete", lastSteps, nil
				}
				return "continue verification", nil, nil
			}
			result, steps := c.ensureFinished(t.Context(), nil, "step limit reached", nil)
			if calls != stopOnCall || !reflect.DeepEqual(steps, lastSteps) || !c.finishCalled.Load() || c.LastRunResult().Outcome != RunOutcomeCancelled || !strings.Contains(result, "prepared immutable evidence") {
				t.Fatalf("continuation did not seal cancellation with its final steps: calls=%d result=%s steps=%#v", calls, result, steps)
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := ReduceToSessionData(events).ContinuationCheckpoint
			if checkpoint == nil || checkpoint.Status != "aborted" || checkpoint.TurnCount != min(stopOnCall, 5) || !reflect.DeepEqual(checkpoint, c.ContinuationCheckpoint()) {
				t.Fatalf("cancelled continuation remained pending or failed to replay: %#v", checkpoint)
			}
		})
	}
}

func TestGracefulWorkflowWrapUpPreservesStallCause(t *testing.T) {
	c, _ := newDrainedWorkflowWrapUp(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(ErrInvocationStalled)
	if _, finished := c.finalizeWorkflowWrapUp(ctx); finished || c.finishCalled.Load() {
		t.Fatal("operator wrap-up concealed an invocation stall")
	}
}

func TestGracefulWorkflowWrapUpStopsCoordinatorBeforeAnotherModelStep(t *testing.T) {
	c, _ := newDrainedWorkflowWrapUp(t)
	script := &finishStopScriptAgent{
		tool:   &recordingTool{name: "team_info", resp: fantasy.NewTextResponse("observed")},
		inputs: []string{`{}`, `{}`},
	}
	ctx := context.WithValue(t.Context(), todoIDKey{}, CoordTodoID)
	result, steps, err := c.runAgentWithStatusAndHistory(ctx, script, "coordinator", "wrap up", nil, &taskTiming{})
	if err != nil {
		t.Fatal(err)
	}
	if script.modelCalls != 1 {
		t.Fatalf("coordinator continued for %d model steps after workers drained", script.modelCalls)
	}
	result, _ = c.ensureFinished(ctx, nil, result, steps)
	if !c.finishCalled.Load() || !strings.Contains(result, "prepared immutable evidence") || c.LastRunResult().Outcome != RunOutcomeCancelled {
		t.Fatalf("coordinator did not seal cancellation: %s", result)
	}
}

func TestWorkflowWrapUpBoundariesPreserveVerificationAndWorkerDrain(t *testing.T) {
	for _, name := range []string{"pending", "unrequested", "budget", "blocked_stop", "verified", "acceptance_recovery", "no_progress"} {
		t.Run(name, func(t *testing.T) {
			c, _ := newDrainedWorkflowWrapUp(t)
			switch name {
			case "pending":
				addTodo(t, c, "worker still pending")
			case "unrequested":
				c.wrapUp.Store(0)
			case "budget":
				c.wrapUp.Store(1)
			case "blocked_stop":
				c.wrapUp.Store(wrapUpFromBlockedTask)
			case "verified":
				c.phaseWorkflow.results[PhaseVerify] = PhaseResult{Status: PhaseStatusSuccess}
			case "acceptance_recovery":
				c.acceptanceRecovery.Store(true)
			case "no_progress":
				c.noProgressStopTripped = true
			}
			if _, finished := c.finalizeWorkflowWrapUp(t.Context()); finished || c.finishCalled.Load() || c.phaseWorkflow.State() != PhaseVerify {
				t.Fatalf("wrap-up bypassed %s boundary: phase=%s", name, c.phaseWorkflow.State())
			}
		})
	}
}

func TestDeterministicFinishRequiresWorkflowVerification(t *testing.T) {
	c := newTestCoordinator(t, withTeamConfig(func(cfg *agent.TeamConfig) { cfg.AllowFreeTextResults = true }))
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhaseVerify, results: map[Phase]PhaseResult{}}
	id := addTodo(t, c, "prepared")
	c.taskTracker.TodoList().UpdateStatus(id, TaskDone, "prepared")
	if c.canDeterministicallyFinishCompletedTasks(t.Context()) {
		t.Fatal("completed prepare tasks bypassed workflow verification")
	}
	c.phaseWorkflow.results[PhaseVerify] = PhaseResult{Status: PhaseStatusSuccess}
	if !c.canDeterministicallyFinishCompletedTasks(t.Context()) {
		t.Fatal("verified workflow could not finish deterministically")
	}
}
