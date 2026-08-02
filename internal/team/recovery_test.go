package team

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func assertRecoveryFailureEvent(t *testing.T, item *TodoItem, class TaskFailureClass, disposition RetryDisposition) {
	t.Helper()
	if item == nil || item.FailureEvent == nil {
		t.Fatalf("recovery task missing FailureEvent: %#v", item)
	}
	if item.FailureEvent.FailureClass != class || item.FailureEvent.Phase == "" || item.FailureEvent.RetryDisposition != disposition {
		t.Fatalf("recovery FailureEvent = %#v, want class=%s disposition=%s", item.FailureEvent, class, disposition)
	}
}

func TestDefaultRecoveryPolicy(t *testing.T) {
	tests := []struct {
		class      SideEffectClass
		unattended bool
		wantPolicy RecoveryPolicy
	}{
		{SideEffectNone, false, RecoveryRetry},
		{SideEffectNone, true, RecoveryRetry},
		{SideEffectWorkspaceWrite, false, RecoveryRetry},
		{SideEffectWorkspaceWrite, true, RecoveryRetry},
		{SideEffectExternalWrite, false, RecoveryReconcile},
		{SideEffectExternalWrite, true, RecoveryManual},
		{SideEffectInfraMutation, false, RecoveryManual},
		{SideEffectInfraMutation, true, RecoveryManual},
		{SideEffectCredential, false, RecoveryManual},
		{SideEffectCredential, true, RecoveryManual},
		{"", false, RecoveryRetry},
		{"", true, RecoveryRetry},
	}

	for _, tt := range tests {
		got := DefaultRecoveryPolicy(tt.class, tt.unattended)
		if got != tt.wantPolicy {
			t.Errorf("DefaultRecoveryPolicy(%q, unattended=%v) = %q, want %q", tt.class, tt.unattended, got, tt.wantPolicy)
		}
	}
}

func TestResolveRecoveryPolicy(t *testing.T) {
	// Explicit policy should override class-based inference
	if got := ResolveRecoveryPolicy(RecoveryRetry, SideEffectInfraMutation, false); got != RecoveryRetry {
		t.Errorf("expected explicit Retry to override InfraMutation, got %q", got)
	}
	if got := ResolveRecoveryPolicy(RecoveryManual, SideEffectNone, false); got != RecoveryManual {
		t.Errorf("expected explicit Manual to override None, got %q", got)
	}
	// Fallback when explicit is empty
	if got := ResolveRecoveryPolicy("", SideEffectInfraMutation, false); got != RecoveryManual {
		t.Errorf("expected fallback to Manual for InfraMutation, got %q", got)
	}
}

func TestResumeInterruptedTasks_InfraMutationBlocked(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:         "1",
			Agent:      "a",
			Desc:       "create infrastructure vm",
			Status:     TaskInProgress,
			SideEffect: SideEffectInfraMutation,
		},
	})

	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-driven for infra_mutation, got %d", n)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("task status should be blocked, got %s", item.Status)
	}
	if !strings.Contains(item.Detail, "manual intervention") {
		t.Errorf("expected manual intervention note in detail, got %q", item.Detail)
	}
	assertRecoveryFailureEvent(t, item, FailurePolicy, NeedsHuman)
}

func TestResumeInterruptedTasks_AllowsReplayFalseBlocked(t *testing.T) {
	c := newBudgetCoordinator(t)
	allowsReplay := false
	c.taskTracker.TodoList().Restore([]*TodoItem{{
		ID: "replay-blocked", Agent: "a", Desc: "non-replayable retry", Status: TaskInProgress,
		Recovery: RecoveryRetry, Execution: ExecutionContract{AllowsReplay: &allowsReplay},
	}})
	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("non-replayable task was re-driven: %d", n)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked || !strings.Contains(item.Detail, "replay policy") {
		t.Fatalf("unexpected replay-blocked state: %#v", item)
	}
	assertRecoveryFailureEvent(t, item, FailurePolicy, ReconcileOnly)
}

func TestResumeInterruptedTasks_UnattendedExternalWriteBlocked(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.unattended = true
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:         "1",
			Agent:      "a",
			Desc:       "deploy to external api",
			Status:     TaskInProgress,
			SideEffect: SideEffectExternalWrite,
		},
	})

	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-driven for unattended external_write, got %d", n)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("task status should be blocked, got %s", item.Status)
	}
}

