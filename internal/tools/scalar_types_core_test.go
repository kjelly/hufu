//go:build linux || darwin

package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

func TestCoreToolScalarConversionPrecedesGuardAndHandler(t *testing.T) {
	const canonical = `{"count":42,"enabled":false}`
	guardCalls, handlerCalls := 0, 0
	approve := true
	tool := &coreTool{
		info: fantasy.ToolInfo{Name: "probe", Required: []string{"count", "enabled"}, Parameters: map[string]any{
			"count": map[string]any{"type": "integer"}, "enabled": map[string]any{"type": "boolean"},
		}},
		guardReviewer: func(_ context.Context, _, args string, _ []string) (bool, string, error) {
			guardCalls++
			if args != canonical {
				t.Fatalf("guard saw a different input: %s", args)
			}
			return approve, "denied by test", nil
		},
		handler: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			handlerCalls++
			if call.Input != canonical {
				t.Fatalf("handler saw a different input: %s", call.Input)
			}
			return fantasy.NewTextResponse("ok"), nil
		},
	}
	ctx := SetToolsAllowed(t.Context(), []string{"probe"})
	ctx = context.WithValue(ctx, GuardRulesKey, []string{"no sudo"})
	response, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"count":"42","enabled":"yes"}`})
	if err != nil || !response.IsError || guardCalls != 0 || handlerCalls != 0 {
		t.Fatal("invalid input reached the guard or handler")
	}
	response, err = tool.Run(ctx, fantasy.ToolCall{Input: `{"count":"42","enabled":"false"}`})
	if err != nil || response.IsError || guardCalls != 1 || handlerCalls != 1 {
		t.Fatalf("canonical input was not executed: %#v %v", response, err)
	}
	approve = false
	response, err = tool.Run(ctx, fantasy.ToolCall{Input: `{"count":"42","enabled":"false"}`})
	if err != nil || !response.IsError || guardCalls != 2 || handlerCalls != 1 {
		t.Fatal("scalar conversion bypassed a guard denial")
	}
}
