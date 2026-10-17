package team

import (
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

const retryRoutingDiagnostic = "report substituted an original gap: unknown 14/2 (source has 2 gaps)"

func criterionFailureFixture(t *testing.T, class TaskFailureClass) (*dagScheduler, []*TodoItem) {
	t.Helper()
	s, items := dagSchedulerOnFailureClassesFixture(t, []TaskFailureClass{FailureVerify}, class)
	s.coord.sessionData = NewSession()
	s.coord.sessionData.CriterionResults = []CriterionResult{{ID: "validated", State: CriterionFailed}}
	for i := range s.tasks {
		s.tasks[i].Kind = TaskKindOutcome
		s.tasks[i].Advances = []string{"validated"}
		s.tasks[i].MaxRetries = 1
		s.tasks[i].SideEffect, s.tasks[i].Recovery = SideEffectNone, RecoveryRetry
	}
	items[1].FailureEvent.Summary = "report verification rejected"
	items[1].VerifyResult = &VerificationResult{ExitCode: 1, Stderr: retryRoutingDiagnostic}
	// Exercise routing and persistence without launching any external worker.
	s.dispatchStopped = true
	return s, items
}

func TestFailureEdgeOwnsCriterionRouting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		class     TaskFailureClass
		exhausted bool
		reset     bool
	}{
		{"verification repairs producer and dependent", FailureVerify, false, true},
		{"class denial cannot fall back to criterion", FailureEnvironment, false, false},
		{"exhausted edge cannot fall back to criterion", FailureVerify, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, items := criterionFailureFixture(t, tc.class)
			if tc.exhausted {
				s.retries[1] = 1
			}
			s.handleEvent(t.Context(), agentTaskResult{idx: 1, todoID: items[1].ID, agentName: items[1].Agent, err: errors.New("audit rejected")})
			if s.criterionRetryUsed != 0 {
				t.Fatal("criterion routing took over an explicit failure edge")
			}
			if tc.reset {
				if s.states[0] != TaskPending || s.states[1] != TaskPending || s.retries[1] != 1 || items[0].RemediationContext == nil || items[0].RemediationContext.Verification.Stderr != retryRoutingDiagnostic {
					t.Fatalf("failure edge lost its wave or evidence: states=%v retries=%v remediation=%+v", s.states, s.retries, items[0].RemediationContext)
				}
			} else if s.states[0] != TaskDone || s.states[1] != TaskError || items[0].RemediationContext != nil {
				t.Fatalf("denied/exhausted failure restarted work: states=%v remediation=%+v", s.states, items[0].RemediationContext)
			}
		})
	}
}

func TestCriterionFallbackCarriesFailureEvidence(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry completed repair", true: "start pending repair"}[pending], func(t *testing.T) {
			s, items := criterionFailureFixture(t, FailureVerify)
			s.tasks[1].OnFailure = nil
			if pending {
				items[0].Status, s.states[0] = TaskPending, TaskPending
			}
			s.handleEvent(t.Context(), agentTaskResult{idx: 1, todoID: items[1].ID, agentName: items[1].Agent, err: errors.New("criterion check rejected")})
			if s.criterionRetryUsed != 1 || s.states[0] != TaskPending || items[0].RemediationContext == nil || items[0].RemediationContext.SourceTaskID != items[1].ID || items[0].RemediationContext.Verification.Stderr != retryRoutingDiagnostic {
				t.Fatalf("criterion fallback lost failure evidence: states=%v context=%+v", s.states, items[0].RemediationContext)
			}
			if pending && items[0].Retries != 0 {
				t.Fatal("starting a pending repair consumed a retry")
			}
		})
	}
}

func TestCriterionCannotRestartAnExhaustedOrOwnedTarget(t *testing.T) {
	for _, ownEdge := range []bool{false, true} {
		s, items := criterionFailureFixture(t, FailureVerify)
		if ownEdge {
			s.tasks[0].OnFailure = new(0)
		} else {
			// A previous DAG reset already used this occurrence's retry, even
			// though criterionRetryCounts is zero in this scheduler.
			items[0].Retries = 1
		}
		if s.routeCriterionRetry(t.Context(), 1, []string{"validated"}) || s.criterionRetryUsed != 0 {
			t.Fatal("criterion retry bypassed the target's declared recovery policy")
		}
	}
}

