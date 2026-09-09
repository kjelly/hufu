package team

import (
	"context"
	"errors"
	"testing"
	"time"
)

// This file implements the deterministic DAG-level portion of spec.md §17's
// mandatory test matrix for the .agent-teams/hufu-coding/ batch shape:
//
//	0: sa        (no deps)
//	1: coder     depends_on [0]
//	2: verifier  depends_on [0,1],     on_failure -> 1, on-failure-classes:[verification, semantic_rejection]
//	3: reviewer  depends_on [0,1,2],   on_failure -> 1, on-failure-classes:[verification, semantic_rejection]
//	4: final-sa  depends_on [0,1,2,3], on_failure -> 1, on-failure-classes:[verification, semantic_rejection]
//
// depends_on lists every earlier task, not only the immediately preceding
// one, matching coordinator.md: Coordinator.dependencyResultsForTask only
// ever exposes a task's *direct* dependencies' results to its compiled
// prompt (no transitive walk), so verifier/reviewer/final-sa each need SA's
// contract listed explicitly, and final-sa additionally needs verifier's and
// reviewer's own results. Widening depends_on does not change resetWave's
// reset set (BFS over revDeps still visits the same {1,2,3,4} whether coder's
// dependents list it directly or reach it transitively) or launchReady's
// readiness gating (task 0 is done permanently before any of 1-4 ever run),
// only which typed results a task's prompt receives.
//
// exactly mirroring .agent-teams/hufu-coding/team.yaml's static contracts. It
// drives dagScheduler.handleEvent directly (the same technique as
// TestDAGSchedulerRoutesSuccessfulNoProgressWithBoundedBudget and this
// package's dag_scheduler_failure_class_test.go) rather than real LLM/Codex
// execution, so it can assert the new failure-class-aware routing (Phase 2)
// and remediation-context propagation (Phase 3) mechanisms across the full
// five-stage topology without a live provider. Items G/H/L/M/N of the matrix
// are already covered by existing generic suites (subagent_binding_test.go,
// resume_test.go/rca20_task_occurrence_test.go, budget_manager_test.go/
// attempt_budget_test.go, workspace_scope_test.go/workset_security_test.go)
// reused as-is; item I is split between a config-lint test
// (dag_scheduler_failure_class_test.go's siblings do not cover it — see
// codex_process_test.go for the existing preflight coverage) and the opt-in
// live smoke suite, since it is inherently an external-binary property.

func hufuCodingWorkflowFixture(t *testing.T) (*dagScheduler, []*TodoItem) {
	t.Helper()
	coord := &Coordinator{
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		sessionData:     NewSession(),
		taskResultCache: make(map[string][]cachedTaskEntry),
		maxConcurrent:   1,
	}
	classes := []TaskFailureClass{FailureVerify, FailureSemanticRejection}
	tasks := []TaskDef{
		{Agent: "sa"},
		{Agent: "coder", DependsOn: []int{0}},
		{Agent: "verifier", DependsOn: []int{0, 1}, OnFailure: intPtr(1), MaxRetries: 4, OnFailureClasses: classes},
		{Agent: "reviewer", DependsOn: []int{0, 1, 2}, OnFailure: intPtr(1), MaxRetries: 4, OnFailureClasses: classes},
		{Agent: "final-sa", DependsOn: []int{0, 1, 2, 3}, OnFailure: intPtr(1), MaxRetries: 2, OnFailureClasses: classes},
	}
	items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "sa", Desc: "SA_ANALYZE"},
		{Agent: "coder", Desc: "CODER_IMPLEMENT"},
		{Agent: "verifier", Desc: "VERIFY_IMPLEMENTATION"},
		{Agent: "reviewer", Desc: "REVIEW_CODE"},
		{Agent: "final-sa", Desc: "FINAL_SA_GATE"},
	})
	s := newDAGScheduler(coord, tasks, items, nil)
	return s, items
}

