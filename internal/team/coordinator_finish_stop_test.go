package team

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"charm.land/fantasy"
)

// Exercise the live runner's stop conditions after actual finish responses,
// including a rejected call that the provider must be allowed to correct.
type finishStopScriptAgent struct {
	tool       fantasy.AgentTool
	inputs     []string
	modelCalls int
}

func (a *finishStopScriptAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("scripted finish agent only supports streaming")
}

func (a *finishStopScriptAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	var steps []fantasy.StepResult
	for _, input := range a.inputs {
		a.modelCalls++
		id := fmt.Sprintf("finish-%d", a.modelCalls)
		toolCall := fantasy.ToolCallContent{ToolCallID: id, ToolName: a.tool.Info().Name, Input: input}
		if err := call.OnToolCall(toolCall); err != nil {
			return nil, err
		}
		response, err := a.tool.Run(ctx, fantasy.ToolCall{ID: id, Name: toolCall.ToolName, Input: input})
		if err != nil {
			return nil, err
		}
		var output fantasy.ToolResultOutputContent = fantasy.ToolResultOutputContentText{Text: response.Content}
		if response.IsError {
			output = fantasy.ToolResultOutputContentError{Error: errors.New(response.Content)}
		}
		toolResult := fantasy.ToolResultContent{ToolCallID: id, ToolName: toolCall.ToolName, Result: output}
		// Fantasy's local-tool executor ignores OnToolResult errors. The
		// stream must stop through StopWhen after an accepted finish, while a
		// rejected result alone must not trip that terminal condition.
		_ = call.OnToolResult(toolResult)
		step := fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{toolCall, toolResult}}}
		steps = append(steps, step)
		for _, stop := range call.StopWhen {
			if stop(steps) {
				return &fantasy.AgentResult{Steps: steps, Response: step.Response}, nil
			}
		}
	}
	return &fantasy.AgentResult{Steps: steps, Response: steps[len(steps)-1].Response}, nil
}

func TestCoordinatorStreamStopsAfterTerminalFinish(t *testing.T) {
	for _, name := range []string{"success", "schema_correction", "acceptance_failed"} {
		t.Run(name, func(t *testing.T) {
			c := newDoneWorkflowCoordinator(t)
			t.Cleanup(func() {
				if err := c.Close(); err != nil {
					t.Error(err)
				}
			})
			c.acceptanceCmd = ""
			inputs := []string{`{"response":"canonical report"}`, `{"response":"duplicate finish"}`}
			wantCalls := 1
			if name == "schema_correction" {
				inputs = append([]string{`{`}, inputs...)
				wantCalls = 2
			}
			if name == "acceptance_failed" {
				c.acceptanceCmd = "exit 1"
			}
			script := &finishStopScriptAgent{tool: &finishTool{coordinator: c}, inputs: inputs}
			ctx := context.WithValue(t.Context(), todoIDKey{}, CoordTodoID)
			_, steps, err := c.runAgentWithStatusAndHistory(ctx, script, "coordinator", "finish", nil, &taskTiming{})
			if err != nil {
				t.Fatal(err)
			}
			if script.modelCalls != wantCalls || len(steps) != wantCalls {
				t.Fatalf("model calls=%d steps=%d, want %d; provider continued after terminal finish", script.modelCalls, len(steps), wantCalls)
			}
			result := c.LastRunResult()
			if result == nil || result.Response == "duplicate finish" {
				t.Fatalf("terminal report replaced: %#v", result)
			}
			if name == "acceptance_failed" && (IsRunOutcomeSuccess(result.Outcome) || result.ExitCode == 0 || result.Acceptance.EffectiveState() != AcceptanceFailed) {
				t.Fatalf("failed acceptance became success: %#v", result)
			}
		})
	}
}

func TestCoordinatorFinishDoesNotStopWorkerStream(t *testing.T) {
	c := newDoneWorkflowCoordinator(t)
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	c.finishCalled.Store(true)
	script := &finishStopScriptAgent{
		tool:   &recordingTool{name: "view", resp: fantasy.NewTextResponse("observed")},
		inputs: []string{`{}`, `{}`},
	}
	ctx := context.WithValue(t.Context(), todoIDKey{}, "worker-1")
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, script, "worker", "observe", nil, &taskTiming{}); err != nil {
		t.Fatal(err)
	}
	if script.modelCalls != 2 {
		t.Fatalf("worker stream stopped after %d calls because coordinator finished", script.modelCalls)
	}
}
