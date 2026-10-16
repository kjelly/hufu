package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func finishedStep(reason fantasy.FinishReason, toolCall bool) fantasy.StepResult {
	content := fantasy.ResponseContent{fantasy.TextContent{Text: "checking finding 2 of 12"}}
	if toolCall {
		content = append(content, fantasy.ToolCallContent{ToolCallID: "call-1", ToolName: "view", Input: `{"file_path":"a.go"}`})
	}
	return fantasy.StepResult{Response: fantasy.Response{Content: content, FinishReason: reason}}
}

func TestStallContinuationPromptNamesOutputLimitTruncation(t *testing.T) {
	cases := []struct {
		name        string
		steps       []fantasy.StepResult
		outputLimit bool
	}{
		{"no steps", nil, false},
		{"model ended its turn", []fantasy.StepResult{finishedStep(fantasy.FinishReasonStop, false)}, false},
		{"output limit on last step", []fantasy.StepResult{finishedStep(fantasy.FinishReasonToolCalls, true), finishedStep(fantasy.FinishReasonLength, false)}, true},
		{"output limit on an earlier step", []fantasy.StepResult{finishedStep(fantasy.FinishReasonLength, false), finishedStep(fantasy.FinishReasonStop, false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stalledAtOutputLimit(tc.steps); got != tc.outputLimit {
				t.Fatalf("stalledAtOutputLimit = %t, want %t", got, tc.outputLimit)
			}
			want := stalledContinuationPrompt
			if tc.outputLimit {
				want = outputLimitContinuationPrompt
			}
			if got := stallContinuationPrompt(tc.steps); got != want {
				t.Fatalf("stallContinuationPrompt = %q, want %q", got, want)
			}
		})
	}
}

func TestProtocolRepairInstructionsCorrectOnlyARejectedSubmission(t *testing.T) {
	const correction = "Correct the rejected fields"
	cases := []struct {
		name     string
		rejected bool
	}{
		{"no submission to correct", false},
		{"rejected submission", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := protocolRepairInstructions(tc.rejected)
			if strings.Contains(got, correction) != tc.rejected {
				t.Fatalf("instructions mention correction = %t, want %t: %q", !tc.rejected, tc.rejected, got)
			}
			if !strings.Contains(got, "Call submit_result exactly once") || !strings.HasSuffix(got, "emit a prose final response.\n") {
				t.Fatalf("instructions lost the submission contract: %q", got)
			}
		})
	}
}

// stalledFinishWorker ends every turn without a tool call, finishing with
// reason, as the 2026-10-15 researcher did when reasoning consumed its whole
// output budget. It records the messages of each call.
type stalledFinishWorker struct {
	reason   fantasy.FinishReason
	calls    int
	messages [][]fantasy.Message
	onCall   func(int)
}

func (m *stalledFinishWorker) result(call fantasy.AgentCall) *fantasy.AgentResult {
	m.calls++
	m.messages = append(m.messages, append([]fantasy.Message(nil), call.Messages...))
	if m.onCall != nil {
		m.onCall(m.calls)
	}
	step := finishedStep(m.reason, false)
	return &fantasy.AgentResult{Steps: []fantasy.StepResult{step}, Response: step.Response}
}

func (m *stalledFinishWorker) Generate(_ context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.result(call), nil
}

func (m *stalledFinishWorker) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.result(fantasy.AgentCall{Prompt: call.Prompt, Messages: call.Messages}), nil
}

func outputLimitTestCoordinator(t *testing.T) (*Coordinator, *TodoItem) {
	t.Helper()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "output-limit", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"researcher": {Name: "researcher", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:    time.Now(),
		taskTracker:    NewTaskTracker(),
		reportStatus:   func(StatusEvent) {},
		taskCache:      newDefaultTaskCache(taskCacheDependencies{}),
		executionRunID: "run-output-limit",
	}
	return c, c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "researcher", Desc: "research the event"}})[0]
}

func lastUserText(t *testing.T, messages []fantasy.Message) string {
	t.Helper()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != fantasy.MessageRoleUser {
			continue
		}
		var text strings.Builder
		for _, part := range messages[i].Content {
			if textPart, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				text.WriteString(textPart.Text)
			}
		}
		return text.String()
	}
	t.Fatal("continuation carried no user message")
	return ""
}

func TestStalledWorkerContinuationNamesOutputLimit(t *testing.T) {
	cases := []struct {
		name   string
		reason fantasy.FinishReason
		want   string
	}{
		{"model ended its turn", fantasy.FinishReasonStop, stalledContinuationPrompt},
		{"output limit", fantasy.FinishReasonLength, outputLimitContinuationPrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, item := outputLimitTestCoordinator(t)
			worker := &stalledFinishWorker{reason: tc.reason}
			// The continuation submits, so no result-only repair follows.
			worker.onCall = func(call int) {
				if call == 2 {
					c.storeSubmittedTaskResult(item.ID, &TaskResult{TaskID: item.ID, Agent: "researcher", Status: "success", Summary: "submitted", Source: "submitted"})
				}
			}
			c.workerAgentOverride = worker
			if _, err := c.executeTask(withTestProtocolRepairInvocationContext(context.Background()), TaskDef{
				Agent: "researcher", Goal: "research the event", Execution: ExecutionContract{RequiresResult: true},
			}, item.ID); err != nil {
				t.Fatalf("executeTask: %v", err)
			}
			if worker.calls != 2 {
				t.Fatalf("worker calls = %d, want the initial turn and one continuation", worker.calls)
			}
			if got := lastUserText(t, worker.messages[1]); got != tc.want {
				t.Fatalf("continuation prompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProtocolRepairPromptForOutputLimitedWorker(t *testing.T) {
	cases := []struct {
		name        string
		reason      fantasy.FinishReason
		outputLimit bool
	}{
		{"model ended its turn", fantasy.FinishReasonStop, false},
		{"output limit", fantasy.FinishReasonLength, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, item := outputLimitTestCoordinator(t)
			c.workerAgentOverride = &stalledFinishWorker{reason: tc.reason}
			repairCalls := 0
			var prompts []string
			c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls, prompts: &prompts, onCall: func(int) {
				c.storeSubmittedTaskResult(item.ID, &TaskResult{TaskID: item.ID, Agent: "researcher", Status: "partial", Summary: "repaired", Source: "submitted"})
			}}
			_, _ = c.executeTask(withTestProtocolRepairInvocationContext(context.Background()), TaskDef{
				Agent: "researcher", Goal: "research the event", Execution: ExecutionContract{RequiresResult: true},
			}, item.ID)
			if repairCalls != 1 || len(prompts) != 1 {
				t.Fatalf("repair calls = %d prompts = %d, want one repair", repairCalls, len(prompts))
			}
			if got := strings.Contains(prompts[0], "## Output limit"); got != tc.outputLimit {
				t.Fatalf("repair prompt names the output limit = %t, want %t:\n%s", got, tc.outputLimit, prompts[0])
			}
			if strings.Contains(prompts[0], "Correct the rejected fields") {
				t.Fatalf("repair prompt asks to correct a submission the worker never made:\n%s", prompts[0])
			}
		})
	}
}
