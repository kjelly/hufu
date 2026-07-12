package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
)

// ── Deliverable verification ──────────────────────────────────────────────────

func newVerifyCoordinator(t *testing.T, projectDir string) *Coordinator {
	t.Helper()
	return &Coordinator{
		session:    &TeamSession{Config: agent.TeamConfig{Name: "test", Timeout: 30}},
		projectDir: projectDir,
	}
}

func TestVerifyTaskDeliverable_EmptyCommand(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	result, err := c.verifyTaskDeliverable(context.Background(), nil, "")
	if err != nil {
		t.Errorf("empty verify command should be a no-op, got %v", err)
	}
	if result != nil {
		t.Fatalf("empty verify command returned evidence: %#v", result)
	}
}

func TestVerifyTaskDeliverable_Success(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newVerifyCoordinator(t, dir)
	result, err := c.verifyTaskDeliverable(context.Background(), nil, "test -f report.md")
	if err != nil {
		t.Errorf("expected success when deliverable exists, got %v", err)
	}
	if result == nil || result.ExitCode != 0 || result.Command != "test -f report.md" || result.WorkDir != dir {
		t.Fatalf("unexpected verification evidence: %#v", result)
	}
}

func TestCompletionVerificationInstructions(t *testing.T) {
	instructions := completionVerificationInstructions("test -f reports/final.md", "/tmp/project")
	for _, want := range []string{"test -f reports/final.md", "/tmp/project", "not accepted until"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("verification instructions missing %q: %s", want, instructions)
		}
	}
}

func TestFinalizeNormalCompletionMarksIncompleteTasksAsErrors(t *testing.T) {
	c := &Coordinator{
		session:      &TeamSession{Config: agent.TeamConfig{Name: "test"}},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "running"}, {Agent: "worker", Desc: "verifying"}})
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskInProgress, "")
	c.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskInProgress, "")
	c.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskVerifying, "")

	c.finalizeNormalCompletion()

	for _, item := range c.taskTracker.TodoList().Items() {
		if item.Status != TaskError {
			t.Fatalf("task %s status = %s, want error", item.ID, item.Status)
		}
		if item.Detail != "coordinator finished before task completed" {
			t.Fatalf("task %s detail = %q", item.ID, item.Detail)
		}
	}
}

func TestVerifyTaskDeliverable_FailureWhenMissing(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	result, err := c.verifyTaskDeliverable(context.Background(), nil, "test -f does-not-exist.md")
	if err == nil {
		t.Fatal("expected error when deliverable is missing")
	}
	if result == nil || result.ExitCode == 0 {
		t.Fatalf("expected non-zero verification evidence, got %#v", result)
	}
}

func TestVerifyTaskDeliverable_FailureIncludesOutput(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	result, err := c.verifyTaskDeliverable(context.Background(), nil, "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should include command output, got %q", err.Error())
	}
	if result == nil || result.ExitCode != 3 || result.Stderr != "boom" {
		t.Fatalf("unexpected verification evidence: %#v", result)
	}
}

func TestVerifyTaskTimeout_UsesDedicatedVerifyTimeout(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	c.session.Config.VerifyTimeout = 1

	if got := c.verifyTaskTimeout(); got != time.Second {
		t.Fatalf("verifyTaskTimeout() = %s, want 1s", got)
	}

	c.session.Config.VerifyTimeout = 0
	if got := c.verifyTaskTimeout(); got != 120*time.Second {
		t.Fatalf("verifyTaskTimeout() = %s, want default 120s", got)
	}
}

// ── Repeated-failure detection ────────────────────────────────────────────────

