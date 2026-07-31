package team

import (
	"context"
	"errors"
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

func TestIsUnfixableVerifyFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"wrong polarity", fmt.Errorf(`deliverable verification failed (command "grep -c foo"): exit status 1: 0 — wrong polarity: the verify command checked that a resource EXISTS`), true},
		{"unrelated verify failure", fmt.Errorf(`deliverable verification failed (command "test -f report.md"): exit status 1`), false},
		{"timeout", fmt.Errorf("verification timed out after 5s"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isUnfixableVerifyFailure(tt.err); got != tt.want {
				t.Errorf("isUnfixableVerifyFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
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

	trimmed, _, removed := trimHistoryPreservingHead(msgs, nil, 10)
	if len(trimmed) != 10 {
		t.Fatalf("expected length 10, got %d", len(trimmed))
	}
	// First message (the goal/setup) must be preserved.
	if firstText(trimmed[0]) != firstText(msgs[0]) {
		t.Errorf("head message not preserved: got %q want %q", firstText(trimmed[0]), firstText(msgs[0]))
	}
	if removed <= 0 {
		t.Fatalf("expected trimmed history to remove source messages, got %d", removed)
	}
	// Last message must be the most recent.
	if firstText(trimmed[len(trimmed)-1]) != firstText(msgs[len(msgs)-1]) {
		t.Errorf("tail message not preserved")
	}

	// No-op when already within max.
	got, _, gotRemoved := trimHistoryPreservingHead(msgs[:5], nil, 10)
	if len(got) != 5 {
		t.Errorf("within-max should be unchanged, got len %d", len(got))
	}
	if gotRemoved != 0 {
		t.Errorf("within-max should not remove source messages, removed %d", gotRemoved)
	}

	// Non-positive max yields nil.
	got, _, gotRemoved = trimHistoryPreservingHead(msgs, nil, 0)
	if got != nil {
		t.Errorf("max<=0 should yield nil")
	}
	if gotRemoved != len(msgs) {
		t.Errorf("max<=0 should remove all source messages, removed %d", gotRemoved)
	}
}

func TestTrimHistoryPreservingHeadKeepsOriginalGoalAndLatestForty(t *testing.T) {
	msgs := make([]fantasy.Message, 100)
	msgs[0] = msgWith("original goal: preserve this")
	for i := 1; i < len(msgs); i++ {
		msgs[i] = msgWith(fmt.Sprintf("exchange-%d", i))
	}
	trimmed, _, _ := trimHistoryPreservingHead(msgs, nil, 41)
	if len(trimmed) != 41 || firstText(trimmed[0]) != "original goal: preserve this" {
		t.Fatalf("unexpected retained history: len=%d first=%q", len(trimmed), firstText(trimmed[0]))
	}
	for i := 60; i < 100; i++ {
		if !strings.Contains(firstText(trimmed[i-59]), fmt.Sprintf("exchange-%d", i)) {
			t.Fatalf("latest exchange-%d was not preserved", i)
		}
	}
}

func TestTrimHistoryPreservingHead_PreservesToolPairWithSourceCounts(t *testing.T) {
	callA := fantasy.ToolCallPart{ToolCallID: "call_a", ToolName: "view", Input: `{"file_path":"a.txt"}`}
	resA := fantasy.ToolResultPart{ToolCallID: "call_a", Output: fantasy.ToolResultOutputContentText{Text: "ok"}}
	callB := fantasy.ToolCallPart{ToolCallID: "call_b", ToolName: "edit", Input: `{"file_path":"b.txt"}`}
	resB := fantasy.ToolResultPart{ToolCallID: "call_b", Output: fantasy.ToolResultOutputContentText{Text: "done"}}

	msgs := []fantasy.Message{
		msgWith("goal"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callA}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{resA}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{callB}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{resB}},
		msgWith("tail"),
	}

	counts := []int{1, 2, 1, 1, 1, 1}
	trimmed, trimmedCounts, removed := trimHistoryPreservingHead(msgs, counts, 4)
	if len(trimmed) != 4 {
		t.Fatalf("expected kept length 4, got %d", len(trimmed))
	}

	if removed != 3 {
		t.Fatalf("expected to remove 3 source-count units, got %d", removed)
	}

	hasCallA := false
	hasResA := false
	hasCallB := false
	hasResB := false
	for _, msg := range trimmed {
		for _, part := range msg.Content {
			if p, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				if p.ToolCallID == "call_a" {
					hasCallA = true
				}
				if p.ToolCallID == "call_b" {
					hasCallB = true
				}
			}
			if p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if p.ToolCallID == "call_a" {
					hasResA = true
				}
				if p.ToolCallID == "call_b" {
					hasResB = true
				}
			}
		}
	}
	if !hasCallB || !hasResB {
		t.Fatalf("latest tool pair should be preserved together, got call_b=%v result_b=%v", hasCallB, hasResB)
	}
	if hasCallA || hasResA {
		t.Fatalf("orphaned tool pair should have been trimmed, found call_a=%v result_a=%v", hasCallA, hasResA)
	}
	if len(trimmedCounts) != len(trimmed) {
		t.Fatalf("source count length mismatch: got %d want %d", len(trimmedCounts), len(trimmed))
	}
	if trimmedCounts[0] != 1 || trimmedCounts[1] != 1 || trimmedCounts[2] != 1 || trimmedCounts[3] != 1 {
		t.Fatalf("unexpected normalized source counts: %#v", trimmedCounts)
	}
}