// hufuCodingAdvance simulates one task occurrence's terminal outcome —
// success (taskErr nil) or a classified failure — and lets the real
// dagScheduler.handleEvent decide readiness/routing exactly as production
// does. A non-nil taskErr seeds a terminal TodoItem status + FailureEvent
// first, matching how PersistFailureWithClass* always persists both
// together before the error ever reaches the scheduler (see
// dagSchedulerOnFailureClassesFixture's comment in
// dag_scheduler_failure_class_test.go).
//
// handleEvent's own launchReady call may start a new goroutine for whatever
// task just became ready (dag_scheduler.go's launchReady/runTask). The
// cancelled ctx these tests use makes that goroutine fail fast rather than
// do any real work, but it still runs concurrently with this function's
// caller until it does — so this drains its single completion event before
// returning, fully serializing every mutation the test makes against the
// shared TodoItem/Coordinator state the same way dagScheduler.run()'s own
// single-consumer event loop would. A timeout (rather than an exact-count
// assertion) tolerates the stages that legitimately start nothing new (the
// batch's last task succeeding, or a suppressed on_failure edge).
func hufuCodingAdvance(t *testing.T, ctx context.Context, s *dagScheduler, items []*TodoItem, idx int, taskErr error, class TaskFailureClass) {
	t.Helper()
	if taskErr != nil {
		s.coord.taskTracker.TodoList().UpdateStatus(items[idx].ID, TaskInProgress, "running")
		s.coord.taskTracker.TodoList().UpdateStatus(items[idx].ID, TaskError, "terminal failure")
		// retryDispositionForTerminalTestClass (dag_scheduler_failure_class_test.go)
		// mirrors what disposition.go's real DecideRecovery would have
		// persisted alongside this class for a budget-exhausted attempt —
		// selfHealEligible reads the disposition, not the class.
		items[idx].FailureEvent = &FailureEventPayload{FailureClass: class, RetryDisposition: retryDispositionForTerminalTestClass(class)}
	}
	s.handleEvent(ctx, agentTaskResult{idx: idx, agentName: s.tasks[idx].Agent, todoID: items[idx].ID, err: taskErr})
	select {
	case <-s.eventCh:
	case <-time.After(2 * time.Second):
	}
}

// TestHufuCodingWorkflowCleanPath is matrix item A: every stage succeeds in
// order and no on_failure edge ever fires.
func TestHufuCodingWorkflowCleanPath(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	for idx := 0; idx < 5; idx++ {
		hufuCodingAdvance(t, ctx, s, items, idx, nil, "")
	}
	for i, want := range []TaskStatus{TaskDone, TaskDone, TaskDone, TaskDone, TaskDone} {
		if s.states[i] != want {
			t.Fatalf("states[%d] = %s, want %s", i, s.states[i], want)
		}
	}
	for i, item := range items {
		if item.RemediationContext != nil {
			t.Fatalf("clean path must never attach remediation context, got items[%d]=%+v", i, item.RemediationContext)
		}
	}
	cancel()
}

// TestHufuCodingWorkflowVerifierSemanticFailureResetsCoderAndDownstream is
// matrix item B, exercising the on-failure-classes allowlist's `verification`
// member directly (the allowlist is [verification, semantic_rejection] —
// `verification` is kept as defense-in-depth per team.yaml's own comment;
// team.yaml ships no verify-spec that currently produces this class). See
// TestHufuCodingWorkflowVerifierGenuineFailedReportResetsCoderAndDownstream
// below for the path hufu-coding's verifier.md/team.yaml actually use today
// (status:failed -> FailureClass=execution ->
// isGenuineWorkerReportedFailure). Both paths must reset the coder and every
// downstream task in its wave, carrying durable remediation evidence.
func TestHufuCodingWorkflowVerifierSemanticFailureResetsCoderAndDownstream(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	items[2].TypedResult = &TaskResult{Status: "failed", Findings: []Finding{{Category: "correctness", Summary: "go test ./... failed"}}}
	hufuCodingAdvance(t, ctx, s, items, 2, errors.New("required check failed"), FailureVerify)

	if s.states[1] != TaskInProgress {
		t.Fatalf("expected coder to be reset and relaunched, states[1]=%s", s.states[1])
	}
	if s.states[2] != TaskPending || s.states[3] != TaskPending || s.states[4] != TaskPending {
		t.Fatalf("expected the whole downstream wave reset to Pending, got states=%v", s.states)
	}
	rc := items[1].RemediationContext
	if rc == nil || rc.SourceTaskID != items[2].ID || rc.SourceAgent != "verifier" || rc.FailureClass != FailureVerify {
		t.Fatalf("coder did not receive verifier's remediation evidence: %+v", rc)
	}
	if len(rc.Findings) != 1 || rc.Findings[0].Summary != "go test ./... failed" {
		t.Fatalf("remediation evidence missing verifier's finding: %+v", rc.Findings)
	}
	if s.retries[2] != 1 {
		t.Fatalf("expected verifier's on_failure budget consumed, retries=%v", s.retries)
	}

	// Coder redoes the work; the whole downstream wave reruns and the batch
	// completes only after fresh verification passes (matrix B items 6-8).
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 3, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 4, nil, "")
	for i, want := range []TaskStatus{TaskDone, TaskDone, TaskDone, TaskDone, TaskDone} {
		if s.states[i] != want {
			t.Fatalf("after remediation, states[%d] = %s, want %s", i, s.states[i], want)
		}
	}
	cancel()
}