func TestSameFailure(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"connection refused", "connection refused", true},
		{"attempt 1 failed: connection refused", "attempt 2 failed: connection refused", true},
		{"Connection Refused", "connection refused", true},
		{"timeout", "connection refused", false},
		{"", "", false},
		{"", "x", false},
	}
	for _, tt := range tests {
		if got := sameFailure(tt.a, tt.b); got != tt.want {
			t.Errorf("sameFailure(%q,%q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// ── Local failure hints ───────────────────────────────────────────────────────

func TestLocalFailureHint(t *testing.T) {
	tests := []struct {
		in       string
		contains string
	}{
		{"deliverable verification failed (command \"x\")", "verification check failed"},
		{"context deadline exceeded", "timed out"},
		{"open foo: no such file or directory", "not found"},
		{"permission denied", "permission"},
		{"step count limit reached", "out of steps"},
		{"duplicate task detected", "already-completed"},
		{"some unknown explosion", "Change your approach"},
	}
	for _, tt := range tests {
		got := localFailureHint(tt.in)
		if !strings.Contains(got, tt.contains) {
			t.Errorf("localFailureHint(%q) = %q, want substring %q", tt.in, got, tt.contains)
		}
	}
}

func TestIsTaskTimeout(t *testing.T) {
	if isTaskTimeout(nil) {
		t.Fatal("nil error must not be treated as a timeout")
	}
	if !isTaskTimeout(context.DeadlineExceeded) {
		t.Fatal("context deadline exceeded must be treated as a timeout")
	}
	if !isTaskTimeout(fmt.Errorf("attempt 1 failed: %w", context.DeadlineExceeded)) {
		t.Fatal("wrapped deadline exceeded must be treated as a timeout")
	}
	if isTaskTimeout(fmt.Errorf("context canceled")) {
		t.Fatal("context canceled must not be treated as a timeout")
	}
}

func TestFailureDetailBudgetExceeded(t *testing.T) {
	c := &Coordinator{}
	c.updateSnapshot(func(s *currentSnapshot) {
		s.Agent = "researcher"
		s.Task = "Find security bugs"
		s.Stage = "tool"
		s.Tool = "bash"
	})

	detail := c.FailureDetail(fmt.Errorf("wall-clock budget exceeded (11m > 10m)"), "budget_exceeded")
	if !strings.Contains(detail, "source=budget_exceeded") {
		t.Fatalf("detail missing source: %q", detail)
	}
	if !strings.Contains(detail, "current=") {
		t.Fatalf("detail missing current status: %q", detail)
	}
	if !strings.Contains(detail, "last_tool=bash") {
		t.Fatalf("detail missing last tool: %q", detail)
	}
}

func TestFailureDetailMaxRoundsExceeded(t *testing.T) {
	c := &Coordinator{}
	detail := c.FailureDetail(fmt.Errorf("max rounds (10) exceeded"), "max_rounds_exceeded")
	if !strings.Contains(detail, "source=max_rounds_exceeded") {
		t.Fatalf("detail missing source: %q", detail)
	}
}

func TestFailureDetailUserDeclined(t *testing.T) {
	c := &Coordinator{}
	detail := c.FailureDetail(fmt.Errorf("user declined task execution"), "user_declined")
	if !strings.Contains(detail, "source=user_declined") {
		t.Fatalf("detail missing source: %q", detail)
	}
}

func TestSegmentFailureSource(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"chat session failed", FailureSourceChatSessionFailed},
		{"team %q failed", FailureSourceTeamFailed},
		{"team %q continuation failed", FailureSourceTeamContinuationFailed},
		{"direct agent @%s failed", FailureSourceDirectAgentFailed},
		{"synthesis for @%s failed", FailureSourceSynthesisFailed},
		{"synthesis continuation for @%s failed", FailureSourceSynthesisContinuationFailed},
		{"something else", FailureSourceSegmentFailed},
	}
	for _, tc := range cases {
		if got := SegmentFailureSource(tc.kind); got != tc.want {
			t.Fatalf("SegmentFailureSource(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestPersistFailureWritesStructuredArtifacts(t *testing.T) {
	workspace := t.TempDir()
	if err := EnsureWorkspaceDirs(workspace); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Config:    agent.TeamConfig{Name: "delegate"},
			Workspace: workspace,
		},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "researcher",
		Desc:  "Find security bugs",
	}})

	c.updateSnapshot(func(s *currentSnapshot) {
		s.Agent = "researcher"
		s.Task = "Find security bugs"
		s.TodoID = items[0].ID
		s.Tool = "bash"
	})

	detail := c.FailureDetail(fmt.Errorf("context deadline exceeded"), "task_timeout")
	c.PersistFailure("researcher", "Find security bugs", items[0].ID, detail)

	gotAgent, gotTask, gotTodoID, gotDetail := c.GetLastFailureContext()
	if gotAgent != "researcher" || gotTask != "Find security bugs" || gotTodoID != items[0].ID || gotDetail != detail {
		t.Fatalf("last failure context mismatch: got %q %q %q %q", gotAgent, gotTask, gotTodoID, gotDetail)
	}

	statusPath := filepath.Join(workspace, statusDir, "researcher.yml")
	statusData, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read status file: %v", err)
	}
	if !strings.Contains(string(statusData), "detail: "+detail) {
		t.Fatalf("status file missing detail: %s", statusData)
	}

	taskFiles, err := filepath.Glob(filepath.Join(workspace, tasksDir, "delegate", "researcher", "*.md"))
	if err != nil {
		t.Fatalf("glob task files: %v", err)
	}
	if len(taskFiles) == 0 {
		t.Fatal("expected task file to be written")
	}
	taskData, err := os.ReadFile(taskFiles[0])
	if err != nil {
		t.Fatalf("read task file: %v", err)
	}
	if !strings.Contains(string(taskData), "## Failure Detail") || !strings.Contains(string(taskData), detail) {
		t.Fatalf("task file missing structured failure detail: %s", taskData)
	}
}

