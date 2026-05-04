package hooks

import (
	"context"
	"encoding/json"
	"testing"
)

func TestHookRegistryNoHooks(t *testing.T) {
	r := NewHookRegistry()
	payload := HookPayload{HookPoint: "before_tool_call", ToolName: "bash"}
	resp := r.Dispatch(context.Background(), "before_tool_call", payload)
	if resp.Result != HookContinue {
		t.Errorf("empty registry should return HookContinue, got %v", resp.Result)
	}
}

func TestHookRegistrySingleContinue(t *testing.T) {
	r := NewHookRegistry()
	r.Register("before_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookContinue}
	})
	resp := r.Dispatch(context.Background(), "before_tool_call", HookPayload{})
	if resp.Result != HookContinue {
		t.Errorf("got %v, want HookContinue", resp.Result)
	}
}

func TestHookRegistrySkip(t *testing.T) {
	r := NewHookRegistry()
	r.Register("before_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookSkip}
	})
	resp := r.Dispatch(context.Background(), "before_tool_call", HookPayload{})
	if resp.Result != HookSkip {
		t.Errorf("got %v, want HookSkip", resp.Result)
	}
}

func TestHookRegistryReplace(t *testing.T) {
	r := NewHookRegistry()
	r.Register("before_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookReplace, Replacement: json.RawMessage(`{"command":"ls"}`)}
	})
	resp := r.Dispatch(context.Background(), "before_tool_call", HookPayload{})
	if resp.Result != HookReplace {
		t.Errorf("got %v, want HookReplace", resp.Result)
	}
	if string(resp.Replacement) != `{"command":"ls"}` {
		t.Errorf("got replacement %q, want %q", string(resp.Replacement), `{"command":"ls"}`)
	}
}

func TestHookRegistryErrorAborts(t *testing.T) {
	r := NewHookRegistry()
	called := 0
	r.Register("before_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		called++
		return HookResponse{Result: HookError, ErrorMessage: "blocked"}
	})
	r.Register("before_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		called++
		return HookResponse{Result: HookContinue}
	})
	resp := r.Dispatch(context.Background(), "before_tool_call", HookPayload{})
	if resp.Result != HookError {
		t.Errorf("got %v, want HookError", resp.Result)
	}
	if resp.ErrorMessage != "blocked" {
		t.Errorf("got error %q, want %q", resp.ErrorMessage, "blocked")
	}
	if called != 1 {
		t.Errorf("second hook should not run after HookError, called=%d", called)
	}
}

func TestHookRegistryLastNonContinueWins(t *testing.T) {
	r := NewHookRegistry()
	r.Register("after_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookContinue}
	})
	r.Register("after_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookReplace, Replacement: json.RawMessage(`"sanitized"`)}
	})
	r.Register("after_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookContinue}
	})
	resp := r.Dispatch(context.Background(), "after_tool_call", HookPayload{})
	if resp.Result != HookReplace {
		t.Errorf("got %v, want HookReplace", resp.Result)
	}
	if string(resp.Replacement) != `"sanitized"` {
		t.Errorf("got replacement %q", string(resp.Replacement))
	}
}

func TestHookRegistryDifferentPoints(t *testing.T) {
	r := NewHookRegistry()
	r.Register("before_tool_call", func(ctx context.Context, p HookPayload) HookResponse {
		return HookResponse{Result: HookSkip}
	})
	resp := r.Dispatch(context.Background(), "after_tool_call", HookPayload{})
	if resp.Result != HookContinue {
		t.Errorf("hooks on different point should not interfere, got %v", resp.Result)
	}
}

func TestMakeContext(t *testing.T) {
	ctx := MakeContext("my-team", "researcher", "1", "test task", "qwen3:8b", "sess-abc")
	if ctx.TeamName != "my-team" {
		t.Errorf("TeamName = %q, want %q", ctx.TeamName, "my-team")
	}
	if ctx.AgentName != "researcher" {
		t.Errorf("AgentName = %q, want %q", ctx.AgentName, "researcher")
	}
	if ctx.Timestamp == "" {
		t.Error("Timestamp should be set")
	}
}

func TestValidateHookPoint(t *testing.T) {
	if err := validateHookPoint("before_tool_call"); err != nil {
		t.Errorf("valid hook point should pass: %v", err)
	}
	if err := validateHookPoint("invalid_hook"); err == nil {
		t.Error("invalid hook point should fail")
	}
}

func TestRegisterShellHooks(t *testing.T) {
	r := NewHookRegistry()
	err := RegisterShellHooks(r, map[string]string{
		"before_tool_call": "echo",
	})
	if err != nil {
		t.Fatalf("RegisterShellHooks error: %v", err)
	}
	if !r.HasHooks("before_tool_call") {
		t.Error("should have hooks registered")
	}
}

func TestRegisterShellHooksInvalid(t *testing.T) {
	r := NewHookRegistry()
	err := RegisterShellHooks(r, map[string]string{
		"invalid_hook": "echo",
	})
	if err == nil {
		t.Error("should reject invalid hook point")
	}
}

func TestRegisterShellHooksEmpty(t *testing.T) {
	r := NewHookRegistry()
	err := RegisterShellHooks(r, map[string]string{
		"before_tool_call": "",
	})
	if err != nil {
		t.Fatalf("empty command should be skipped: %v", err)
	}
	if r.HasHooks("before_tool_call") {
		t.Error("empty command should not register a hook")
	}
}
