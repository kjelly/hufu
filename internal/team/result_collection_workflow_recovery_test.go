package team

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResultCollectionWorkflowRecoveryCommitsPreparedResultWithFailedConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, test := range []struct {
		name       string
		dependency bool
		stranded   bool
		dag        bool
		unrelated  bool
	}{
		{name: "dependent_verifier", dependency: true},
		{name: "unrelated_failure"},
		{name: "stranded_verifier", dependency: true, stranded: true},
		{name: "stranded_dag_verifier", dependency: true, stranded: true, dag: true},
		{name: "unrelated_block", dependency: true, stranded: true, unrelated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, producer, workCtx, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.ContractID, spec.Phase, spec.SideEffect = "report", PhaseVerify, SideEffectNone
			}, func(c *Coordinator) {
				c.session.Config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "verify"}}
				c.session.Config.Policies.AllowPhaseSkip = true
				c.session.ContractTasks = []TaskDef{{ID: "prepare", Agent: "worker", Phase: PhasePrepare}, {ID: "report", Agent: "worker", Phase: PhaseVerify}, {ID: "verify-report", Agent: "worker", Phase: PhaseVerify}}
				workflow, err := newRuntimeWorkflow(c.session)
				if err != nil {
					t.Fatal(err)
				}
				c.phaseWorkflow = workflow
				c.SetSessionData(NewSession())
			})
			if err := c.phaseWorkflow.restore(PhaseVerify, map[Phase]PhaseResult{PhasePrepare: {Status: PhaseStatusSuccess}}, "", NewRetryState()); err != nil {
				t.Fatal(err)
			}
			cp, err := c.beginResultCollection(workCtx, producer.ID, receipt, "original report")
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.reserveResultCollection(workCtx, cp, "report-collector", []byte(`{"tools":[]}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.recordCollectionResponse(workCtx, cp, []byte(`{"status":"success","summary":"original report"}`))
			if err != nil {
				t.Fatal(err)
			}
			cp, err = c.prepareCollectionClaims(workCtx, cp)
			if err != nil {
				t.Fatal(err)
			}
			c.recordCollectionStopReceipt(producer.ID, &resultCollectionFailure{Reason: RepairFailureCommit, Checkpoint: cp, Cause: context.DeadlineExceeded})
			c.PersistFailureWithClassAndStatusAndOutput(producer.Agent, producer.Goal, producer.ID, "commit deadline exceeded", NeedsHuman, FailureProtocol, TaskBlocked, "original report")
			spec := TodoSpec{Agent: "worker", Desc: "verify the accepted report", ContractID: "verify-report", Phase: PhaseVerify, SideEffect: SideEffectNone,
				Action: &Action{Capability: "verify-report", Type: "verify"}}
			if test.dependency {
				spec.DependsOn = []string{producer.ID}
			}
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
			consumer := items[0]
			consumerStatus := TaskError
			if test.stranded {
				consumerStatus = TaskBlocked
				if test.unrelated {
					c.PersistFailureWithClassAndStatus(consumer.Agent, consumer.Desc, consumer.ID, "independent protocol error", RetryNone, FailureProtocol, TaskBlocked)
				} else if test.dag {
					scheduler := &dagScheduler{coord: c, tasks: []TaskDef{{}, {DependsOn: []int{0}}}, todoItems: []*TodoItem{c.todoItemByID(producer.ID), consumer}, states: []TaskStatus{TaskError, TaskPending}}
					detail, class := scheduler.strandedDependencyDetail(1)
					c.persistStrandedDependent(consumer, detail, class)
				} else {
					if err := c.checkInterruptedDependencies(consumer); err == nil {
						t.Fatal("dependent verifier was admitted before its producer had a result")
					}
				}
				if err := c.phaseWorkflow.observe(c.taskTracker.TodoList().Items()); err == nil {
					t.Fatal("workflow did not fail on the blocked producer")
				}
			} else {
				if err := c.CommitTaskTransition(t.Context(), consumer.ID, TaskPending, TaskInProgress, "verifying", "", nil); err != nil {
					t.Fatal(err)
				}
				c.PersistFailureWithClassAndStatus(consumer.Agent, consumer.Desc, consumer.ID, "report unavailable", RetryNone, FailureVerify, TaskError)
				if err := c.phaseWorkflow.cancel("coordinator", "operator", "close unsuccessful verification"); err == nil {
					t.Fatal("cancellation did not close the phase")
				}
			}
			c.saveCheckpoint()
			c = reopenCollectedWorkflowCoordinator(t, c)
			if c.phaseWorkflow.State() != PhaseFailed {
				t.Fatal("replay lost the failed workflow")
			}
			assessment, err := c.AssessResultCollectionRecovery(t.Context(), producer.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !test.dependency || test.unrelated {
				if assessment.Disposition != RecoveryBlocked {
					t.Fatalf("unrelated failure was accepted: %+v", assessment)
				}
				return
			}
			if assessment.Disposition != RecoveryCommitLocal || assessment.AdditionalTurns != 0 || assessment.ContinuesWorkflow {
				t.Fatalf("prepared report was not eligible for local commit: %+v", assessment.Checks)
			}
			c.executionRunID = "commit-report-after-restart"
			repairCtx := WithResultOnlyRepair(WithSessionResume(t.Context()), producer.ID)
			if err := c.authorizeOperatorResultRepair(repairCtx); err != nil {
				t.Fatal(err)
			}
			worker := scriptWorker(textTurn("must not repeat saved work"))
			c.workerAgentOverride = worker
			if _, err := c.executeTask(repairCtx, taskDefFromTodoItem(c.todoItemByID(producer.ID)), producer.ID); err != nil {
				t.Fatal(err)
			}
			c = reopenCollectedWorkflowCoordinator(t, c)
			if err := c.reconcileRecoveredWorkflow(t.Context()); err != nil {
				t.Fatal(err)
			}
			settled := c.todoItemByID(producer.ID)
			committed := currentResultCollection(settled)
			if worker.Calls() != 0 || settled.Status != TaskDone || committed == nil || committed.Work != cp.Work || committed.TurnsUsed != cp.TurnsUsed ||
				c.todoItemByID(consumer.ID).Status != consumerStatus || c.phaseWorkflow.State() != PhaseFailed {
				t.Fatal("local commit changed completed work, the failed dependent, or the workflow boundary")
			}
			events, err := c.readActiveLineageEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			policy := c.ExecutionPolicySnapshot()
			plan := &ContractRecoveryPlan{MetadataOnly: true, SourceBranch: c.activeBranchID(), SourcePolicy: policy.ConfigurationHash, Policy: policy,
				Retained: 1, Retried: 1, Tasks: []ContractRecoveryTask{{ID: producer.ID, Action: "retain"}, {ID: consumer.ID, Action: "retry"}}}
			tasks := c.taskTracker.TodoList().Items()
			if err := validateUnchangedContractVerifierRetry(tasks, events, plan); err != nil {
				t.Fatalf("current verifier must be eligible after canonical replay: %v", err)
			}
			otherBranch := *plan
			otherBranch.SourceBranch = "unrelated"
			if err := validateUnchangedContractVerifierRetry(tasks, events, &otherBranch); err == nil {
				t.Fatal("a different branch acquired retry authority")
			}
			changed := cloneTodoItem(todoItemByID(tasks, consumer.ID))
			changed.OccurrenceRevision++
			if err := validateUnchangedContractVerifierRetry([]*TodoItem{settled, changed}, events, plan); err == nil {
				t.Fatal("a different verifier occurrence acquired retry authority")
			}
			plan.Tasks[0].Action, plan.Tasks[1].Action = "retry", "retain"
			if err := validateUnchangedContractVerifierRetry(tasks, events, plan); err == nil {
				t.Fatal("retry authority replayed the completed producer")
			}
		})
	}
}

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
	c.PersistFailureWithClassAndStatusAndOutput(c.todoItemByID(id).Agent, "review", id, "rejected collection "+id, NeedsHuman, FailureProtocol, TaskBlocked, "saved report")
	return cp
}

func TestResultCollectionWorkflowRecoveryRepairsSuccessivePhases(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"completed", "same_phase", "earlier_result_changed", "failed_phase_changed"} {
		t.Run(scenario, func(t *testing.T) {
			firstPhase := PhasePrepare
			initial := map[Phase]PhaseResult{}
			if scenario == "same_phase" {
				firstPhase = PhaseVerify
				initial[PhasePrepare] = PhaseResult{Status: PhaseStatusSuccess}
			}
			c, first, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", false, func(spec *TodoSpec) {
				spec.Phase, spec.ContractID = firstPhase, "setup"
			}, func(c *Coordinator) {
				c.session.Config.Workflow = agent.WorkflowConfig{Phases: []string{"prepare", "verify"}}
				c.session.Config.Policies.AllowPhaseSkip = true
				c.session.ContractTasks = []TaskDef{{ID: "setup", Agent: "worker", Phase: firstPhase}, {ID: "review", Agent: "worker", Phase: PhaseVerify}}
				var err error
				c.phaseWorkflow, err = newRuntimeWorkflow(c.session)
				if err != nil {
					t.Fatal(err)
				}
				c.SetSessionData(NewSession())
			})
			if err := c.phaseWorkflow.restore(firstPhase, initial, "", NewRetryState()); err != nil {
				t.Fatal(err)
			}
			originals := make(map[string]*ResultCollectionCheckpoint)
			for index, phase := range []Phase{firstPhase, PhaseVerify} {
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
				if index == 1 && (scenario == "earlier_result_changed" || scenario == "failed_phase_changed") {
					if err == nil || c.phaseWorkflow.State() != PhaseFailed {
						t.Fatal("changed evidence reopened the later failed phase")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				reconcile := c.reconcileRecoveredWorkflow
				if scenario == "same_phase" && index == 0 {
					// Reopen VERIFY with more work still to run in it.
					reconcile = c.reconcileCollectedWorkflow
				}
				if err := reconcile(t.Context()); err != nil {
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
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
	for _, scenario := range []string{"forward", "reverse", "prepare_phase", "changed_peer_result", "missing_peer_grant", "deferred_dispatch", "restart_after_release"} {
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
			var deferredID string
			if scenario == "deferred_dispatch" || scenario == "restart_after_release" {
				pending := spec
				pending.Desc, pending.SideEffect, pending.Execution = "undispatched peer", SideEffectNone, ExecutionContract{}
				ids := c.taskTracker.TodoList().ReserveIDs(1)
				projection, err := taskOccurrenceProjectionFromSpec(pending, ids[0])
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.admitTaskOccurrence(ctx, projection, ids[0], 1); err != nil {
					t.Fatal(err)
				}
				if _, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{pending}, ids); err != nil {
					t.Fatal(err)
				}
				deferredID = ids[0]
				c.PersistFailureWithClassAndStatus(pending.Agent, pending.Desc, deferredID, dispatchDeferredDetail, NeedsHuman, FailurePolicy, TaskBlocked)
				if item := c.todoItemByID(deferredID); !collectionDispatchDeferred(item, first) || len(item.FailureFingerprints) != 0 {
					t.Fatalf("undispatched task was counted as a runtime failure: %+v", item.FailureEvent)
				}
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
				if err != nil || a.Disposition != RecoveryCommitLocal || a.ContinuesWorkflow != (index == 1 && deferredID == "") {
					t.Fatalf("task %s assessment=%+v err=%v", id, a, err)
				}
				if deferredID != "" && !slices.Equal(a.DeferredDispatch, []string{deferredID}) {
					t.Fatalf("preview did not identify the deferred peer: %v", a.DeferredDispatch)
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
				if index == 1 && scenario == "restart_after_release" {
					proof, err := c.completedCollectionWorkflowProof(t.Context())
					if err != nil || proof == nil || !slices.Equal(proof.deferred, []string{deferredID}) {
						t.Fatalf("missing durable release proof: %+v, %v", proof, err)
					}
					metadata := map[string]any{"reset_for_resume": true, "release_deferred_dispatch": true}
					if err := c.CommitTaskTransition(t.Context(), deferredID, TaskBlocked, TaskPending, collectionDispatchResumeDetail, "", metadata); err != nil {
						t.Fatal(err)
					}
					c = reopenCollectedWorkflowCoordinator(t, c)
					if c.phaseWorkflow.State() != PhaseFailed || !collectionDispatchRequeued(c.todoItemByID(deferredID)) {
						t.Fatal("restart lost the release or reopened the failed phase early")
					}
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
				} else if deferredID != "" {
					item := c.todoItemByID(deferredID)
					if c.phaseWorkflow.State() != phase || !collectionDispatchRequeued(item) || worker.Calls() != 0 {
						t.Fatal("recovery did not durably release the first dispatch without executing work")
					}
					c = reopenCollectedWorkflowCoordinator(t, c)
					c.workerAgentOverride = scriptWorker(textTurn("first dispatch after recovery"))
					if n, err := c.ResumeInterruptedTasks(WithSessionResume(t.Context())); err != nil || n != 1 {
						t.Fatalf("ordinary resume dispatched %d tasks: %v", n, err)
					}
					if c.todoItemByID(deferredID).Status != TaskDone || c.phaseWorkflow.State() != next {
						t.Fatalf("ordinary resume left task %s and phase %s", c.todoItemByID(deferredID).Status, c.phaseWorkflow.State())
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
				if item.ID == deferredID {
					if item.Status != TaskDone || item.Retries != 0 {
						t.Fatal("replay counted the deferral as an attempt")
					}
					continue
				}
				cp := currentResultCollection(item)
				if item.Status != TaskDone || cp.Work != original.Work || cp.TurnsUsed != original.TurnsUsed || item.TypedResult.Summary != "saved independent result" {
					t.Fatal("replay changed saved work, claims or collector turns")
				}
			}
		})
	}
}

func TestResultCollectionWorkflowRecoveryRequiresFailureAttribution(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"matching", "optional", "wrong_optional_category", "ordinary", "completed", "structural", "different_task", "different_phase", "different_failure", "repair_progress", "other_unresolved", "parallel_collections", "parallel_wrong_phase", "parallel_unsafe", "extra_failure", "uncertain_effect", "legacy_deferred", "dispatched_peer"} {
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
			case "legacy_deferred", "dispatched_peer":
				fp := NewFailureFingerprint("", item.Agent, stableOperation(item), FailurePolicy, dispatchDeferredDetail)
				peer := &TodoItem{ID: "deferred", Agent: item.Agent, Phase: item.Phase, Status: TaskBlocked,
					SideEffect: SideEffectNone, Verify: item.Verify, VerifySpec: item.VerifySpec, Kind: item.Kind,
					FailureFingerprints: []FailureFingerprint{fp},
					FailureEvent: &FailureEventPayload{TaskID: "deferred", Phase: "coordination", FailureClass: FailurePolicy,
						RetryDisposition: NeedsHuman, Fingerprint: fp.Digest}}
				if kind == "dispatched_peer" {
					peer.ExecutionReceipt = &ExecutionReceipt{Attempt: 1}
				}
				source.Tasks = append(source.Tasks, peer)
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
			wantAllowed := kind == "matching" || kind == "optional" || kind == "ordinary" || kind == "repair_progress" || kind == "parallel_collections" || kind == "legacy_deferred"
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
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	t.Parallel()
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
			repaired := c.todoItemByID(item.ID)
			if !hasCommittedWorkflowCollectionRepair(repaired, c.activeBranchID()) || hasCommittedWorkflowCollectionRepair(repaired, "descendant") {
				t.Fatal("a durable collection grant must prove recovery only on its own branch")
			}
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
