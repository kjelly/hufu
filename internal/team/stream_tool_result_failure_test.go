package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// Use Fantasy's real local-tool executor: unlike a mock Agent, it discards
// OnToolResult errors. Each request changes the rejected payload so an exact
// tool-input loop detector cannot accidentally make the test pass.
type rejectedResultStreamModel struct {
	fantasy.LanguageModel
	calls    int
	toolName string
	input    func(int) string
	finish   fantasy.FinishReason
	pauseAt  int
	choices  []fantasy.ToolChoice
}

type resultStreamSubmission struct {
	Status            string         `json:"status"`
	Summary           string         `json:"summary"`
	StructuredPayload map[string]any `json:"structured_payload"`
}

func (*rejectedResultStreamModel) Model() string    { return "test-model" }
func (*rejectedResultStreamModel) Provider() string { return "test-provider" }

func (m *rejectedResultStreamModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	if call.ToolChoice != nil {
		m.choices = append(m.choices, *call.ToolChoice)
	}
	if m.calls == m.pauseAt {
		return func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
	callID := fmt.Sprintf("submit-%d", m.calls)
	input := m.input(m.calls)
	return func(yield func(fantasy.StreamPart) bool) {
		for _, part := range []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeToolInputStart, ID: callID, ToolCallName: m.toolName},
			{Type: fantasy.StreamPartTypeToolInputDelta, ID: callID, Delta: input},
			{Type: fantasy.StreamPartTypeToolInputEnd, ID: callID},
			{Type: fantasy.StreamPartTypeToolCall, ID: callID, ToolCallName: m.toolName, ToolCallInput: input},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: m.finish},
		} {
			if !yield(part) {
				return
			}
		}
	}, nil
}

func TestLocalToolResultFailureStopsFantasyStream(t *testing.T) {
	for _, name := range []string{"next_round", "step_limit", "provider_stop", "valid_correction", "same_turn_continuation"} {
		t.Run(name, func(t *testing.T) {
			c := newBudgetCoordinator(t)
			c.session.Workspace = t.TempDir()
			c.executionRunID = "run-local-result-failure"
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "submit a result"}})[0]
			compiled, ref := compiledReviewContract(t, true)
			toolCalls := 0
			tool := fantasy.NewAgentTool(submitResultToolName, "Submit a structured result", func(_ context.Context, input resultStreamSubmission, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				toolCalls++
				raw, err := json.Marshal(input.StructuredPayload)
				if err != nil {
					return fantasy.ToolResponse{}, err
				}
				if _, err := validateStructuredResultPayload(compiled, ref, raw); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				return fantasy.NewTextResponse("accepted"), nil
			})
			model := &rejectedResultStreamModel{
				toolName: submitResultToolName,
				finish:   fantasy.FinishReasonToolCalls,
				input: func(attempt int) string {
					payload := fmt.Sprintf(`{"verdict":"wrong-%d","max_tokens":7,"findings":[{"summary":"ok"}]}`, attempt)
					if name == "valid_correction" && attempt == maxRepeatedSubmitResultFailures {
						payload = `{"verdict":"approve","max_tokens":7,"findings":[{"summary":"ok"}]}`
					}
					return fmt.Sprintf(`{"status":"success","summary":"attempt %d","structured_payload":%s}`, attempt, payload)
				},
			}
			if name == "provider_stop" {
				model.finish = fantasy.FinishReasonStop
			}
			if name == "same_turn_continuation" {
				model.pauseAt = maxRepeatedSubmitResultFailures
			}
			ag := fantasy.NewAgent(model, fantasy.WithTools(tool), fantasy.WithMaxRetries(0))
			transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, item.ID, c.executionRunID, 1, "reviewer")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = transcript.Close() })
			ctx := context.WithValue(withTestAuxiliaryInvocationContext(t.Context()), todoIDKey{}, item.ID)
			ctx = context.WithValue(ctx, taskTranscriptKey{}, transcript)
			ctx = context.WithValue(ctx, taskRequiresResultKey{}, name == "same_turn_continuation")
			// Bound an unfixed stream deterministically, without waiting for a timeout.
			limit := maxRepeatedSubmitResultFailures + 1
			if name == "step_limit" || name == "valid_correction" {
				limit = maxRepeatedSubmitResultFailures
			}
			// A stop finish ends a turn immediately; three independent tool errors
			// in the same turn are covered by the callback-return test below.
			if name == "provider_stop" {
				limit = 1
			}
			_, steps, err := c.runAgentWithStatusAndHistory(ctx, ag, "reviewer", "submit a result", nil, &taskTiming{}, fantasy.StepCountIs(limit))
			if name == "valid_correction" || name == "provider_stop" {
				if err != nil {
					t.Fatalf("recoverable result error stopped the stream: %v", err)
				}
			} else if !isSubmitResultProtocolLoop(err) || ClassifyTaskFailureStructured(FailureClassificationInput{Err: err}) != FailureProtocol {
				t.Fatalf("stream error = %v, want a protocol loop, not success or timeout", err)
			}
			wantCalls := min(limit, maxRepeatedSubmitResultFailures)
			wantSteps := wantCalls
			if name == "same_turn_continuation" {
				wantSteps++
			}
			if model.calls != wantSteps || toolCalls != wantCalls || len(steps) != wantSteps {
				t.Fatalf("provider/tool/steps = %d/%d/%d, want %d/%d/%d", model.calls, toolCalls, len(steps), wantSteps, wantCalls, wantSteps)
			}
			var results int
			for _, record := range transcript.evidenceRecords() {
				if record.Event == "tool_result" {
					results++
				}
			}
			if results != wantCalls {
				t.Fatalf("retained tool observations = %d, want %d", results, wantCalls)
			}
		})
	}
}

