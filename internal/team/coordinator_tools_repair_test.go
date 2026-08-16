package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/kjelly/hufu/internal/audit"
)

type protocolRecordingTool struct {
	name      string
	runs      int
	response  fantasy.ToolResponse
	err       error
	provider  fantasy.ProviderOptions
	lastInput string
}

func (m *protocolRecordingTool) Info() fantasy.ToolInfo {
	if m.name != "" && m.name != "agent" {
		return fantasy.ToolInfo{Name: m.name}
	}
	return fantasy.ToolInfo{
		Name: "agent",
		Parameters: map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"agent": map[string]any{"type": "string"}, "goal": map[string]any{"type": "string"}},
					"required":             []string{"agent", "goal"},
					"additionalProperties": false,
				},
			},
		},
		Required: []string{"tasks"},
	}
}

func TestCoordinatorToolArgumentRepairRedirectsOneWrongToolWithoutExecution(t *testing.T) {
	c := &Coordinator{}
	state := &protocolRepairState{}
	agentTool := &protocolRecordingTool{name: "agent"}
	strayTool := &protocolRecordingTool{name: "ls"}
	agentWrapper := &protocolRepairWrapper{base: agentTool, c: c, state: state}
	strayWrapper := &protocolRepairWrapper{base: strayTool, c: c, state: state}

	first, err := agentWrapper.Run(context.Background(), fantasy.ToolCall{ID: "original", Name: "agent", Input: `{"tasks":"bad"}`})
	if err != nil || !first.IsError {
		t.Fatalf("first response=%+v err=%v", first, err)
	}
	redirect, err := strayWrapper.Run(context.Background(), fantasy.ToolCall{ID: "stray", Name: "ls", Input: `{}`})
	if err != nil || !redirect.IsError {
		t.Fatalf("redirect response=%+v err=%v", redirect, err)
	}
	if strayTool.runs != 0 || agentTool.runs != 0 {
		t.Fatalf("redirect executed a tool: stray=%d agent=%d", strayTool.runs, agentTool.runs)
	}
	for _, fragment := range []string{`pending for tool "agent"`, `Do not call "ls"`, `only permitted next call is "agent"`} {
		if !strings.Contains(redirect.Content, fragment) {
			t.Fatalf("redirect prompt missing %q: %s", fragment, redirect.Content)
		}
	}

	corrected, err := agentWrapper.Run(context.Background(), fantasy.ToolCall{ID: "corrected", Name: "agent", Input: `{"tasks":[{"agent":"worker","goal":"fixed"}]}`})
	if err != nil || corrected.IsError || agentTool.runs != 1 {
		t.Fatalf("corrected response=%+v err=%v runs=%d", corrected, err, agentTool.runs)
	}
}

func TestCoordinatorToolArgumentRepairFailsClosedAfterSecondWrongTool(t *testing.T) {
	state := &protocolRepairState{}
	agentWrapper := &protocolRepairWrapper{base: &protocolRecordingTool{name: "agent"}, c: &Coordinator{}, state: state}
	strayWrapper := &protocolRepairWrapper{base: &protocolRecordingTool{name: "ls"}, c: &Coordinator{}, state: state}
	_, _ = agentWrapper.Run(context.Background(), fantasy.ToolCall{ID: "original", Name: "agent", Input: `{"tasks":"bad"}`})
	_, _ = strayWrapper.Run(context.Background(), fantasy.ToolCall{ID: "first-stray", Name: "ls", Input: `{}`})
	_, err := strayWrapper.Run(context.Background(), fantasy.ToolCall{ID: "second-stray", Name: "ls", Input: `{}`})
	if err == nil || !strings.Contains(err.Error(), "repair failed closed") {
		t.Fatalf("second stray error=%v, want terminal fail-closed error", err)
	}
}

func (m *protocolRecordingTool) Run(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	m.runs++
	m.lastInput = call.Input
	if m.response.Type == "" && m.err == nil {
		return fantasy.NewTextResponse("success"), nil
	}
	return m.response, m.err
}

func (m *protocolRecordingTool) ProviderOptions() fantasy.ProviderOptions { return m.provider }
func (m *protocolRecordingTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	m.provider = opts
}

func newProtocolWrapper(c *Coordinator, base fantasy.AgentTool) *protocolRepairWrapper {
	return &protocolRepairWrapper{base: base, c: c, state: &protocolRepairState{}}
}

func TestCoordinatorToolArgumentsNativeArrayExecutesNormally(t *testing.T) {
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(&Coordinator{}, base)
	response, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "native", Name: "agent", Input: `{"tasks":[{"agent":"worker","goal":"bounded"}]}`})
	if err != nil || response.IsError {
		t.Fatalf("native call failed: response=%+v err=%v", response, err)
	}
	if base.runs != 1 {
		t.Fatalf("base runs = %d, want 1", base.runs)
	}
}

