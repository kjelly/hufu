package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

type scriptedCoordinatorTool struct {
	name    string
	results []scriptedToolResult
	calls   int
}

type scriptedToolResult struct {
	response fantasy.ToolResponse
	err      error
}

func (t *scriptedCoordinatorTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: t.name} }
func (t *scriptedCoordinatorTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *scriptedCoordinatorTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (t *scriptedCoordinatorTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	result := t.results[t.calls%len(t.results)]
	t.calls++
	return result.response, result.err
}

func errorResponse(text string) scriptedToolResult {
	return scriptedToolResult{response: fantasy.NewTextErrorResponse(text)}
}

func okResponse() scriptedToolResult {
	return scriptedToolResult{response: fantasy.NewTextResponse("ok")}
}

func TestCoordinatorToolFailureSemantics(t *testing.T) {
	type outcome struct {
		terminal bool
		fatal    bool
	}
	cases := []struct {
		name         string
		tool         string
		todoID       string
		repairActive bool
		results      []scriptedToolResult
		want         []outcome
	}{
		{
			name:    "error response is returned to the model",
			tool:    "finish",
			todoID:  CoordTodoID,
			results: []scriptedToolResult{errorResponse("cannot finish successfully while worker tasks failed; call finish again with acknowledge_failed_tasks:true")},
			want:    []outcome{{}},
		},
		{
			name:    "go error is a hard boundary",
			tool:    "agent",
			todoID:  CoordTodoID,
			results: []scriptedToolResult{{response: fantasy.NewTextErrorResponse("journal append failed"), err: errors.New("journal append failed")}},
			want:    []outcome{{terminal: true}},
		},
		{
			name:    "fatal error keeps its marker through the gate",
			tool:    "agent",
			todoID:  CoordTodoID,
			results: []scriptedToolResult{{response: fantasy.NewTextErrorResponse("no-progress stop"), err: markCoordinatorFatal(errors.New("no-progress stop"))}},
			want:    []outcome{{terminal: true, fatal: true}},
		},
		{
			name:    "consecutive error responses are bounded",
			tool:    "agent",
			todoID:  CoordTodoID,
			results: []scriptedToolResult{errorResponse("bad verify")},
			want:    []outcome{{}, {}, {}, {terminal: true}},
		},
		{
			name:    "a successful call resets the streak",
			tool:    "agent",
			todoID:  CoordTodoID,
			results: []scriptedToolResult{errorResponse("e1"), errorResponse("e2"), errorResponse("e3"), okResponse(), errorResponse("e4")},
			want:    []outcome{{}, {}, {}, {}, {}},
		},
		{
			name:         "policy repair responses do not count toward the streak",
			tool:         "agent",
			todoID:       CoordTodoID,
			repairActive: true,
			results:      []scriptedToolResult{errorResponse(coordinatorPolicyRepairPrefix + "\nAttempt 1/2")},
			want:         []outcome{{}, {}, {}, {}, {}},
		},
		{
			name:    "worker tool errors are untouched",
			tool:    "bash",
			todoID:  "7",
			results: []scriptedToolResult{errorResponse("exit status 1")},
			want:    []outcome{{}, {}, {}, {}, {}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := gateTestCoordinator()
			c.taskTracker = NewTaskTracker()
			c.coordinatorPolicyRepairPending.Store(tc.repairActive)
			inner := &scriptedCoordinatorTool{name: tc.tool, results: tc.results}
			gated := c.gatePolicyTools([]fantasy.AgentTool{inner})[0]
			ctx := tools.SetToolsAllowed(context.Background(), []string{tc.tool})
			ctx = context.WithValue(ctx, todoIDKey{}, tc.todoID)
			for i, want := range tc.want {
				response, err := gated.Run(ctx, fantasy.ToolCall{ID: "call", Name: tc.tool})
				if gotTerminal := errors.Is(err, errCoordinatorToolFailure); gotTerminal != want.terminal {
					t.Fatalf("call %d: terminal=%v (err=%v response=%#v), want %v", i, gotTerminal, err, response, want.terminal)
				}
				if gotFatal := errors.Is(err, errCoordinatorFatal); gotFatal != want.fatal {
					t.Fatalf("call %d: fatal=%v (err=%v), want %v", i, gotFatal, err, want.fatal)
				}
				if !want.terminal && err != nil {
					t.Fatalf("call %d: recoverable result carried err=%v", i, err)
				}
			}
		})
	}
}

func TestCoordinatorToolErrorStreakResetsPerInvocation(t *testing.T) {
	c := gateTestCoordinator()
	c.coordinatorToolErrorStreak.Store(maxConsecutiveCoordinatorToolErrors)
	c.noteCoordinatorToolSuccess()
	if got := c.coordinatorToolErrorStreak.Load(); got != 0 {
		t.Fatalf("streak after success = %d, want 0", got)
	}
}

func TestMarkCoordinatorFatalKeepsWrappedSentinels(t *testing.T) {
	if markCoordinatorFatal(nil) != nil {
		t.Fatal("nil error must stay nil")
	}
	err := markCoordinatorFatal(errCoordinatorPolicyRepairExhausted)
	if !errors.Is(err, errCoordinatorFatal) || !errors.Is(err, errCoordinatorPolicyRepairExhausted) {
		t.Fatalf("marked error %v lost a sentinel", err)
	}
	if again := markCoordinatorFatal(err); again != err {
		t.Fatal("marking an already fatal error must not wrap it twice")
	}
	if !strings.Contains(err.Error(), errCoordinatorPolicyRepairExhausted.Error()) {
		t.Fatalf("marked error text = %q, want the original message", err.Error())
	}
}

func TestValidateToolArgumentsLetsAgentDecoderOwnUndeclaredKeys(t *testing.T) {
	task := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"agent": map[string]any{"type": "string", "enum": []string{"worker"}},
			"goal":  map[string]any{"type": "string"},
		},
		"required":             []string{"agent", "goal"},
		"additionalProperties": false,
	}
	parameters := map[string]any{"tasks": map[string]any{"type": "array", "items": task}}
	cases := []struct {
		name     string
		tool     string
		input    string
		wantPath string
	}{
		{name: "agent accepts a documented but undeclared task field", tool: "agent", input: `{"tasks":[{"agent":"worker","goal":"g","verify_spec":{"type":"command"}}]}`},
		{name: "agent accepts an undeclared top-level key", tool: "agent", input: `{"tasks":[{"agent":"worker","goal":"g"}],"note":"x"}`},
		{name: "agent still enforces declared enums", tool: "agent", input: `{"tasks":[{"agent":"outsider","goal":"g"}]}`, wantPath: "$.tasks[0].agent"},
		{name: "agent still enforces required fields", tool: "agent", input: `{"tasks":[{"agent":"worker"}]}`, wantPath: "$.tasks[0].goal"},
		{name: "other tools keep rejecting undeclared keys", tool: "finish", input: `{"tasks":[{"agent":"worker","goal":"g","verify_spec":{}}]}`, wantPath: "$.tasks[0].verify_spec"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := fantasy.ToolInfo{Name: tc.tool, Parameters: parameters, Required: []string{"tasks"}}
			err := validateToolArguments(tc.input, info)
			switch {
			case tc.wantPath == "" && err != nil:
				t.Fatalf("validateToolArguments() = %+v, want nil", err)
			case tc.wantPath != "" && (err == nil || err.Path != tc.wantPath):
				t.Fatalf("validateToolArguments() = %+v, want error at %s", err, tc.wantPath)
			}
		})
	}
}