func TestToolResultFailureSurvivesIgnoredCallbackAndStreamSuccess(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuation=%t", continuation), func(t *testing.T) {
			testIgnoredToolResultFailure(t, continuation)
		})
	}
}

func testIgnoredToolResultFailure(t *testing.T, continuation bool) {
	t.Helper()
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.executionRunID = "run-ignored-result-error"
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "submit a result"}})[0]
	streams := 0
	ag := &mockAgent{streamFunc: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		streams++
		if continuation && streams == 1 {
			return &fantasy.AgentResult{Steps: []fantasy.StepResult{{Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop}}}}, nil
		}
		for attempt := range maxRepeatedSubmitResultFailures {
			id := fmt.Sprintf("submit-%d", attempt)
			if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: id, ToolName: submitResultToolName, Input: fmt.Sprintf(`{"summary":"%d"}`, attempt)}); err != nil {
				return nil, err
			}
			_ = call.OnToolResult(fantasy.ToolResultContent{ToolCallID: id, ToolName: submitResultToolName, Result: fantasy.ToolResultOutputContentError{Error: errors.New("structured_payload_invalid: missing required field")}})
		}
		_, _, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Model: tokenBudgetTestModel{}})
		if !isSubmitResultProtocolLoop(err) {
			t.Errorf("next PrepareStep error = %v, want protocol loop", err)
		}
		return &fantasy.AgentResult{}, nil
	}}
	ctx := context.WithValue(withTestAuxiliaryInvocationContext(t.Context()), todoIDKey{}, item.ID)
	ctx = context.WithValue(ctx, taskRequiresResultKey{}, true)
	_, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "reviewer", "submit a result", nil, &taskTiming{})
	if !isSubmitResultProtocolLoop(err) || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("ignored callback error = %v, want protocol loop", err)
	}
	wantStreams := 1
	if continuation {
		wantStreams++
	}
	if streams != wantStreams {
		t.Fatalf("streams = %d, want %d", streams, wantStreams)
	}
}

func TestFantasySubmitResultLoopRepairsWithoutWorkerReplay(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.executionRunID = "run-fantasy-result-repair"
	c.session.Agents = map[string]*agent.AgentDef{
		"reviewer": {Name: "reviewer", Role: "worker", SideEffect: string(SideEffectExternalWrite), Generation: agent.GenerationParams{Model: "test"}},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "finalize a result"}})[0]
	model := &rejectedResultStreamModel{
		toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls,
		input: func(attempt int) string {
			return fmt.Sprintf(`{"status":"success","summary":"attempt %d","structured_payload":{"value":%d}}`, attempt, attempt)
		},
	}
	tool := fantasy.NewAgentTool(submitResultToolName, "Submit a result", func(_ context.Context, _ resultStreamSubmission, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextErrorResponse("structured_payload_invalid: /value: got number, want string"), nil
	})
	c.workerAgentOverride = fantasy.NewAgent(model, fantasy.WithTools(tool), fantasy.WithMaxRetries(0), fantasy.WithStopConditions(fantasy.StepCountIs(4)))
	repairCalls := 0
	var prompts []string
	c.repairAgentOverride = &scriptedRepairAgent{
		calls: &repairCalls, prompts: &prompts,
		onCall: func(int) {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "reviewer", Status: TaskResultStatusSuccess,
				Summary: "repaired result", Source: "submitted",
			})
		},
	}
	_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
		Agent: "reviewer", Goal: "finalize a result", Recovery: RecoveryManual,
		SideEffect: SideEffectExternalWrite, Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if model.calls != maxRepeatedSubmitResultFailures || repairCalls != 1 {
		t.Fatalf("provider/repair calls = %d/%d, want three original steps and one result-only repair", model.calls, repairCalls)
	}
	got := c.todoItemByID(item.ID)
	if got.Status != TaskDone || got.ExecutionReceipt == nil || got.ExecutionReceipt.RepairProvenance == nil || !got.ExecutionReceipt.RepairProvenance.Success {
		t.Fatalf("repaired task projection = %#v", got)
	}
	if got.ExecutionReceipt.StepBudget.Used != maxRepeatedSubmitResultFailures {
		t.Fatalf("receipt lost executed steps: %#v", got.ExecutionReceipt.StepBudget)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Runtime validation error") || !strings.Contains(prompts[0], "/value: got number, want string") {
		t.Fatalf("repair lost schema failure evidence: %q", prompts)
	}
}