// TestHufuCodingWorkflowVerifierGenuineFailedReportResetsCoderAndDownstream
// is the full-DAG counterpart to
// TestHufuCodingGenuineFailedReportAuthorizesReset
// (hufu_coding_classification_test.go): it drives the exact raw class real
// production persists for hufu-coding's verifier.md (status:failed ->
// FailureClass=execution via coordinator_task_run.go's
// withFailureClassOverride, since team.yaml ships no verify-spec — see
// TestHufuCodingNoVerifySpecOnSemanticRoles), not the FailureVerify class the
// sibling test above uses directly. The reset must still fire, because
// dagScheduler.effectiveFailureClassForTodo canonicalizes this genuine
// self-report to FailureSemanticRejection — which team.yaml's
// on-failure-classes names explicitly — before checking the allowlist.
func TestHufuCodingWorkflowVerifierGenuineFailedReportResetsCoderAndDownstream(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	items[2].TypedResult = &TaskResult{Status: TaskResultStatusFailed, Source: "submitted", Findings: []Finding{{Category: "correctness", Summary: "go test ./... failed"}}}
	hufuCodingAdvance(t, ctx, s, items, 2, errors.New("required check failed"), FailureExecution)

	if s.states[1] != TaskInProgress {
		t.Fatalf("expected coder to be reset and relaunched, states[1]=%s", s.states[1])
	}
	if s.states[2] != TaskPending || s.states[3] != TaskPending || s.states[4] != TaskPending {
		t.Fatalf("expected the whole downstream wave reset to Pending, got states=%v", s.states)
	}
	rc := items[1].RemediationContext
	if rc == nil || rc.SourceTaskID != items[2].ID || rc.SourceAgent != "verifier" || rc.FailureClass != FailureSemanticRejection {
		t.Fatalf("coder did not receive verifier's remediation evidence with the canonicalized class: %+v", rc)
	}
	if len(rc.Findings) != 1 || rc.Findings[0].Summary != "go test ./... failed" {
		t.Fatalf("remediation evidence missing verifier's finding: %+v", rc.Findings)
	}
}

// TestHufuCodingWorkflowVerifierInfraFailureWithNoStoredResultDoesNotReset
// is the negative complement: a verifier task that fails with the exact same
// FailureClass=execution but never stored a TypedResult at all (a real
// protocol/infra abort, not a self-report) must self-heal instead of
// resetting the coder — the class alone is identical to the test above, so
// only isGenuineWorkerReportedFailure's TypedResult check tells them apart.
func TestHufuCodingWorkflowVerifierInfraFailureWithNoStoredResultDoesNotReset(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, errors.New("app-server disconnected"), FailureExecution)

	if s.states[1] != TaskDone {
		t.Fatalf("infra failure with no stored result must not reset the coder ancestor: states=%v", s.states)
	}
	if items[1].RemediationContext != nil {
		t.Fatalf("infra failure must not synthesize a fake finding, got %+v", items[1].RemediationContext)
	}
	if s.states[2] != TaskInProgress {
		t.Fatalf("expected the verifier itself to be retried in place, states[2]=%s", s.states[2])
	}
}