func TestFailureDiagnosticSurvivesSQLiteReplayAndWorkerDispatch(t *testing.T) {
	s, items := criterionFailureFixture(t, FailureVerify)
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-retry", "session-retry")
	if err != nil {
		t.Fatal(err)
	}
	s.coord.eventStore, s.coord.executionRunID = store, "run-retry"
	s.handleEvent(t.Context(), agentTaskResult{idx: 1, todoID: items[1].ID, agentName: items[1].Agent, err: errors.New("audit rejected")})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewEventStore(workspace, "run-retry", "session-retry")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	producer := todoItemByID(replayed, items[0].ID)
	if producer == nil || producer.RemediationContext == nil || producer.RemediationContext.Verification.Stderr != retryRoutingDiagnostic {
		t.Fatalf("durable reset lost diagnostic: %+v", producer)
	}
	// Reconstruct a new worker runtime from replayed tasks, then capture the
	// actual model input. Rendering a synthetic remediation string alone
	// would not prove dispatch reads the persisted occurrence.
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "retry-replay", Timeout: 30}, Agents: map[string]*agent.AgentDef{
			"coder": {Name: "coder", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
		}},
		sessionData: NewSession(), taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {},
		taskCache: newDefaultTaskCache(taskCacheDependencies{}), executionRunID: "run-retry",
	}
	c.taskTracker.TodoList().Restore(replayed)
	var calls int
	var prompts []string
	c.workerAgentOverride = &scriptedRepairAgent{calls: &calls, prompts: &prompts, onCall: func(int) {
		c.storeSubmittedTaskResult(producer.ID, &TaskResult{TaskID: producer.ID, Agent: "coder", Status: TaskResultStatusSuccess, Summary: "corrected the report", Source: "submitted"})
	}}
	_, err = c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), taskDefFromTodoItem(producer), producer.ID)
	if err != nil || calls != 1 || len(prompts) != 1 || !strings.Contains(prompts[0], retryRoutingDiagnostic) {
		t.Fatalf("restored worker did not receive rejection: calls=%d err=%v prompts=%v", calls, err, prompts)
	}
}

func TestFailedResetDoesNotPublishRemediation(t *testing.T) {
	s, items := criterionFailureFixture(t, FailureVerify)
	s.coord.SetEventJournal(failingJournal{err: errors.New("reset append failed")})
	err := s.resetWaveWithRemediation(t.Context(), 0, buildRemediationContext(items[1].ID, items[1]))
	if err == nil || items[0].Status != TaskDone || items[0].Retries != 0 || items[0].RemediationContext != nil {
		t.Fatalf("failed durable append modified live state: err=%v item=%+v", err, items[0])
	}
}

func TestDeferredResetKeepsRemediationUntilWorkerSettles(t *testing.T) {
	s, items := criterionFailureFixture(t, FailureVerify)
	s.states[0], items[0].Status = TaskInProgress, TaskInProgress
	s.states[1] = TaskError
	remediation := buildRemediationContext(items[1].ID, items[1])
	if err := s.resetWaveWithRemediation(t.Context(), 0, remediation); err != nil {
		t.Fatal(err)
	}
	if !s.needsReset[0] || items[0].RemediationContext != nil {
		t.Fatal("in-flight reset must defer publication until the worker settles")
	}
	s.handleEvent(t.Context(), agentTaskResult{idx: 0, todoID: items[0].ID, agentName: items[0].Agent})
	if s.states[0] != TaskPending || s.needsReset[0] || s.pendingRemediation[0] != nil || items[0].RemediationContext == nil || items[0].RemediationContext.Verification.Stderr != retryRoutingDiagnostic {
		t.Fatalf("deferred reset lost its diagnostic: item=%+v", items[0])
	}
}
