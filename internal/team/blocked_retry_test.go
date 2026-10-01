package team

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// countingClassificationBackend counts worker turns so a test can tell
// whether the runtime replayed a task.
type countingClassificationBackend struct {
	classificationTestBackend
	turns atomic.Int32
}

func (b *countingClassificationBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.turns.Add(1)
	b.classificationTestBackend.ServeHTTP(w, r)
}

func TestWorkerReportedBlockedIsNotReplayed(t *testing.T) {
	tests := []struct {
		name       string
		args       string
		wantReplay bool
	}{
		{name: "blocked stops", args: `{"status":"blocked","summary":"BLOCKED: step-risk=system-change; needs explicit user confirmation"}`, wantReplay: false},
		{name: "partial still retries", args: `{"status":"partial","summary":"half done"}`, wantReplay: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &countingClassificationBackend{classificationTestBackend: classificationTestBackend{argsJSON: test.args}}
			c, worker := newClassificationTestCoordinatorWithBackend(t, backend)
			worker.MaxRetries = 2
			c.session.Config.MaxRetries = 2
			item := admitClassificationTestTask(t, c, worker, nil)
			task := TaskDef{Agent: worker.Name, Goal: item.Goal, Execution: ExecutionContract{RequiresResult: true}}
			if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
				t.Fatal("an incomplete result must not complete the task")
			}
			turns := backend.turns.Load()
			if test.wantReplay && turns < 2 {
				t.Fatalf("worker turns = %d, want a retry", turns)
			}
			if !test.wantReplay && turns != 1 {
				t.Fatalf("worker turns = %d, want exactly 1 (no replay of a blocked task)", turns)
			}
			got := c.todoItemByID(item.ID)
			if !test.wantReplay && (got.TypedResult == nil || got.TypedResult.Status != TaskResultStatusBlocked) {
				t.Fatalf("stored result = %+v, want the blocked report kept", got.TypedResult)
			}
		})
	}
}

func TestWorkerReportedBlockedDetectsOnlyBlocked(t *testing.T) {
	for status, want := range map[string]bool{
		TaskResultStatusBlocked: true, TaskResultStatusFailed: false, TaskResultStatusPartial: false,
	} {
		err := withFailureClassOverride(validateCompletedTaskResult(&TaskResult{Status: status, Summary: "s"}), FailureExecution)
		if got := workerReportedBlocked(err); got != want {
			t.Errorf("workerReportedBlocked(%s) = %v, want %v", status, got, want)
		}
		if !strings.Contains(err.Error(), `worker reported incomplete task status "`+status+`"`) {
			t.Errorf("message changed: %v", err)
		}
	}
}

func TestDenialDetectionAndHintsNeverInviteWorkarounds(t *testing.T) {
	denials := []string{
		"Guard rule violation: Rule 3 violated: the command uses sudo",
		"path '/home/u/.cache' is outside allowed paths and consent failed: no human available",
		"path '/etc/x' is outside allowed paths — access denied by user",
		"read-only bash policy denied command \"rm\"",
	}
	for index, denial := range denials {
		if index == 0 && !isPermissionBlockedFailureDetail(denial) || index > 0 && !isSafetyDenialFailureDetail(denial) {
			t.Errorf("not detected as a denial: %q", denial)
		}
		hint := strings.ToLower(localFailureHint(denial))
		if strings.Contains(hint, "alternative") || strings.Contains(hint, "change your approach") || !strings.Contains(hint, "blocked") {
			t.Errorf("hint for %q invites a workaround: %q", denial, hint)
		}
	}
	if !strings.Contains(buildFailureReflectionPrompt("a", "g", "Guard rule violation"), "never suggest another command") {
		t.Fatal("reflection prompt does not forbid workaround suggestions")
	}
}

// statusSubmittingAgent is a fake worker that reports the given result through
// the real submit_result tool.
type statusSubmittingAgent struct {
	c     *Coordinator
	input string
}

func (a statusSubmittingAgent) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a statusSubmittingAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a statusSubmittingAgent) run(ctx context.Context) (*fantasy.AgentResult, error) {
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if _, err := (&submitResultTool{coordinator: a.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: a.input}); err != nil {
		return nil, err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "reported"}}}}, nil
}

// TestOnlyABlockedWorkerStopsDelegation runs a producer and its dependent
// through ExecuteTasks. A producer that fails with an ordinary error leaves
// the coordinator free to redispatch the work; only a worker that reported
// itself blocked puts the run into wrap-up.
func TestOnlyABlockedWorkerStopsDelegation(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		retries    int
		wantWrapUp bool
	}{
		{name: "failed producer", input: `{"status":"failed","summary":"build failed"}`, wantWrapUp: false},
		// The operator executor has a retry budget; a worker-reported block is
		// recognized where the runtime would otherwise replay the task.
		{name: "blocked producer", input: `{"status":"blocked","summary":"BLOCKED: step-risk=system-change; needs explicit user confirmation"}`, retries: 1, wantWrapUp: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, _, _ := newRCA20ExecutionCoordinator(t)
			c.workerAgentOverride = statusSubmittingAgent{c: c, input: test.input}
			c.session.Config.MaxRetries = test.retries
			c.session.Agents["worker"].MaxRetries = test.retries
			tasks := []TaskDef{
				{Agent: "worker", Goal: "produce the artifact", SideEffect: SideEffectNone},
				{Agent: "worker", Goal: "consume the artifact", SideEffect: SideEffectNone, DependsOn: []int{0}},
			}
			if _, err := c.ExecuteTasks(context.Background(), tasks); err == nil {
				t.Fatal("a failed producer must not complete the batch")
			}
			items := c.taskTracker.TodoList().Items()
			if len(items) != 2 || items[1].Status != TaskBlocked {
				t.Fatalf("tasks = %+v, want the dependent blocked by its producer", items)
			}
			if c.IsWrapUp() != test.wantWrapUp {
				t.Fatalf("wrap-up = %v, want %v (producer %s %q)", c.IsWrapUp(), test.wantWrapUp, items[0].Status, items[0].Detail)
			}
			_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "produce the artifact again", SideEffect: SideEffectNone}})
			refused := err != nil && strings.Contains(err.Error(), "wrap-up in progress")
			if refused != test.wantWrapUp {
				t.Fatalf("redispatch error = %v, want refused=%v", err, test.wantWrapUp)
			}
		})
	}
}