// TestHufuCodingWorkflowReviewerMustFixFindingResetsCoder is matrix item C:
// the reviewer's own must-fix finding (FailureVerify, exercising the
// allowlist path directly — see the comment on
// TestHufuCodingWorkflowVerifierSemanticFailureResetsCoderAndDownstream)
// resets the coder, carries the finding forward, and the verifier reruns
// even though it was previously green.
func TestHufuCodingWorkflowReviewerMustFixFindingResetsCoder(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "") // verifier initially green
	items[3].TypedResult = &TaskResult{Status: "failed", Findings: []Finding{{Category: "correctness", Summary: "unchecked error return at handler.go:42"}}}
	hufuCodingAdvance(t, ctx, s, items, 3, errors.New("must-fix finding"), FailureVerify)

	if s.states[1] != TaskInProgress {
		t.Fatalf("expected coder to be reset and relaunched, states[1]=%s", s.states[1])
	}
	if s.states[2] != TaskPending {
		t.Fatalf("expected the previously-green verifier to be reset for a fresh run, states[2]=%s", s.states[2])
	}
	rc := items[1].RemediationContext
	if rc == nil || rc.SourceTaskID != items[3].ID || rc.SourceAgent != "reviewer" || len(rc.Findings) != 1 {
		t.Fatalf("coder did not receive reviewer's remediation evidence: %+v", rc)
	}
	if rc.Findings[0].Summary != "unchecked error return at handler.go:42" {
		t.Fatalf("remediation evidence missing reviewer's finding text: %+v", rc.Findings)
	}

	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "") // verifier reruns against the new diff
	hufuCodingAdvance(t, ctx, s, items, 3, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 4, nil, "") // final-sa only runs after clean review
	if s.states[4] != TaskDone {
		t.Fatalf("expected final-sa to run and complete after clean review, states[4]=%s", s.states[4])
	}
	cancel()
}

// TestHufuCodingWorkflowReviewerInfraFailureDoesNotResetCoder is matrix item
// D: a reviewer provider/runtime failure (FailureExecution — not in the
// contract's on-failure-classes allowlist) must retry the reviewer itself,
// must not reset the coder, must not touch the workspace/verifier's prior
// green state, and must not be treated as a code-review finding.
func TestHufuCodingWorkflowReviewerInfraFailureDoesNotResetCoder(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 3, errors.New("app-server disconnected"), FailureExecution)

	if s.states[1] != TaskDone {
		t.Fatalf("reviewer infra failure must not reset the coder, states[1]=%s", s.states[1])
	}
	if s.states[2] != TaskDone {
		t.Fatalf("reviewer infra failure must not reset the previously-green verifier, states[2]=%s", s.states[2])
	}
	if items[1].RemediationContext != nil {
		t.Fatalf("reviewer infra failure must not synthesize a fake code-review finding, got %+v", items[1].RemediationContext)
	}
	if s.states[3] != TaskInProgress {
		t.Fatalf("expected the reviewer itself to be retried in place, states[3]=%s", s.states[3])
	}
	if s.retries[3] != 1 {
		t.Fatalf("expected the reviewer's own retry budget consumed, retries=%v", s.retries)
	}
	cancel()
}

// TestHufuCodingWorkflowReviewerInfraFailureBudgetExhaustedBlocksNotRejects
// extends matrix item D: once the reviewer's own retry budget is exhausted
// on repeated infrastructure failures, the run must leave the reviewer
// blocked/errored rather than ever falling through to a coder reset.
func TestHufuCodingWorkflowReviewerInfraFailureBudgetExhaustedBlocksNotRejects(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	s.tasks[3].MaxRetries = 1 // exhaust after a single infra retry, to keep the test short
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 3, errors.New("app-server disconnected"), FailureExecution) // consumes the sole retry
	hufuCodingAdvance(t, ctx, s, items, 3, errors.New("app-server disconnected"), FailureExecution) // budget now exhausted

	if s.states[1] != TaskDone || s.states[2] != TaskDone {
		t.Fatalf("exhausted reviewer infra budget must still never reset coder/verifier, states=%v", s.states)
	}
	if s.states[3] != TaskError {
		t.Fatalf("expected the reviewer to remain in its terminal error state (blocked/partial), got %s", s.states[3])
	}
	cancel()
}