func TestValidateTaskOutput(t *testing.T) {
	task := TaskDef{Goal: "Summarize the findings"}
	longReport := "Let me walk through each finding in detail.\n\n" + strings.Repeat("Finding: the guest agent was disconnected and had to be restarted before verification could proceed. ", 6) + "\nConclusion: PASS."

	cases := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"normal output passes", "Summary complete.\n\nKey issue: guest agent was disconnected.", false},
		{"empty output fails", "   ", true},
		{"short unfinished progress update fails", "Let me try to query the VMs for their IPs using different methods:", true},
		{"long report starting with 'let me' passes", longReport, false},
		{"terse but complete output passes", "CANNOT VERIFY. The VMs do not exist.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskOutput(task, tc.output)
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected output to pass validation, got %v", err)
			}
		})
	}
}

// ── Worker auxiliary-context budget ───────────────────────────────────────────

func TestAssembleContextWithinBudget(t *testing.T) {
	a := strings.Repeat("A", 100)
	b := strings.Repeat("B", 100)
	cc := strings.Repeat("C", 100)

	// Budget fits all three.
	got := assembleContextWithinBudget([]string{a, b, cc}, 5000)
	for _, p := range []string{a, b, cc} {
		if !strings.Contains(got, p) {
			t.Errorf("expected all parts within ample budget")
		}
	}
	if !strings.HasPrefix(got, "\n\n") {
		t.Errorf("non-empty result must be prefixed with blank line for appending")
	}

	// Budget fits only the first two; lowest-priority dropped.
	got = assembleContextWithinBudget([]string{a, b, cc}, 250)
	if !strings.Contains(got, a) || !strings.Contains(got, b) {
		t.Errorf("higher-priority parts should be kept")
	}
	if strings.Contains(got, cc) {
		t.Errorf("lowest-priority part should be dropped when over budget")
	}

	// Zero budget yields nothing.
	if got := assembleContextWithinBudget([]string{a}, 0); got != "" {
		t.Errorf("zero budget should yield empty, got %q", got)
	}

	// Empty parts ignored.
	if got := assembleContextWithinBudget([]string{"", ""}, 100); got != "" {
		t.Errorf("all-empty parts should yield empty, got %q", got)
	}
}

// ── Conversation-history head preservation ────────────────────────────────────

func msgWith(text string) fantasy.Message { return fantasy.NewUserMessage(text) }

func TestTrimHistoryPreservingHead(t *testing.T) {
	msgs := make([]fantasy.Message, 20)
	for i := range msgs {
		msgs[i] = msgWith(string(rune('a' + i)))
	}

	trimmed := trimHistoryPreservingHead(msgs, 10)
	if len(trimmed) != 10 {
		t.Fatalf("expected length 10, got %d", len(trimmed))
	}
	// First message (the goal/setup) must be preserved.
	if firstText(trimmed[0]) != firstText(msgs[0]) {
		t.Errorf("head message not preserved: got %q want %q", firstText(trimmed[0]), firstText(msgs[0]))
	}
	// Last message must be the most recent.
	if firstText(trimmed[len(trimmed)-1]) != firstText(msgs[len(msgs)-1]) {
		t.Errorf("tail message not preserved")
	}

	// No-op when already within max.
	if got := trimHistoryPreservingHead(msgs[:5], 10); len(got) != 5 {
		t.Errorf("within-max should be unchanged, got len %d", len(got))
	}

	// Non-positive max yields nil.
	if got := trimHistoryPreservingHead(msgs, 0); got != nil {
		t.Errorf("max<=0 should yield nil")
	}
}

func firstText(m fantasy.Message) string {
	for _, part := range m.Content {
		if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			return txt.Text
		}
	}
	return ""
}