func TestCoordinatorToolArgumentsRejectStringifiedArrayWithoutExecution(t *testing.T) {
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(&Coordinator{}, base)
	response, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "bad-string", Name: "agent", Input: `{"tasks":"[{\"agent\":\"worker\",\"goal\":\"secret\"}]"}`})
	if err != nil || !response.IsError {
		t.Fatalf("stringified array response=%+v err=%v", response, err)
	}
	if base.runs != 0 {
		t.Fatalf("rejected call executed %d times", base.runs)
	}
	for _, fragment := range []string{"$.tasks", "expected array", "got string", "Valid example:", "without commentary"} {
		if !strings.Contains(response.Content, fragment) {
			t.Fatalf("repair prompt missing %q: %s", fragment, response.Content)
		}
	}
}

func TestCoordinatorToolArgumentsRejectStringArrayElementWithoutExecution(t *testing.T) {
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(&Coordinator{}, base)
	response, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "bad-item", Name: "agent", Input: `{"tasks":["not-an-object"]}`})
	if err != nil || !response.IsError {
		t.Fatalf("mixed array response=%+v err=%v", response, err)
	}
	if base.runs != 0 || !strings.Contains(response.Content, "$.tasks[0]") {
		t.Fatalf("mixed array was not rejected before execution: runs=%d response=%s", base.runs, response.Content)
	}
}

func TestCoordinatorToolArgumentsRequireCompleteTaskObject(t *testing.T) {
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(&Coordinator{}, base)
	response, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "missing-goal", Name: "agent", Input: `{"tasks":[{"agent":"worker"}]}`})
	if err != nil || !response.IsError {
		t.Fatalf("incomplete task response=%+v err=%v", response, err)
	}
	if base.runs != 0 || !strings.Contains(response.Content, "$.tasks[0].goal") {
		t.Fatalf("incomplete task was not rejected before execution: runs=%d response=%s", base.runs, response.Content)
	}
}

func TestValidateToolArgumentsHonorsPrefixItemsAndMinimum(t *testing.T) {
	info := fantasy.ToolInfo{
		Name: "ordered",
		Parameters: map[string]any{"tasks": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "object"},
			"prefixItems": []map[string]any{
				{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string", "enum": []string{"first"}}}, "required": []string{"agent"}},
			},
		}, "retries": map[string]any{"type": "integer", "minimum": 0}},
		Required: []string{"tasks", "retries"},
	}
	if err := validateToolArguments(`{"tasks":[{"agent":"second"}],"retries":0}`, info); err == nil || err.Path != "$.tasks[0].agent" {
		t.Fatalf("prefix validation error=%v", err)
	}
	if err := validateToolArguments(`{"tasks":[{"agent":"first"}],"retries":-1}`, info); err == nil || err.Path != "$.retries" {
		t.Fatalf("minimum validation error=%v", err)
	}
}

func TestCoordinatorToolArgumentRepairExecutesCorrectedCallExactlyOnce(t *testing.T) {
	c := &Coordinator{}
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(c, base)

	first, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "original", Name: "agent", Input: `{"tasks":"bad"}`})
	if err != nil || !first.IsError {
		t.Fatalf("first response=%+v err=%v", first, err)
	}
	second, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "repair", Name: "agent", Input: `{"tasks":[{"agent":"worker","goal":"fixed"}]}`})
	if err != nil || second.IsError {
		t.Fatalf("repair response=%+v err=%v", second, err)
	}
	if base.runs != 1 {
		t.Fatalf("corrected call executed %d times, want 1", base.runs)
	}
	if c.coordinatorProtocolRepairsAttempt.Load() != 1 || c.coordinatorProtocolRepairsSuccess.Load() != 1 {
		t.Fatalf("repair metrics attempted=%d succeeded=%d", c.coordinatorProtocolRepairsAttempt.Load(), c.coordinatorProtocolRepairsSuccess.Load())
	}
}

func TestCoordinatorToolArgumentSecondMalformedRepairRetainsBothDiagnostics(t *testing.T) {
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(&Coordinator{}, base)
	ctx := context.WithValue(context.Background(), modelKey{}, "openai/gpt-test")

	_, _ = wrapper.Run(ctx, fantasy.ToolCall{ID: "original-call", Name: "agent", Input: `{"tasks":"bad"}`})
	_, err := wrapper.Run(ctx, fantasy.ToolCall{ID: "repair-call", Name: "agent", Input: `{"tasks":["still-bad"]}`})
	if err == nil {
		t.Fatal("second malformed call did not fail closed")
	}
	for _, fragment := range []string{"tool=\"agent\"", "model=\"openai/gpt-test\"", "provider=\"openai\"", "original_call_id=\"original-call\"", "repair_call_id=\"repair-call\"", "original_error=", "repair_error=", "$.tasks[0]"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("terminal diagnostic missing %q: %v", fragment, err)
		}
	}
	if base.runs != 0 {
		t.Fatalf("malformed calls executed %d times", base.runs)
	}
}

