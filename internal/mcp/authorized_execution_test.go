package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/mark3labs/mcp-go/client"
)

func authorizedExecutionTestManager(t *testing.T) (*MCPToolManager, MCPTool, string) {
	t.Helper()
	tool := MCPTool{
		Name: "server__tool", ServerName: "server", OrigName: "tool",
		Description: "test", InputSchema: map[string]any{"type": "object"},
	}
	fingerprint, err := MCPToolDescriptorSHA256(tool)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewMCPToolManager("", "")
	manager.tools = []MCPTool{tool}
	manager.toolMap[tool.Name] = tool
	manager.clients[tool.ServerName] = new(client.Client)
	return manager, tool, fingerprint
}

func TestExecuteAuthorizedToolRejectsDescriptorBeforeAuthorizer(t *testing.T) {
	manager, tool, _ := authorizedExecutionTestManager(t)
	authorized := false
	ctx := WithToolAuthorizer(t.Context(), func(context.Context, string, string, string) error {
		authorized = true
		return nil
	})
	_, _, err := manager.ExecuteAuthorizedTool(ctx, tool.Name, strings.Repeat("0", 64), `{}`)
	if !IsToolDescriptorMismatchError(err) || authorized {
		t.Fatalf("error=%v authorized=%v", err, authorized)
	}
}

func TestExecuteAuthorizedToolPreservesAuthorizerForDirectAndGatewayPath(t *testing.T) {
	manager, tool, fingerprint := authorizedExecutionTestManager(t)
	sentinel := errors.New("blocked")
	calls := 0
	ctx := WithToolAuthorizer(t.Context(), func(_ context.Context, server, name, input string) error {
		calls++
		if server != tool.ServerName || name != tool.OrigName || input != `{"a":1}` {
			t.Fatalf("authorizer received server=%q name=%q input=%q", server, name, input)
		}
		return sentinel
	})
	_, _, err := manager.ExecuteAuthorizedTool(ctx, tool.Name, fingerprint, `{"a":1}`)
	if !IsToolAuthorizationError(err) || !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("gateway-compatible path error=%v calls=%d", err, calls)
	}
	direct := &mcpAgentTool{tool: tool, manager: manager}
	response, runErr := direct.Run(ctx, fantasy.ToolCall{Input: `{"a":1}`})
	if runErr != nil || !response.IsError || calls != 2 {
		t.Fatalf("direct response=%#v err=%v calls=%d", response, runErr, calls)
	}
}
