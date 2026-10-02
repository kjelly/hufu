package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestEvaluateDeterministicGuardRules(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		rules   []string
		blocked bool
		wantErr bool
	}{
		{name: "non matching", input: `{"command":"go test ./..."}`, rules: []string{"deny_tool_input_regex:(?i)roster"}},
		{name: "matching", input: `{"command":"pilot edit --actions roster.json"}`, rules: []string{"deny_tool_input_regex:(?i)roster"}, blocked: true},
		{name: "invalid rule fails closed", input: "anything", rules: []string{"deny_tool_input_regex:["}, wantErr: true},
		{name: "legacy prose remains sidecar rule", input: "anything", rules: []string{"never use production secrets"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, err := EvaluateDeterministicGuardRules("bash", tt.input, tt.rules)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if (reason != "") != tt.blocked {
				t.Fatalf("reason = %q, blocked %v, want blocked %v", reason, reason != "", tt.blocked)
			}
		})
	}
}

func TestCoreToolEnforcesDeterministicGuardBeforeHandler(t *testing.T) {
	called := false
	tool := &coreTool{
		info: fantasy.ToolInfo{Name: "bash", Parameters: map[string]any{
			"command": map[string]any{"type": "string"},
		}, Required: []string{"command"}},
		handler: func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			called = true
			return fantasy.NewTextResponse("executed"), nil
		},
	}
	ctx := context.WithValue(context.Background(), GuardRulesKey, []string{
		"deny_tool_input_regex:(?i)roster_add_host",
	})
	ctx = context.WithValue(ctx, AgentToolsAllowedKey, []string{"bash"})
	var dispositions []ToolExecutionDisposition
	ctx = context.WithValue(ctx, ToolExecutionDispositionReporterKey, ToolExecutionDispositionReporter(func(d ToolExecutionDisposition) {
		dispositions = append(dispositions, d)
	}))
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "guarded", Input: `{"command":"pilot edit --actions roster_add_host.json"}`})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "guard") {
		t.Fatalf("guarded call was not rejected by the guard: %q", resp.Content)
	}
	if called {
		t.Fatal("handler ran for a deterministically denied call")
	}
	// The runtime learns the call never ran, so it does not count as a change.
	if len(dispositions) != 1 || dispositions[0].Kind != "guard_denied" || dispositions[0].Executed || dispositions[0].ToolCallID != "guarded" {
		t.Fatalf("dispositions = %+v (response %q), want one unexecuted guard denial", dispositions, resp.Content)
	}
}