func TestCoordinatorToolPolicyAndExecutionErrorsDoNotEnterRepair(t *testing.T) {
	tests := []struct {
		name     string
		response fantasy.ToolResponse
		err      error
	}{
		{name: "policy", response: fantasy.NewTextErrorResponse("policy failure")},
		{name: "post-execution", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := &protocolRecordingTool{response: test.response, err: test.err}
			c := &Coordinator{}
			wrapper := newProtocolWrapper(c, base)
			response, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: test.name, Name: "agent", Input: `{"tasks":[{"agent":"worker","goal":"valid"}]}`})
			if test.err == nil && (err != nil || !response.IsError || response.Content != test.name+" failure") {
				t.Fatalf("response=%+v err=%v", response, err)
			}
			if test.err != nil && err != test.err {
				t.Fatalf("error=%v, want %v", err, test.err)
			}
			if base.runs != 1 || c.coordinatorProtocolRepairsAttempt.Load() != 0 {
				t.Fatalf("runs=%d repairs=%d", base.runs, c.coordinatorProtocolRepairsAttempt.Load())
			}
		})
	}
}

func TestCoordinatorToolArgumentCancellationDuringRepairDoesNotExecute(t *testing.T) {
	base := &protocolRecordingTool{}
	c := &Coordinator{taskTracker: NewTaskTracker()}
	wrapper := newProtocolWrapper(c, base)
	_, _ = wrapper.Run(context.Background(), fantasy.ToolCall{ID: "original", Name: "agent", Input: `{"tasks":"bad"}`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := wrapper.Run(ctx, fantasy.ToolCall{ID: "repair", Name: "agent", Input: `{"tasks":[{"agent":"worker","goal":"valid"}]}`})
	if err == nil || base.runs != 0 {
		t.Fatalf("cancelled repair err=%v runs=%d", err, base.runs)
	}
	if len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatal("cancelled repair changed coordinator task state")
	}
}

func TestCoordinatorToolArgumentAuditRecordsPathWithoutSecret(t *testing.T) {
	workspace := t.TempDir()
	logger, err := audit.NewAuditLogger(workspace, "repair-team")
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{auditLogger: logger}
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(c, base)
	secret := "TOP-SECRET-TASK-TEXT"
	ctx := context.WithValue(context.Background(), modelKey{}, "ollama/qwen")
	_, _ = wrapper.Run(ctx, fantasy.ToolCall{ID: "audit-call", Name: "agent", Input: `{"tasks":"` + secret + `"}`})
	_, err = wrapper.Run(ctx, fantasy.ToolCall{ID: "audit-repair", Name: "agent", Input: `{"tasks":[{"agent":"worker","goal":"fixed"}]}`})
	if err != nil {
		t.Fatalf("valid repair failed: %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(workspace, "logs", "audit", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("audit files=%v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	logText := string(data)
	for _, fragment := range []string{`"event":"tool_argument_schema_violation"`, `"json_path":"$.tasks"`, `"expected_type":"array"`, `"actual_type":"string"`, `"repair_attempt":false`, `"disposition":"repair_requested"`, `"repair_attempt":true`, `"disposition":"repaired_and_executed"`} {
		if !strings.Contains(logText, fragment) {
			t.Fatalf("audit missing %s: %s", fragment, logText)
		}
	}
	if strings.Contains(logText, secret) {
		t.Fatalf("audit leaked task secret: %s", logText)
	}
}

func TestCoordinatorToolLocalValidationRemainsEnabledWithProviderOptions(t *testing.T) {
	base := &protocolRecordingTool{}
	wrapper := newProtocolWrapper(&Coordinator{}, base)
	user := "strict-schema-provider"
	options := openaicompat.NewProviderOptions(&openaicompat.ProviderOptions{User: &user})
	wrapper.SetProviderOptions(options)
	response, err := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "strict", Name: "agent", Input: `{"tasks":"bad"}`})
	if err != nil || !response.IsError || base.runs != 0 {
		t.Fatalf("strict provider bypassed validation: response=%+v err=%v runs=%d", response, err, base.runs)
	}
	if len(wrapper.ProviderOptions()) != 1 {
		t.Fatal("provider options were not forwarded")
	}
}