// TestHufuCodingWorkflowFinalSAFinalRejectionResetsFullWave is matrix item E:
// final-SA determines the request is still incomplete even though
// verification/review were green; the coder receives final-SA's evidence
// and the entire downstream wave (verifier, reviewer, final-SA) reruns —
// the prior green review is not treated as permanent proof.
func TestHufuCodingWorkflowFinalSAFinalRejectionResetsFullWave(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 3, nil, "")
	items[4].TypedResult = &TaskResult{Status: "failed", Summary: "requested behavior still missing: no CLI flag was added"}
	hufuCodingAdvance(t, ctx, s, items, 4, errors.New("request incomplete"), FailureVerify)

	if s.states[1] != TaskInProgress {
		t.Fatalf("expected coder to be reset and relaunched, states[1]=%s", s.states[1])
	}
	if s.states[2] != TaskPending || s.states[3] != TaskPending || s.states[4] != TaskPending {
		t.Fatalf("expected the full downstream wave (verifier, reviewer, final-sa) reset, got states=%v", s.states)
	}
	rc := items[1].RemediationContext
	if rc == nil || rc.SourceTaskID != items[4].ID || rc.SourceAgent != "final-sa" {
		t.Fatalf("coder did not receive final-SA's remediation evidence: %+v", rc)
	}
	cancel()
}

// TestHufuCodingWorkflowFinalSAInfraFailureDoesNotResetCoder is matrix item
// F: a final-SA provider/infrastructure failure retries final-SA itself and
// must not reset the coder or any already-green upstream task.
func TestHufuCodingWorkflowFinalSAInfraFailureDoesNotResetCoder(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 1, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 2, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 3, nil, "")
	hufuCodingAdvance(t, ctx, s, items, 4, errors.New("final-sa provider timeout"), FailureTimeout)

	if s.states[1] != TaskDone || s.states[2] != TaskDone || s.states[3] != TaskDone {
		t.Fatalf("final-sa infra failure must not reset any upstream task, states=%v", s.states)
	}
	if items[1].RemediationContext != nil {
		t.Fatalf("final-sa infra failure must not synthesize remediation evidence, got %+v", items[1].RemediationContext)
	}
	if s.states[4] != TaskInProgress {
		t.Fatalf("expected final-sa itself to be retried in place, states[4]=%s", s.states[4])
	}
	cancel()
}

// TestHufuCodingWorkflowSAFailureNeverLetsCoderStart is the direct
// regression for a prior review finding (SA could report
// completed_with_gaps for a blocking requirement ambiguity, and since that
// status reaches TaskDone, the coder could start against an unconfirmed
// contract). SA has no on_failure edge of its own — nothing resets into it,
// and it resets nothing else — so the *only* mechanism protecting the coder
// is the ordinary DAG dependency gate: coder (depends_on: [0]) cannot become
// ready until SA reaches TaskDone. sa.md now never uses completed_with_gaps
// (partial/blocked for a genuine ambiguity instead), so this failure path is
// what actually happens for a request SA could not confidently confirm; the
// coder must simply never launch.
func TestHufuCodingWorkflowSAFailureNeverLetsCoderStart(t *testing.T) {
	s, items := hufuCodingWorkflowFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.inProgress = 1
	hufuCodingAdvance(t, ctx, s, items, 0, errors.New("requirement is ambiguous"), FailureExecution)

	if s.states[0] != TaskError {
		t.Fatalf("expected SA to remain in its terminal state, got %s", s.states[0])
	}
	if s.states[1] != TaskPending {
		t.Fatalf("expected the coder to never become ready while SA has not reached TaskDone, got %s", s.states[1])
	}
}
