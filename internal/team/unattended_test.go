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
	"github.com/kjelly/hufu/internal/agent"
)

func newBudgetCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	return &Coordinator{
		session:         &TeamSession{Config: agent.TeamConfig{Name: "test", Timeout: 30}},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(event StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
}

func TestSetUnattended(t *testing.T) {
	c := newBudgetCoordinator(t)
	if c.IsUnattended() {
		t.Fatal("default should not be unattended")
	}
	c.SetUnattended(true)
	if !c.IsUnattended() {
		t.Error("SetUnattended(true) not reflected")
	}
}

func TestSetAutoApprove(t *testing.T) {
	c := newBudgetCoordinator(t)
	if c.IsAutoApprove() {
		t.Fatal("default should not be auto-approve")
	}
	c.SetAutoApprove(true)
	if !c.IsAutoApprove() {
		t.Error("SetAutoApprove(true) not reflected")
	}
}

func TestBudgetExceeded_Disabled(t *testing.T) {
	c := newBudgetCoordinator(t)
	if ex, _ := c.budgetExceeded(); ex {
		t.Error("no budget configured should never be exceeded")
	}
}

func TestBudgetExceeded_WallClock(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.sessionTime = time.Now().Add(-10 * time.Minute)
	c.SetBudget(60, 0) // 60s budget, already 10min elapsed
	ex, reason := c.budgetExceeded()
	if !ex {
		t.Fatal("wall-clock budget should be exceeded")
	}
	if reason == "" {
		t.Error("expected a reason")
	}
}

func TestBudgetExceeded_WallClock_NotYet(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(3600, 0)
	if ex, _ := c.budgetExceeded(); ex {
		t.Error("fresh session should be within a 1h budget")
	}
}

func TestBudgetExceeded_Tokens(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(0, 1000)
	c.tokensUsed.Store(1500)
	ex, reason := c.budgetExceeded()
	if !ex {
		t.Fatal("token budget should be exceeded")
	}
	if reason == "" {
		t.Error("expected a reason")
	}
}

func TestBudgetExceeded_Tokens_NotYet(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(0, 1000)
	c.tokensUsed.Store(999)
	if ex, _ := c.budgetExceeded(); ex {
		t.Error("999 < 1000 should be within budget")
	}
}

func TestSetBudget_IgnoresZero(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetBudget(100, 200)
	c.SetBudget(0, 0) // zeros must not clear existing budgets
	if c.maxWallClock != 100*time.Second {
		t.Errorf("wall-clock budget should be preserved, got %v", c.maxWallClock)
	}
	if c.tokenBudget != 200 {
		t.Errorf("token budget should be preserved, got %d", c.tokenBudget)
	}
}

func TestRunAcceptance_Empty(t *testing.T) {
	c := newBudgetCoordinator(t)
	if _, err := c.runAcceptance(context.Background()); err != nil {
		t.Errorf("no acceptance command should be a no-op, got %v", err)
	}
}

func TestRunAcceptance_Pass(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetAcceptance("true")
	if _, err := c.runAcceptance(context.Background()); err != nil {
		t.Errorf("`true` should pass, got %v", err)
	}
}

func TestRunAcceptance_Fail(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetAcceptance("echo nope >&2; false")
	if _, err := c.runAcceptance(context.Background()); err == nil {
		t.Fatal("`false` should fail acceptance")
	}
}

func TestRunRollback_Custom(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetRollback("true")
	if err := c.runRollback(context.Background()); err != nil {
		t.Errorf("expected custom rollback 'true' to pass, got %v", err)
	}

	c.SetRollback("false")
	if err := c.runRollback(context.Background()); err == nil {
		t.Error("expected custom rollback 'false' to fail")
	}
}

func TestRunRollback_Git(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hufu-rollback-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	gitDir := filepath.Join(tmpDir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatalf("failed to create mock git dir: %v", err)
	}

	c := newBudgetCoordinator(t)
	c.projectDir = tmpDir
	// We override the default rollback cmd with a mock script because we are not in a real git repo (just empty .git dir)
	// and calling 'git' might fail or warn. However, we want to test if it detects the .git dir and falls back to git commands.
	err = c.runRollback(context.Background())
	if err == nil {
		t.Error("expected rollback to fail on empty mock git repo")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("expected error to mention git, got %v", err)
	}
}

func TestSelfHealingAndRollback(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetUnattended(true)
	c.SetAcceptance("false") // always fails
	c.SetRollback("true")    // rollback succeeds
	c.sessionData = NewSession()

	tool := &finishTool{coordinator: c}
	call := fantasy.ToolCall{Input: `{"response":"test completion"}`}

	// Round 1 of self-healing
	resp, err := tool.Run(context.Background(), call)
	if err != nil {
		t.Fatalf("unexpected tool run error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error response on first self-healing attempt")
	}
	if c.selfHealingAttempts != 1 {
		t.Errorf("expected selfHealingAttempts = 1, got %d", c.selfHealingAttempts)
	}
	if !strings.Contains(resp.Content, "Acceptance check failed") {
		t.Errorf("expected error message to mention acceptance failure, got %q", resp.Content)
	}

	// Round 2 of self-healing
	resp, err = tool.Run(context.Background(), call)
	if err != nil {
		t.Fatalf("unexpected tool run error: %v", err)
	}
	if !resp.IsError {
		t.Error("expected error response on second self-healing attempt")
	}
	if c.selfHealingAttempts != 2 {
		t.Errorf("expected selfHealingAttempts = 2, got %d", c.selfHealingAttempts)
	}

	// Round 3: self-healing exhausted, runs rollback
	resp, err = tool.Run(context.Background(), call)
	if err != nil {
		t.Fatalf("unexpected tool run error: %v", err)
	}
	if resp.IsError {
		t.Error("expected success response indicating finished execution (with rollback message)")
	}
	if !strings.Contains(resp.Content, "FINISHED:") {
		t.Errorf("expected finished output prefix, got %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "rolled back successfully") {
		t.Errorf("expected note about successful rollback, got %q", resp.Content)
	}
}

func TestFinishRequiresAcknowledgementForFailedTasks(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	failed := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "write final report"}})[0]
	c.taskTracker.TodoList().UpdateStatus(failed.ID, TaskError, "expected report.md was not created")
	tool := &finishTool{coordinator: c}

	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all tasks passed"}`})
	if err != nil {
		t.Fatalf("unexpected tool error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "cannot finish successfully") {
		t.Fatalf("finish without acknowledgement = %#v, want failed-task error", resp)
	}

	resp, err = tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"partial result", "acknowledge_failed_tasks":true}`})
	if err != nil {
		t.Fatalf("unexpected tool error: %v", err)
	}
	if resp.IsError || !strings.Contains(resp.Content, "UNRESOLVED TASKS") || !strings.Contains(resp.Content, failed.ID) {
		t.Fatalf("acknowledged finish = %#v, want unresolved-task summary", resp)
	}
}

func TestSessionRestoreTasksAndCache(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hufu-checkpoint-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	c := newBudgetCoordinator(t)
	c.session.Workspace = tmpDir

	sd := NewSession()
	sd.Tasks = []*TodoItem{
		{
			ID:     "1",
			Agent:  "worker",
			Desc:   "task 1",
			Status: TaskDone,
			Output: "result of task 1",
		},
		{
			ID:     "2",
			Agent:  "worker",
			Desc:   "task 2",
			Status: TaskError,
		},
	}

	// Restoration
	c.SetSessionData(sd)

	restoredItems := c.taskTracker.TodoList().Items()
	if len(restoredItems) != 2 {
		t.Errorf("expected 2 restored items, got %d", len(restoredItems))
	}
	if restoredItems[0].ID != "1" || restoredItems[0].Status != TaskDone {
		t.Errorf("first task not restored correctly: %+v", restoredItems[0])
	}
	if restoredItems[1].ID != "2" || restoredItems[1].Status != TaskError {
		t.Errorf("second task not restored correctly: %+v", restoredItems[1])
	}

	// Verify semantic cache prepopulation
	c.taskResultCacheMu.RLock()
	cache := c.taskResultCache["worker"]
	c.taskResultCacheMu.RUnlock()

	if len(cache) != 1 {
		t.Errorf("expected 1 cached entry for 'worker', got %d", len(cache))
	} else {
		if cache[0].taskDesc != "task 1" || cache[0].output != "result of task 1" {
			t.Errorf("cached entry not populated correctly: %+v", cache[0])
		}
	}

	// Verify change hook is registered and triggers saveCheckpoint
	checkpointFile := filepath.Join(tmpDir, "session.json")
	if _, err := os.Stat(checkpointFile); err == nil {
		os.Remove(checkpointFile)
	}

	// Update task status, should trigger checkpoint saving
	c.taskTracker.TodoList().UpdateStatus("2", TaskInProgress, "retrying")

	if _, err := os.Stat(checkpointFile); err != nil {
		t.Error("expected session.json checkpoint to be saved on status change")
	} else {
		saved := LoadSession(tmpDir)
		if saved == nil || len(saved.Tasks) != 2 || saved.Tasks[1].Status != TaskInProgress {
			t.Errorf("saved checkpoint data is invalid: %+v", saved)
		}
	}
}

type mockAgent struct {
	streamFunc func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error)
}

func (m *mockAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.streamFunc(ctx, call)
}

func (m *mockAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func TestLoopDetection_ToolCallAbort(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()

	ag := &mockAgent{
		streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
			// Call 1: command fails
			err := call.OnToolCall(fantasy.ToolCallContent{
				ToolName: "bash",
				Input:    `{"command":"false"}`,
			})
			if err != nil {
				return nil, err
			}

			var errRes fantasy.ToolResultOutputContentError
			errRes.Error = errors.New("command failed")
			err = call.OnToolResult(fantasy.ToolResultContent{
				ToolName: "bash",
				Result:   errRes,
			})
			if err != nil {
				return nil, err
			}

			// Call 2: command fails
			err = call.OnToolCall(fantasy.ToolCallContent{
				ToolName: "bash",
				Input:    `{"command":"false"}`,
			})
			if err != nil {
				return nil, err
			}
			err = call.OnToolResult(fantasy.ToolResultContent{
				ToolName: "bash",
				Result:   errRes,
			})
			if err != nil {
				return nil, err
			}

			// Call 3: exact same command called again. This should fail immediately inside OnToolCall!
			err = call.OnToolCall(fantasy.ToolCallContent{
				ToolName: "bash",
				Input:    `{"command":"false"}`,
			})
			if err != nil {
				// This is the expected loop error!
				return nil, err
			}

			return nil, fmt.Errorf("should have aborted before step 3")
		},
	}

	ctx := withTestAuxiliaryInvocationContext(t.Context())
	_, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "developer", "run task", nil, &taskTiming{})
	if err == nil {
		t.Fatal("expected runAgentWithStatusAndHistory to return an error due to loop detection")
	}

	if !strings.Contains(err.Error(), "stuck in a loop executing the same failing command") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCoordinatorToolErrorTerminatesOrchestratorStream(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()

	ag := &mockAgent{
		streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
			if err := call.OnToolCall(fantasy.ToolCallContent{
				ToolCallID: "coordinator-grep-1",
				ToolName:   "agent",
				Input:      `{"pattern":"--invalid"}`,
			}); err != nil {
				return nil, err
			}

			var errRes fantasy.ToolResultOutputContentError
			errRes.Error = errors.New("delegation policy rejected task")
			if err := call.OnToolResult(fantasy.ToolResultContent{
				ToolCallID: "coordinator-grep-1",
				ToolName:   "agent",
				Result:     errRes,
			}); err != nil {
				return nil, err
			}

			return nil, fmt.Errorf("coordinator continued after a failed tool call")
		},
	}

	ctx := context.WithValue(context.Background(), todoIDKey{}, CoordTodoID)
	_, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "coordinator", "coordinate task", nil, &taskTiming{})
	if err == nil {
		t.Fatal("expected coordinator tool error to terminate the stream")
	}
	if !errors.Is(err, errCoordinatorToolFailure) {
		t.Errorf("expected coordinator tool failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), `tool "agent" failed`) {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCoordinatorReadOnlyToolErrorContinuesOrchestratorStream(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()

	ag := &mockAgent{streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if err := call.OnToolCall(fantasy.ToolCallContent{
			ToolCallID: "coordinator-view-1",
			ToolName:   "view",
			Input:      `{"file_path":".git"}`,
		}); err != nil {
			return nil, err
		}
		var errRes fantasy.ToolResultOutputContentError
		errRes.Error = errors.New(".git is a directory")
		if err := call.OnToolResult(fantasy.ToolResultContent{
			ToolCallID: "coordinator-view-1",
			ToolName:   "view",
			Result:     errRes,
		}); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "continued after read-only observation failure"},
		}}}, nil
	}}

	ctx := context.WithValue(context.Background(), todoIDKey{}, CoordTodoID)
	result, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "coordinator", "coordinate task", nil, &taskTiming{})
	if err != nil {
		t.Fatalf("read-only coordinator error should not terminate stream: %v", err)
	}
	if !strings.Contains(result, "continued after read-only") {
		t.Fatalf("result = %q, want continued response", result)
	}
}

func TestCoordinatorTeamInfoErrorContinuesOrchestratorStream(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()

	ag := &mockAgent{streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if err := call.OnToolCall(fantasy.ToolCallContent{
			ToolCallID: "coordinator-team-info-1",
			ToolName:   "team_info",
			Input:      `{"action":"unknown_action"}`,
		}); err != nil {
			return nil, err
		}
		var errRes fantasy.ToolResultOutputContentError
		errRes.Error = errors.New("unknown team_info action: unknown_action")
		if err := call.OnToolResult(fantasy.ToolResultContent{
			ToolCallID: "coordinator-team-info-1",
			ToolName:   "team_info",
			Result:     errRes,
		}); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "continued after team_info observation failure"},
		}}}, nil
	}}

	ctx := context.WithValue(context.Background(), todoIDKey{}, CoordTodoID)
	result, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "coordinator", "coordinate task", nil, &taskTiming{})
	if err != nil {
		t.Fatalf("read-only team_info coordinator error should not terminate stream: %v", err)
	}
	if errors.Is(err, errCoordinatorToolFailure) {
		t.Fatalf("team_info error must not be reported as a coordinator tool failure")
	}
	if !strings.Contains(result, "continued after team_info") {
		t.Fatalf("result = %q, want continued response", result)
	}
}

func TestCoordinatorInitialToolCorrectionContinuesOrchestratorStream(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.session.Config.Delegation.InitialCoordinatorTool = "agent"
	c.initialToolCorrections.Store(1)

	ag := &mockAgent{streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if err := call.OnToolCall(fantasy.ToolCallContent{
			ToolCallID: "wrong-initial-tool",
			ToolName:   "team_info",
		}); err != nil {
			return nil, err
		}
		var errRes fantasy.ToolResultOutputContentError
		errRes.Error = errors.New(initialCoordinatorToolCorrectionPrefix + ` coordinator's first tool call must be "agent"`)
		if err := call.OnToolResult(fantasy.ToolResultContent{
			ToolCallID: "wrong-initial-tool",
			ToolName:   "team_info",
			Result:     errRes,
		}); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "continued to required initial tool"},
		}}}, nil
	}}

	ctx := context.WithValue(context.Background(), todoIDKey{}, CoordTodoID)
	result, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "coordinator", "coordinate task", nil, &taskTiming{})
	if err != nil {
		t.Fatalf("runtime-issued initial correction terminated stream: %v", err)
	}
	if !strings.Contains(result, "continued to required initial tool") {
		t.Fatalf("result = %q, want continued coordinator output", result)
	}
}

func TestWrapUpRecoveryDoesNotRetryCoordinatorToolFailure(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetWrapUp()
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		t.Fatal("wrap-up recovery must not invoke the coordinator after a direct tool failure")
		return "", nil, nil
	}

	_, _, recovered := c.attemptWrapUpRecovery(context.Background(), &agent.AgentDef{}, fmt.Errorf("%w: tool %q failed", errCoordinatorToolFailure, "grep"))
	if recovered {
		t.Fatal("coordinator tool failure must not be recovered with another model turn")
	}
}
