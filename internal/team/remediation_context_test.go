package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestBuildRemediationContextExtractsAndRedactsSourceEvidence(t *testing.T) {
	source := &TodoItem{
		ID:      "task-2",
		Agent:   "reviewer",
		Retries: 1, // attempt 2
		FailureEvent: &FailureEventPayload{
			FailureClass: FailureVerify,
			Summary:      "a must-fix finding was reported",
		},
		TypedResult: &TaskResult{
			Status:  "failed",
			Summary: "typed result summary (should not override FailureEvent summary)",
			Findings: []Finding{
				{Category: "correctness", Summary: "off-by-one in retry counter", Detail: "Authorization: Bearer abcdef1234567890 leaked in log"},
			},
		},
		VerifyResult: &VerificationResult{Command: "go test ./...", ExitCode: 1, Stderr: "FAIL"},
	}

	rc := buildRemediationContext("task-2", source)
	if rc == nil {
		t.Fatal("expected non-nil remediation context")
	}
	if rc.SourceTaskID != "task-2" || rc.SourceAgent != "reviewer" || rc.SourceAttempt != 2 {
		t.Fatalf("unexpected source identity: %+v", rc)
	}
	if rc.FailureClass != FailureVerify {
		t.Fatalf("expected FailureClass from FailureEvent, got %q", rc.FailureClass)
	}
	if rc.Summary != "a must-fix finding was reported" {
		t.Fatalf("expected FailureEvent.Summary to take precedence, got %q", rc.Summary)
	}
	if rc.Status != "failed" {
		t.Fatalf("expected Status from TypedResult, got %q", rc.Status)
	}
	if len(rc.Findings) != 1 || rc.Findings[0].Category != "correctness" {
		t.Fatalf("expected findings carried over, got %+v", rc.Findings)
	}
	if strings.Contains(rc.Findings[0].Detail, "abcdef1234567890") {
		t.Fatalf("expected secret redacted from finding detail, got %q", rc.Findings[0].Detail)
	}
	if rc.Verification == nil || rc.Verification.ExitCode != 1 {
		t.Fatalf("expected verification result carried over, got %+v", rc.Verification)
	}
}

func TestBuildRemediationContextNilSource(t *testing.T) {
	if rc := buildRemediationContext("task-1", nil); rc != nil {
		t.Fatalf("expected nil remediation context for nil source, got %+v", rc)
	}
}

func TestAppendRemediationContextRendersBoundedEvidence(t *testing.T) {
	rc := &RemediationContext{
		SourceTaskID:  "task-2",
		SourceAgent:   "reviewer",
		SourceAttempt: 1,
		FailureClass:  FailureVerify,
		Status:        "failed",
		Summary:       "concrete must-fix finding",
		Findings:      []Finding{{Category: "correctness", Summary: "regression", Detail: "see file.go:10"}},
		Verification:  &VerificationResult{Command: "go test ./...", ExitCode: 1, Stderr: "FAIL: TestFoo"},
	}
	var b strings.Builder
	appendRemediationContext(&b, rc)
	out := b.String()
	for _, want := range []string{"task-2", "reviewer", string(FailureVerify), "concrete must-fix finding", "regression", "see file.go:10", "go test ./...", "FAIL: TestFoo"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered remediation context missing %q:\n%s", want, out)
		}
	}
}

func TestAppendRemediationContextNilIsNoop(t *testing.T) {
	var b strings.Builder
	appendRemediationContext(&b, nil)
	if b.Len() != 0 {
		t.Fatalf("expected no output for nil remediation context, got %q", b.String())
	}
}

// TestDAGSchedulerAttachesRemediationContextOnSemanticReset is the central
// spec.md §9.3 regression: a semantic on_failure reset must attach the
// source task's canonical evidence to the reset ancestor, so the coder's
// next dispatch can read it back (coordinator_task_run.go).
func TestDAGSchedulerAttachesRemediationContextOnSemanticReset(t *testing.T) {
	s, items := dagSchedulerOnFailureClassesFixture(t, []TaskFailureClass{FailureVerify}, FailureVerify)
	items[1].TypedResult = &TaskResult{Status: "failed", Findings: []Finding{{Category: "correctness", Summary: "required check failed"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: "verifier", todoID: items[1].ID, err: errors.New("required check failed")})
	// Drain the relaunched coder goroutine before reading any TodoItem field
	// it may concurrently mutate (executeTask -> PersistFailure ->
	// CommitTaskTransition) — s.states/s.retries are scheduler-owned and safe
	// to read immediately, but items[]/TodoItem fields are not.
	drainRelaunchedTask(t, s)

	rc := items[0].RemediationContext
	if rc == nil {
		t.Fatal("expected the coder ancestor to receive a remediation context")
	}
	if rc.SourceTaskID != items[1].ID || rc.SourceAgent != "verifier" || rc.FailureClass != FailureVerify {
		t.Fatalf("unexpected remediation context: %+v", rc)
	}
	if len(rc.Findings) != 1 || rc.Findings[0].Summary != "required check failed" {
		t.Fatalf("expected verifier's finding to be carried into the remediation context, got %+v", rc.Findings)
	}
}

// TestDAGSchedulerSelfLoopDoesNotAttachRemediationContext confirms a task
// that resets itself (on_failure targeting its own index, the same-task
// retry mechanism buildRetryContextWithSubmittedResult already covers) does
// not also produce a self-referential remediation context.
func TestDAGSchedulerSelfLoopDoesNotAttachRemediationContext(t *testing.T) {
	coord := &Coordinator{
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		maxConcurrent:   1,
	}
	tasks := []TaskDef{
		{Agent: "verifier", OnFailure: intPtr(0), MaxRetries: 2, OnFailureClasses: []TaskFailureClass{FailureVerify}},
	}
	items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "verifier", Desc: "verify"}})
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskInProgress, "running")
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskError, "terminal failure")
	items[0].FailureEvent = &FailureEventPayload{FailureClass: FailureVerify}

	s := newDAGScheduler(coord, tasks, items, nil)
	s.states[0], s.inProgress = TaskInProgress, 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.handleEvent(ctx, agentTaskResult{idx: 0, agentName: "verifier", todoID: items[0].ID, err: errors.New("required check failed")})
	// See the comment in TestBuildRemediationContextExtractsAndRedactsSourceEvidence's
	// sibling above: drain before reading the TodoItem field the relaunched
	// goroutine may concurrently mutate.
	drainRelaunchedTask(t, s)

	if items[0].RemediationContext != nil {
		t.Fatalf("self-loop on_failure must not attach a remediation context, got %+v", items[0].RemediationContext)
	}
}

// TestReduceToTodoListReconstructsRemediationContext pins spec.md §9.3's
// durability requirement: remediation context must survive event replay
// (e.g. after a Hufu process restart), not just live in-memory execution.
func TestReduceToTodoListReconstructsRemediationContext(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"id":     "coder-1",
		"status": "pending",
		"remediation_context": map[string]any{
			"source_task_id": "verifier-1",
			"source_agent":   "verifier",
			"failure_class":  "verification",
			"summary":        "required check failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	todos := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "coder-1", Payload: payload}})
	if len(todos) != 1 {
		t.Fatalf("expected 1 todo item, got %d", len(todos))
	}
	rc := todos[0].RemediationContext
	if rc == nil {
		t.Fatal("remediation context was lost during event reduction")
	}
	if rc.SourceTaskID != "verifier-1" || rc.SourceAgent != "verifier" || rc.FailureClass != FailureVerify || rc.Summary != "required check failed" {
		t.Fatalf("reduced remediation context = %#v", rc)
	}
}
