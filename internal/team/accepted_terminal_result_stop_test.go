package team

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

// acceptedTerminalResultScriptAgent emulates a model that would continue
// after submit_result if the stream did not stop at the accepted occurrence.
// It evaluates the runner's stop conditions between scripted completions just
// as Fantasy does between real provider responses.
type acceptedTerminalResultScriptAgent struct {
	tool              *submitResultTool
	modelCalls        int
	acceptedCalls     int
	duplicateCalls    int
	postResultToolRun int
	invalidFirst      bool
}

func (a *acceptedTerminalResultScriptAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("scripted test agent only supports streaming")
}

func (a *acceptedTerminalResultScriptAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	var steps []fantasy.StepResult
	for {
		a.modelCalls++
		input := `{"status":"success","summary":"canonical first result"}`
		toolName := submitResultToolName
		if a.invalidFirst && a.modelCalls == 1 {
			input = `{"status":"success"}`
		}
		if a.modelCalls > 1 && !a.invalidFirst {
			a.duplicateCalls++
			a.postResultToolRun++
			input = `{"status":"success","summary":"duplicate result"}`
		}
		if a.modelCalls > 2 || (a.invalidFirst && a.modelCalls > 2) {
			toolName = "view"
			a.postResultToolRun++
			input = `{"file_path":"after-submit"}`
		}

		callID := fmt.Sprintf("scripted-%d", a.modelCalls)
		toolCall := fantasy.ToolCallContent{ToolCallID: callID, ToolName: toolName, Input: input}
		if err := call.OnToolCall(toolCall); err != nil {
			return nil, err
		}
		var response fantasy.ToolResponse
		var err error
		if toolName == submitResultToolName {
			response, err = a.tool.Run(ctx, fantasy.ToolCall{ID: callID, Name: toolName, Input: input})
			if err == nil && !response.IsError {
				a.acceptedCalls++
			}
		} else {
			response = fantasy.NewTextResponse("view should not run after an accepted result")
		}
		if err != nil {
			return nil, err
		}
		var result fantasy.ToolResultOutputContent
		if response.IsError {
			result = fantasy.ToolResultOutputContentError{Error: errors.New(response.Content)}
		} else {
			result = fantasy.ToolResultOutputContentText{Text: response.Content}
		}
		toolResult := fantasy.ToolResultContent{ToolCallID: callID, ToolName: toolName, Result: result}
		if err := call.OnToolResult(toolResult); err != nil {
			return nil, err
		}
		step := fantasy.StepResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
			toolCall,
			toolResult,
			fantasy.TextContent{Text: "scripted worker completion"},
		}}}
		steps = append(steps, step)
		stop := false
		for _, condition := range call.StopWhen {
			if condition(steps) {
				stop = true
				break
			}
		}
		if stop || a.modelCalls >= 3 {
			return &fantasy.AgentResult{Steps: steps, Response: step.Response}, nil
		}
	}
}

func newAcceptedTerminalResultStopCoordinator(t *testing.T, worker fantasy.Agent) (*Coordinator, *TodoItem) {
	t.Helper()
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "accepted-terminal-result", Timeout: 30, MaxRetries: 0},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:         time.Now(),
		taskTracker:         NewTaskTracker(),
		reportStatus:        func(StatusEvent) {},
		taskResultCache:     make(map[string][]cachedTaskEntry),
		executionRunID:      "run-accepted-terminal-result",
		workerAgentOverride: worker,
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "submit one result"}})[0]
	return c, item
}

func TestAcceptedTerminalResultStopsWorkerBeforeDuplicateAndPostResultTools(t *testing.T) {
	worker := &acceptedTerminalResultScriptAgent{}
	c, item := newAcceptedTerminalResultStopCoordinator(t, worker)
	worker.tool = &submitResultTool{coordinator: c, todoID: item.ID}

	if _, err := c.executeTask(withTestProtocolRepairInvocationContext(context.Background()), TaskDef{
		Agent: "worker", Goal: "submit one result",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID); err != nil {
		t.Fatalf("execute task: %v", err)
	}
	if worker.modelCalls != 1 {
		t.Fatalf("model completions = %d, want 1", worker.modelCalls)
	}
	if worker.acceptedCalls != 1 || worker.duplicateCalls != 0 || worker.postResultToolRun != 0 {
		t.Fatalf("result/tool calls = accepted:%d duplicate:%d post-result:%d, want 1/0/0", worker.acceptedCalls, worker.duplicateCalls, worker.postResultToolRun)
	}
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want %s", item.Status, TaskDone)
	}
	if result := c.GetTaskResult(item.ID); result == nil || result.Summary != "canonical first result" {
		t.Fatalf("canonical result = %#v, want first accepted result", result)
	}
}

func TestInvalidSubmitResultDoesNotStopWorkerBeforeSchemaCorrection(t *testing.T) {
	worker := &acceptedTerminalResultScriptAgent{invalidFirst: true}
	c, item := newAcceptedTerminalResultStopCoordinator(t, worker)
	worker.tool = &submitResultTool{coordinator: c, todoID: item.ID}

	if _, err := c.executeTask(withTestProtocolRepairInvocationContext(context.Background()), TaskDef{
		Agent: "worker", Goal: "correct the result schema",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID); err != nil {
		t.Fatalf("execute task: %v", err)
	}
	if worker.modelCalls != 2 {
		t.Fatalf("model completions = %d, want invalid first call plus correction", worker.modelCalls)
	}
	if worker.acceptedCalls != 1 || worker.duplicateCalls != 0 || worker.postResultToolRun != 0 {
		t.Fatalf("result/tool calls = accepted:%d duplicate:%d post-result:%d, want 1/0/0", worker.acceptedCalls, worker.duplicateCalls, worker.postResultToolRun)
	}
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want %s", item.Status, TaskDone)
	}
	if result := c.GetTaskResult(item.ID); result == nil || result.Summary != "canonical first result" {
		t.Fatalf("canonical result = %#v, want corrected result", result)
	}
}