func TestResumeInterruptedTasks_ReconciliationFlow(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hufu-recovery-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := newBudgetCoordinator(t)
	c.projectDir = tmpDir

	// Case 1: Reconcile tool exit 0 -> Complete -> Mark Done
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:            "10",
			Agent:         "a",
			Desc:          "task complete probe",
			Status:        TaskInProgress,
			SideEffect:    SideEffectExternalWrite,
			Recovery:      RecoveryReconcile,
			ReconcileTool: "exit 0",
		},
	})

	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-executed for completed reconcile, got %d", n)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskDone {
		t.Errorf("expected status TaskDone, got %s", item.Status)
	}
	if item.RecoveryState != RecoveryStateComplete {
		t.Errorf("expected recovery state 'complete', got %s", item.RecoveryState)
	}

	// Case 2: Reconcile tool exit 2 -> Partial -> Blocked
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:            "11",
			Agent:         "a",
			Desc:          "task partial probe",
			Status:        TaskInProgress,
			SideEffect:    SideEffectExternalWrite,
			Recovery:      RecoveryReconcile,
			ReconcileTool: "exit 2",
		},
	})

	n, err = c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-executed for partial reconcile, got %d", n)
	}
	item = c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("expected status TaskBlocked, got %s", item.Status)
	}
	if item.RecoveryState != RecoveryStatePartial {
		t.Errorf("expected recovery state 'partial', got %s", item.RecoveryState)
	}
	assertRecoveryFailureEvent(t, item, FailurePolicy, NeedsHuman)

	// Case 3: Reconcile tool exit 3 -> Unknown -> Blocked
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:            "12",
			Agent:         "a",
			Desc:          "task unknown probe",
			Status:        TaskInProgress,
			SideEffect:    SideEffectExternalWrite,
			Recovery:      RecoveryReconcile,
			ReconcileTool: "exit 3",
		},
	})

	n, err = c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-executed for unknown reconcile, got %d", n)
	}
	item = c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("expected status TaskBlocked, got %s", item.Status)
	}
	if item.RecoveryState != RecoveryStateUnknown {
		t.Errorf("expected recovery state 'unknown', got %s", item.RecoveryState)
	}
	assertRecoveryFailureEvent(t, item, FailurePolicy, NeedsHuman)
}

func TestResumeInterruptedTasks_FailOnUnknownState(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hufu-recovery-unknown-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	session := &TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
		Config: agent.TeamConfig{
			Name:     "test-team",
			GoalMode: "exploratory",
		},
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("failed to create coordinator: %v", err)
	}

	strictProf, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	strictProf.DisableHistoricalTaskReuse = false
	strictProf.DisableJournalRestore = false
	c.SetExecutionProfile(strictProf)

	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:            "1",
			Agent:         "a",
			Desc:          "task unknown probe with strict profile",
			Status:        TaskInProgress,
			SideEffect:    SideEffectExternalWrite,
			Recovery:      RecoveryReconcile,
			ReconcileTool: "exit 3",
		},
	})

	_, err = c.ResumeInterruptedTasks(context.Background())
	if err == nil {
		t.Fatal("expected error from ResumeInterruptedTasks when FailOnUnknownState is true, got nil")
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskError {
		t.Errorf("expected status TaskError under FailOnUnknownState, got %s", item.Status)
	}
	if item.RecoveryState != RecoveryStateUnknown {
		t.Errorf("expected recovery state 'unknown', got %s", item.RecoveryState)
	}
	assertRecoveryFailureEvent(t, item, FailurePolicy, NeedsHuman)
}

func TestResumeInterruptedTasks_RecoveryEventStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hufu-recovery-es-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	es, err := NewEventStore(tmpDir, "run-rec", "session-rec")
	if err != nil {
		t.Fatalf("failed to create event store: %v", err)
	}

	c := newBudgetCoordinator(t)
	c.eventStore = es
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:         "20",
			Agent:      "a",
			Desc:       "mutate infrastructure",
			Status:     TaskInProgress,
			SideEffect: SideEffectInfraMutation,
		},
	})

	_, _ = c.ResumeInterruptedTasks(context.Background())

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}

	found := false
	for _, ev := range events {
		if ev.Type == "recovery_decision" && ev.TaskID == "20" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected recovery_decision event in EventStore for task 20")
	}
}