func TestTrimHistoryPreservingHead_FallbackNeverReturnsEmpty(t *testing.T) {
	call := fantasy.ToolCallPart{
		ToolCallID: "call_1",
		ToolName:   "view",
		Input:      `{"file_path":"src/main.go"}`,
	}
	result := fantasy.ToolResultPart{
		ToolCallID: "call_1",
		Output:     fantasy.ToolResultOutputContentText{Text: "ok"},
	}

	msgs := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call}},
		fantasy.NewUserMessage("middle"),
		fantasy.NewUserMessage("tail"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}},
	}
	counts := []int{1, 2, 1, 1}

	trimmed, trimmedCounts, removed := trimHistoryPreservingHead(msgs, counts, 1)
	if len(trimmed) == 0 {
		t.Fatalf("trimHistoryPreservingHead returned empty history; expected at least one message")
	}
	if removed < 0 || removed > len(msgs) {
		t.Fatalf("removed source-count units out of range: %d", removed)
	}
	if len(trimmedCounts) != len(trimmed) {
		t.Fatalf("source count length mismatch: got %d want %d", len(trimmedCounts), len(trimmed))
	}
}

func TestNewCoordinatorRestoresConversationHistorySourceState(t *testing.T) {
	ws := t.TempDir()
	history := []fantasy.Message{
		fantasy.NewUserMessage("goal"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "started"}}},
	}
	if err := SaveConversationHistory(ws, history); err != nil {
		t.Fatalf("SaveConversationHistory failed: %v", err)
	}

	sd := NewSession()
	sd.ConversationHistorySourceCounts = []int{2, 4}
	sd.ConversationHistorySourceOffset = 7
	if err := SaveSession(ws, sd); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	session := &TeamSession{
		Config:    agent.TeamConfig{Name: "test", GoalMode: "exploratory"},
		Workspace: ws,
		Dir:       ws,
	}
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 8, false, false, false, nil, nil, nil, false, "", false, false, []string(nil), false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}

	if c.conversationHistorySourceOffset != 7 {
		t.Fatalf("expected restored source offset 7, got %d", c.conversationHistorySourceOffset)
	}
	if len(c.conversationHistorySourceCounts) != len(history) {
		t.Fatalf("expected source count length %d, got %d", len(history), len(c.conversationHistorySourceCounts))
	}
	if c.conversationHistorySourceCounts[0] != 2 || c.conversationHistorySourceCounts[1] != 4 {
		t.Fatalf("restored counts mismatch: %#v", c.conversationHistorySourceCounts)
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

func TestVerifyTaskDeliverable_ExpiredParentContext(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	result, err := c.verifyTaskDeliverable(ctx, nil, "true")
	if err == nil {
		t.Fatal("expected an error when the parent context is already expired")
	}
	if result != nil {
		t.Fatalf("verify command must not run on an expired context, got evidence: %#v", result)
	}
	if !strings.Contains(err.Error(), "task deadline exceeded before the verify command could run") {
		t.Errorf("error should name the task deadline, got: %v", err)
	}
	if strings.Contains(err.Error(), "verification timed out") {
		t.Errorf("error must not be blamed on the verify command: %v", err)
	}
}

func TestVerifyTaskDeliverable_ParentExpiresMidVerify(t *testing.T) {
	c := newVerifyCoordinator(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := c.verifyTaskDeliverable(ctx, nil, "sleep 5")
	if err == nil {
		t.Fatal("expected an error when the parent deadline expires mid-verify")
	}
	if !strings.Contains(err.Error(), "task deadline exceeded while the verify command was running") {
		t.Errorf("error should say the task deadline hit mid-verify, got: %v", err)
	}
}

func TestVerifyModeObservationPreservesExecutionFailures(t *testing.T) {
	t.Run("normal non-zero exit is observed", func(t *testing.T) {
		c := newVerifyCoordinator(t, t.TempDir())
		result, err := c.verifyTaskDeliverableWithMode(context.Background(), nil, "false", "observation")
		if err != nil {
			t.Fatalf("observation mode should accept an ordinary non-zero exit, got %v", err)
		}
		if result == nil || result.ExitCode == 0 {
			t.Fatalf("observation result = %#v, want non-zero exit evidence", result)
		}
	})

	t.Run("verification timeout", func(t *testing.T) {
		c := newVerifyCoordinator(t, t.TempDir())
		c.session.Config.VerifyTimeout = 1
		result, err := c.verifyTaskDeliverableWithMode(context.Background(), nil, "sleep 5", "observation")
		if err == nil {
			t.Fatal("observation mode must preserve a verification timeout")
		}
		if result == nil || !result.TimedOut {
			t.Fatalf("timeout result = %#v, want timed out evidence", result)
		}
	})

	t.Run("cancelled parent context", func(t *testing.T) {
		c := newVerifyCoordinator(t, t.TempDir())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := c.verifyTaskDeliverableWithMode(ctx, nil, "true", "observation")
		if err == nil {
			t.Fatal("observation mode must preserve a cancelled parent context")
		}
		if result != nil {
			t.Fatalf("cancelled context should not run the command, got %#v", result)
		}
	})

	t.Run("command cannot start", func(t *testing.T) {
		c := newVerifyCoordinator(t, t.TempDir())
		missingShell := filepath.Join(t.TempDir(), "missing-shell")
		result, err := c.verifyTaskDeliverableWithMode(context.Background(), &agent.AgentDef{Shell: missingShell}, "true", "observation")
		if err == nil {
			t.Fatal("observation mode must preserve a command-start failure")
		}
		if result == nil || result.ExitCode != -1 {
			t.Fatalf("start failure result = %#v, want exit code -1", result)
		}
	})
}

func TestVerifyModesAcceptDiagnosticNonZeroExits(t *testing.T) {
	tests := []struct {
		name    string
		command string
		mode    string
	}{
		{name: "expected failure preserves non-ASCII diagnostic", command: "false # 中文", mode: "expected_failure"},
		{name: "observation preserves non-ASCII diagnostic", command: "false # 中文", mode: "observation"},
		{name: "expected failure preserves wrong-polarity diagnostic", command: "printf x | grep -c missing", mode: "expected_failure"},
		{name: "observation preserves wrong-polarity diagnostic", command: "printf x | grep -c missing", mode: "observation"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newVerifyCoordinator(t, t.TempDir())
			result, err := c.verifyTaskDeliverableWithMode(context.Background(), nil, tt.command, tt.mode)
			if err != nil {
				t.Fatalf("%s mode should accept a normal non-zero exit, got %v", tt.mode, err)
			}
			if result == nil || result.ExitCode == 0 || result.TimedOut {
				t.Fatalf("verification result = %#v, want ordinary non-zero exit evidence", result)
			}
		})
	}
}

func TestRecordRunAborted(t *testing.T) {
	ws := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "test"}, Workspace: ws},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.sessionData.AddEntry("user", "run the mission")

	c.recordRunAborted(fmt.Errorf("coordinator: %w", context.Canceled))
	if result := c.LastRunResult(); result == nil || result.Outcome != RunOutcomeCancelled || result.ExitCode != 130 || result.GoalSatisfied {
		t.Fatalf("abort RunResult = %#v, want cancelled, exit 130, goal_satisfied false", result)
	}

	entries := c.sessionData.Entries
	last := entries[len(entries)-1]
	if last.Role != "assistant" {
		t.Fatalf("last entry role = %q, want assistant", last.Role)
	}
	if !strings.Contains(last.Content, "aborted (cancelled by user)") {
		t.Errorf("abort entry missing reason: %q", last.Content)
	}

	if loaded := LoadSession(ws); loaded == nil || len(loaded.Entries) != len(entries) || loaded.RunResult == nil || loaded.RunResult.Outcome != RunOutcomeCancelled {
		t.Error("aborted session was not persisted to disk")
	}

	statusData, err := os.ReadFile(filepath.Join(ws, "status", "coordinator.yml"))
	if err != nil {
		t.Fatalf("coordinator status not written: %v", err)
	}
	if !strings.Contains(string(statusData), "aborted") {
		t.Errorf("coordinator status missing abort reason: %s", statusData)
	}
}

func TestRecordRunAbortedFailureCreatesFailedRunResult(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}, sessionData: NewSession(), taskTracker: NewTaskTracker()}
	c.recordRunAborted(errors.New("provider unavailable"))
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomeFailed || result.ExitCode != 1 || result.GoalSatisfied || result.Reason == "" {
		t.Fatalf("failure abort RunResult = %#v", result)
	}
}
