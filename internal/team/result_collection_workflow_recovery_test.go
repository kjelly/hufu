package team

import (
	"errors"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func blockWorkflowCollectionRecovery(t *testing.T, c *Coordinator, id string, receipt ExecutionReceipt) *ResultCollectionCheckpoint {
	t.Helper()
	workReceipt := receipt
	workReceipt.TaskID, workReceipt.ModelExecutionID = id, "work-"+id
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, id, receipt.RunID, 1, c.todoItemByID(id).Agent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transcript.Close() })
	if err := transcript.RecordAssistantOutput("original independent work " + id); err != nil {
		t.Fatal(err)
	}
	ref, err := sealVerbatimTaskTranscript(transcript)
	if err != nil {
		t.Fatal(err)
	}
	workReceipt.TranscriptRef = ref.ID
	workCtx := occurrenceTestContext(c, id, 1)
	cp, err := c.beginResultCollection(workCtx, id, workReceipt, "original independent report "+id)
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.reserveResultCollection(workCtx, cp, "collector-"+id, []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.recordCollectionResponse(workCtx, cp, []byte(`{"status":"success","summary":"saved independent result"}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = c.blockResultCollection(workCtx, cp, RepairFailureInvalidSchema)
	if err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(id, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: cp, Cause: errors.New("rejected collection")})
	c.PersistFailureWithClassAndStatusAndOutput(c.todoItemByID(id).Agent, "review", id, "rejected collection", NeedsHuman, FailureProtocol, TaskBlocked, "saved report")
	return cp
}

func TestResultCollectionWorkflowRecoveryRepairsSuccessivePhases(t *testing.T) {
	for _, scenario := range []string{"completed", "earlier_result_changed", "failed_phase_changed"} {
		t.Run(scenario, func(t *testing.T) {
			c, first, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.Phase, spec.ContractID = PhasePrepare, "prepare"
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
			if err := c.phaseWorkflow.restore(PhasePrepare, map[Phase]PhaseResult{}, "", NewRetryState()); err != nil {
				t.Fatal(err)
			}
			originals := make(map[string]*ResultCollectionCheckpoint)
			for index, phase := range []Phase{PhasePrepare, PhaseVerify} {
				id := first.ID
				if index == 1 {
					c.executionRunID = "later-phase-work"
					spec := TodoSpec{Agent: first.Agent, Desc: "independent later review", Phase: phase, ContractID: "review", Model: receipt.Model, ExecutionTarget: receipt.ExecutionTarget, ExecutionTopology: first.ExecutionTopology, Execution: first.Execution}
					ids := c.taskTracker.TodoList().ReserveIDs(1)
					projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
					if err != nil {
						t.Fatal(err)
					}
					if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
						t.Fatal(err)
					}
					items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
					if err != nil {
						t.Fatal(err)
					}
					id = items[0].ID
					if err := c.CommitTaskTransition(t.Context(), id, TaskPending, TaskInProgress, "working", "", nil); err != nil {
						t.Fatal(err)
					}
				}
				work := receipt
				work.RunID = c.executionRunID
				originals[id] = blockWorkflowCollectionRecovery(t, c, id, work)
				if err := c.phaseWorkflow.observe(c.taskTracker.TodoList().Items()); err == nil {
					t.Fatal("blocked collection did not fail its phase")
				}
				c.saveCheckpoint()
				c = reopenCollectedWorkflowCoordinator(t, c)
				assessment, err := c.AssessResultCollectionRecovery(t.Context(), id)
				if err != nil || assessment.Disposition != RecoveryCommitLocal {
					t.Fatalf("phase %s assessment=%+v err=%v", phase, assessment, err)
				}
				c.executionRunID = "phase-repair-" + id
				if err := c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), id)); err != nil {
					t.Fatal(err)
				}
				worker := scriptWorker(textTurn("must not repeat completed work"))
				c.workerAgentOverride = worker
				if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
					t.Fatal(err)
				}
				if worker.Calls() != 0 || c.todoItemByID(id).Status != TaskDone {
					t.Fatal("collection repair repeated work or lost the accepted result")
				}
				// Reconstruct solely from the journal after completion, including
				// the earlier phase's successful repair and its frozen grant.
				c = reopenCollectedWorkflowCoordinator(t, c)
				if index == 1 && scenario == "earlier_result_changed" {
					items := c.taskTracker.TodoList().Items()
					changed := cloneTodoItem(todoItemByID(items, first.ID))
					changed.TypedResult.Summary = "forged earlier result"
					items[slices.IndexFunc(items, func(item *TodoItem) bool { return item.ID == first.ID })] = changed
					c.taskTracker.TodoList().Restore(items)
				}
				if index == 1 && scenario == "failed_phase_changed" {
					c.phaseWorkflow.results[phase] = PhaseResult{Status: PhaseStatusFailure, Errors: []ExecutionError{{Category: "CONFIGURATION"}}}
				}
				_, err = c.beginFollowUpWorkflow(WithSessionResume(t.Context()), "resume accepted collection")
				if index == 1 && scenario != "completed" {
					if err == nil || c.phaseWorkflow.State() != PhaseFailed {
						t.Fatal("changed evidence reopened the later failed phase")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := c.reconcileRecoveredWorkflow(t.Context()); err != nil {
					t.Fatal(err)
				}
				want := PhaseVerify
				if index == 1 {
					want = PhaseDone
				}
				if c.phaseWorkflow.State() != want {
					t.Fatalf("phase %s recovered to %s, want %s", phase, c.phaseWorkflow.State(), want)
				}
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			items, err := ReplayTodoList(events)
			if err != nil || ReduceToSessionData(events).WorkflowState != PhaseDone {
				t.Fatalf("repaired phases did not survive replay: %v", err)
			}
			for _, item := range items {
				original, cp := originals[item.ID], currentResultCollection(item)
				if item.Status != TaskDone || cp.Work != original.Work || cp.TurnsUsed != original.TurnsUsed || item.TypedResult.Summary != "saved independent result" {
					t.Fatal("successive phase repairs changed accepted work or consumed extra turns")
				}
			}
		})
	}
}

func TestResultCollectionWorkflowRecoveryRepairsParallelTasksSequentially(t *testing.T) {
	for _, scenario := range []string{"forward", "reverse", "prepare_phase", "changed_peer_result", "missing_peer_grant"} {
		t.Run(scenario, func(t *testing.T) {
			phase, next := PhaseVerify, PhaseDone
			if scenario == "prepare_phase" {
				phase, next = PhasePrepare, PhaseVerify
			}
			c, first, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.Phase, spec.ContractID = phase, "review"
			}, func(c *Coordinator) {
				c.session.Config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "verify"}}
				c.session.Config.Policies.AllowPhaseSkip = true
				c.session.ContractTasks = []TaskDef{{ID: "prepare", Agent: "worker", Phase: PhasePrepare}, {ID: "review", Agent: "worker", Phase: PhaseVerify}, {ID: "other-review", Agent: "worker", Phase: PhaseVerify}}
				if phase == PhasePrepare {
					c.session.ContractTasks = []TaskDef{{ID: "review", Agent: "worker", Phase: PhasePrepare}, {ID: "other-review", Agent: "worker", Phase: PhasePrepare}, {ID: "verify", Agent: "worker", Phase: PhaseVerify}}
				}
				var err error
				c.phaseWorkflow, err = newRuntimeWorkflow(c.session)
				if err != nil {
					t.Fatal(err)
				}
				c.SetSessionData(NewSession())
			})
			spec := TodoSpec{Agent: first.Agent, Desc: "independent review", Phase: phase, ContractID: "other-review", Model: receipt.Model, ExecutionTarget: receipt.ExecutionTarget, ExecutionTopology: first.ExecutionTopology, Execution: first.Execution}
			ids := c.taskTracker.TodoList().ReserveIDs(1)
			projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.admitTaskOccurrence(ctx, projection, ids[0], 1); err != nil {
				t.Fatal(err)
			}
			created, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, ids)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.CommitTaskTransition(ctx, created[0].ID, TaskPending, TaskInProgress, "working", "", nil); err != nil {
				t.Fatal(err)
			}
			results := map[Phase]PhaseResult{}
			if phase == PhaseVerify {
				results[PhasePrepare] = PhaseResult{Status: PhaseStatusSuccess}
			}
			if err := c.phaseWorkflow.restore(phase, results, "", NewRetryState()); err != nil {
				t.Fatal(err)
			}
			originals := make(map[string]*ResultCollectionCheckpoint)
			for _, id := range []string{first.ID, created[0].ID} {
				originals[id] = blockWorkflowCollectionRecovery(t, c, id, receipt)
			}
			if err := c.phaseWorkflow.observe(c.taskTracker.TodoList().Items()); err == nil {
				t.Fatal("workflow did not fail on blocked collections")
			}
			c.saveCheckpoint()
			order := []string{first.ID, created[0].ID}
			if scenario == "reverse" {
				slices.Reverse(order)
			}
			for index, id := range order {
				c = reopenCollectedWorkflowCoordinator(t, c)
				a, err := c.AssessResultCollectionRecovery(t.Context(), id)
				if err != nil || a.Disposition != RecoveryCommitLocal || a.ContinuesWorkflow != (index == 1) {
					t.Fatalf("task %s assessment=%+v err=%v", id, a, err)
				}
				c.executionRunID = "repair-" + id
				repair := WithResultOnlyRepair(t.Context(), id)
				if _, err := c.beginFollowUpWorkflow(repair, "repair collection"); err != nil {
					t.Fatal(err)
				}
				if err := c.authorizeOperatorResultRepair(repair); err != nil {
					t.Fatal(err)
				}
				worker := scriptWorker(textTurn("must not repeat work"))
				c.workerAgentOverride = worker
				if _, err := c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(id)), id); err != nil {
					t.Fatal(err)
				}
				if worker.Calls() != 0 || c.todoItemByID(id).Status != TaskDone || c.phaseWorkflow.State() != PhaseFailed {
					t.Fatal("repair replayed work or reopened workflow prematurely")
				}
				// Crash after completion, before settling the workflow gate.
				c = reopenCollectedWorkflowCoordinator(t, c)
				if index == 1 && (scenario == "changed_peer_result" || scenario == "missing_peer_grant") {
					changed := cloneTodoItem(c.todoItemByID(order[0]))
					if scenario == "changed_peer_result" {
						changed.TypedResult.Summary = "forged peer result"
					} else {
						changed.ExecutionReceipt.RepairProvenance.OperatorGrants = nil
					}
					items := c.taskTracker.TodoList().Items()
					items[slices.IndexFunc(items, func(item *TodoItem) bool { return item.ID == changed.ID })] = changed
					c.taskTracker.TodoList().Restore(items)
					_ = c.reconcileRecoveredWorkflow(t.Context())
					if c.phaseWorkflow.State() != PhaseFailed {
						t.Fatal("noncanonical peer proof reopened workflow")
					}
					return
				}
				if err := c.reconcileRecoveredWorkflow(t.Context()); err != nil {
					t.Fatal(err)
				}
				if index == 0 {
					if c.phaseWorkflow.State() != PhaseFailed || c.todoItemByID(order[1]).Status != TaskBlocked {
						t.Fatal("partial recovery released a peer or downstream work")
					}
					if _, err := c.beginFollowUpWorkflow(WithSessionResume(t.Context()), "ordinary resume"); err == nil {
						t.Fatal("ordinary resume bypassed the remaining blocked task")
					}
				} else if c.phaseWorkflow.State() != next {
					t.Fatalf("complete recovery left workflow in %s", c.phaseWorkflow.State())
				}
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := ReplayTodoList(events)
			if err != nil || ReduceToSessionData(events).WorkflowState != next {
				t.Fatalf("canonical replay failed: %v", err)
			}
			for _, item := range replayed {
				original := originals[item.ID]
				cp := currentResultCollection(item)
				if item.Status != TaskDone || cp.Work != original.Work || cp.TurnsUsed != original.TurnsUsed || item.TypedResult.Summary != "saved independent result" {
					t.Fatal("replay changed saved work, claims or collector turns")
				}
			}
		})
	}
}

func TestResultCollectionWorkflowRecoveryRequiresFailureAttribution(t *testing.T) {
	for _, kind := range []string{"matching", "optional", "wrong_optional_category", "ordinary", "completed", "structural", "different_task", "different_phase", "different_failure", "repair_progress", "other_unresolved", "parallel_collections", "parallel_wrong_phase", "parallel_unsafe", "extra_failure", "uncertain_effect"} {
		t.Run(kind, func(t *testing.T) {
			c, id, _ := blockedRecoveryFixture(t, `{"status":"success","summary":"saved work"}`, false, RepairFailureInvalidSchema)
			item := cloneTodoItem(c.todoItemByID(id))
			item.Phase = PhaseVerify
			failure := ExecutionError{Phase: PhaseVerify, Category: "TASK_BLOCKED", Component: item.Agent, Source: item.Agent, Message: item.Detail, Cause: item.Detail}
			source := &SessionData{WorkflowState: PhaseFailed, Tasks: []*TodoItem{item}, PhaseResults: map[Phase]PhaseResult{PhaseVerify: {Status: PhaseStatusBlocked, Errors: []ExecutionError{failure}}}}
			switch kind {
			case "optional":
				item.Optional = true
				failure.Category = "OPTIONAL_TASK_BLOCKED"
			case "wrong_optional_category":
				item.Optional = true
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
			case "other_unresolved", "parallel_collections", "parallel_wrong_phase", "parallel_unsafe":
				other := cloneTodoItem(item)
				other.ID = "other"
				source.Tasks = append(source.Tasks, other)
				if kind == "other_unresolved" {
					other.Status = TaskPending
				}
				if kind == "parallel_wrong_phase" {
					other.Phase = PhasePrepare
				}
				if kind == "parallel_unsafe" {
					other.RecoveryState = RecoveryStateUnknown
				}
			case "uncertain_effect":
				item.RecoveryState = RecoveryStateUnknown
			}
			source.PhaseResults[PhaseVerify] = PhaseResult{Status: PhaseStatusBlocked, Errors: []ExecutionError{failure}}
			if kind == "parallel_collections" || kind == "parallel_wrong_phase" || kind == "parallel_unsafe" || kind == "extra_failure" {
				result := source.PhaseResults[PhaseVerify]
				result.Errors = append(result.Errors, failure)
				source.PhaseResults[PhaseVerify] = result
			}
			before, err := collectionDigest(source)
			if err != nil {
				t.Fatal(err)
			}
			phase, err := resultRepairWorkflowPhase(source, id)
			wantAllowed := kind == "matching" || kind == "optional" || kind == "ordinary" || kind == "repair_progress" || kind == "parallel_collections"
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
	for _, boundary := range []string{"completed", "optional_completed", "workflow_changed", "result_changed"} {
		t.Run(boundary, func(t *testing.T) {
			c, item, ctx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.Phase, spec.ContractID = PhaseVerify, "review"
				spec.Optional = boundary == "optional_completed"
			}, func(c *Coordinator) {
				c.session.Config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "verify"}}
				c.session.Config.Policies.AllowPhaseSkip = true
				c.session.ContractTasks = []TaskDef{{ID: "prepare", Agent: "worker", Phase: PhasePrepare}, {ID: "review", Agent: "worker", Phase: PhaseVerify}}
				c.session.ContractTasks[1].Optional = boundary == "optional_completed"
				if boundary == "optional_completed" {
					c.session.ContractTasks = append(c.session.ContractTasks, TaskDef{ID: "required-review", Agent: "worker", Phase: PhaseVerify})
				}
				var err error
				c.phaseWorkflow, err = newRuntimeWorkflow(c.session)
				if err != nil {
					t.Fatal(err)
				}
				c.SetSessionData(NewSession())
			})
			if boundary == "optional_completed" {
				required := &TodoItem{ID: "required", Agent: "worker", Status: TaskDone, Phase: PhaseVerify, ContractID: "required-review", TypedResult: &TaskResult{TaskID: "required", Agent: "worker", Status: TaskResultStatusSuccess, Summary: "required review completed"}}
				c.taskTracker.TodoList().AddReserved([]*TodoItem{required})
				if _, err := c.EventJournal().Append(ctx, RunEvent{Type: string(EventTaskCreated), RunID: receipt.RunID, BranchID: "main", TaskID: required.ID, Actor: required.Agent, Payload: mustProjectionJSON(t, required)}); err != nil {
					t.Fatal(err)
				}
			}
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
			if boundary != "completed" && boundary != "optional_completed" {
				if err == nil || c.phaseWorkflow.State() != PhaseFailed {
					t.Fatal("changed workflow or noncanonical result reopened a failed phase")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.reconcileRecoveredWorkflow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if boundary == "optional_completed" {
				if err := c.phaseWorkflow.requireFinished(); err != nil {
					t.Fatal(err)
				}
				c.saveCheckpoint()
			}
			if c.phaseWorkflow.State() != PhaseDone {
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