func TestInferSideEffectClass(t *testing.T) {
	tests := []struct {
		tools string
		want  SideEffectClass
	}{
		{"", SideEffectNone},
		{"all", SideEffectInfraMutation}, // "all" grants sudo → conservative
		{"view,grep,glob,ls", SideEffectNone},
		{"view,write,edit", SideEffectWorkspaceWrite},
		{"bash", SideEffectWorkspaceWrite},
		{"golang,lua", SideEffectWorkspaceWrite},
		{"view,download,fetch", SideEffectWorkspaceWrite},
		{"view,ssh", SideEffectExternalWrite},
		{"bash,ssh", SideEffectExternalWrite},      // ssh present, no sudo
		{"view,sudo", SideEffectInfraMutation},     // sudo highest
		{"bash,ssh,sudo", SideEffectInfraMutation}, // sudo wins
		{"edit,ssh,write", SideEffectExternalWrite},
	}
	for _, tt := range tests {
		got := InferSideEffectClass(tt.tools)
		if got != tt.want {
			t.Errorf("InferSideEffectClass(%q) = %q, want %q", tt.tools, got, tt.want)
		}
	}
}

func TestResumeInterruptedTasks_RecoveryNeverSkipped(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{
			ID:         "30",
			Agent:      "a",
			Desc:       "credential rotation (never retry)",
			Status:     TaskInProgress,
			SideEffect: SideEffectCredential,
			Recovery:   RecoveryNever, // explicit override
		},
	})

	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-driven for never policy, got %d", n)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskSkipped {
		t.Errorf("never policy should skip task, got status %s", item.Status)
	}
	if !strings.Contains(item.Detail, "left as-is") {
		t.Errorf("expected 'left as-is' in detail, got %q", item.Detail)
	}
	// 'never' must NOT emit needs_human (unlike 'manual').
	// Verify the task is no longer interrupted so a second resume is a no-op.
	n2, _ := c.ResumeInterruptedTasks(context.Background())
	if n2 != 0 {
		t.Errorf("skipped task should not be re-selected on second resume, got %d", n2)
	}
}

func TestResolveTaskRecovery_Precedence(t *testing.T) {
	// Tier 3: tool-inferred (no explicit, agent has no side_effect frontmatter).
	sshAgent := &agent.AgentDef{Name: "deployer", Tools: "view,ssh"}
	se, rec, rt := resolveTaskRecovery(sshAgent, TaskDef{Agent: "deployer", Goal: "deploy"})
	if se != SideEffectExternalWrite {
		t.Errorf("tier3: expected external_write from ssh tool, got %q", se)
	}
	if rec != "" {
		t.Errorf("tier3: recovery should be empty (derived at resume), got %q", rec)
	}
	if rt != "" {
		t.Errorf("tier3: reconcile_tool should be empty, got %q", rt)
	}

	// Tier 2: agent-level frontmatter overrides tool inference.
	infraAgent := &agent.AgentDef{Name: "ops", Tools: "bash,sudo", SideEffect: "external_write", Recovery: "manual", ReconcileTool: "exit 0"}
	se, rec, rt = resolveTaskRecovery(infraAgent, TaskDef{Agent: "ops", Goal: "rotate creds"})
	if se != SideEffectExternalWrite {
		t.Errorf("tier2: expected external_write from frontmatter, got %q", se)
	}
	if rec != RecoveryManual {
		t.Errorf("tier2: expected manual from frontmatter, got %q", rec)
	}
	if rt != "exit 0" {
		t.Errorf("tier2: expected reconcile_tool from frontmatter, got %q", rt)
	}

	// Tier 1: task-level explicit overrides agent frontmatter + tools.
	se, rec, rt = resolveTaskRecovery(infraAgent, TaskDef{Agent: "ops", Goal: "rotate", SideEffect: SideEffectCredential, Recovery: RecoveryNever, ReconcileTool: "echo ok"})
	if se != SideEffectCredential {
		t.Errorf("tier1: expected credential from task, got %q", se)
	}
	if rec != RecoveryNever {
		t.Errorf("tier1: expected never from task, got %q", rec)
	}
	if rt != "echo ok" {
		t.Errorf("tier1: expected reconcile_tool from task, got %q", rt)
	}

	// Read-only agent → none → retry on resume.
	roAgent := &agent.AgentDef{Name: "reader", Tools: "view,grep,glob,ls"}
	se, _, _ = resolveTaskRecovery(roAgent, TaskDef{Agent: "reader", Goal: "read"})
	if se != SideEffectNone {
		t.Errorf("readonly: expected none, got %q", se)
	}

	// Nil agentDef (unknown agent): task values pass through unchanged.
	se, rec, rt = resolveTaskRecovery(nil, TaskDef{Agent: "ghost", Goal: "x", SideEffect: SideEffectInfraMutation})
	if se != SideEffectInfraMutation {
		t.Errorf("nil agent: expected infra_mutation passthrough, got %q", se)
	}
	if rec != "" || rt != "" {
		t.Errorf("nil agent: recovery/reconcile should be empty, got %q/%q", rec, rt)
	}
}
