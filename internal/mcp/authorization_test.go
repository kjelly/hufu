package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestMCPAgentToolEnforcesContextAuthorizerBeforeTransport(t *testing.T) {
	called := false
	manager := &MCPToolManager{}
	tool := &mcpAgentTool{
		tool:    MCPTool{Name: "filesystem__read", ServerName: "filesystem", OrigName: "read"},
		manager: manager,
	}
	ctx := WithToolAuthorizer(context.Background(), func(_ context.Context, server, name, _ string) error {
		called = true
		if server != "filesystem" || name != "read" {
			t.Fatalf("authorizer received %q/%q", server, name)
		}
		return context.DeadlineExceeded
	})
	response, err := tool.Run(ctx, fantasy.ToolCall{Input: `{}`})
	if err != nil {
		t.Fatalf("Run() returned transport error after policy denial: %v", err)
	}
	if !response.IsError || !strings.Contains(response.Content, context.DeadlineExceeded.Error()) {
		t.Fatalf("policy denial response = %#v", response)
	}
	if !called {
		t.Fatal("MCP authorizer was not called")
	}
}

func TestAgentSpecificMCPToolUsesAuthorizerAndExecutesDeclaredTool(t *testing.T) {
	server := NewAgentMCPServer("helper", map[string]agent.MCPToolConfig{
		"run-tests": {Cmd: "printf agent-specific-ok"},
	}, "bash")
	tools := server.RegisterTools("", "", "")
	if len(tools) != 1 {
		t.Fatalf("registered agent MCP tools = %d, want 1", len(tools))
	}
	called := false
	ctx := WithToolAuthorizer(context.Background(), func(_ context.Context, server, name, _ string) error {
		called = true
		if server != "helper" || name != "run-tests" {
			t.Fatalf("authorizer received %q/%q", server, name)
		}
		return nil
	})
	response, err := tools[0].Run(ctx, fantasy.ToolCall{Input: `{}`})
	if err != nil {
		t.Fatalf("agent-specific MCP Run failed: %v", err)
	}
	if response.IsError || response.Content != "agent-specific-ok" {
		t.Fatalf("agent-specific MCP response = %#v", response)
	}
	if !called {
		t.Fatal("agent-specific MCP authorizer was not called")
	}
}

func TestAgentSpecificMCPToolDenialPreventsCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	server := NewAgentMCPServer("Helper", map[string]agent.MCPToolConfig{
		"run-tests": {Cmd: "touch " + marker},
	}, "bash")
	tools := server.RegisterTools("", "", "")
	ctx := WithToolAuthorizer(context.Background(), func(context.Context, string, string, string) error {
		return context.DeadlineExceeded
	})
	response, err := tools[0].Run(ctx, fantasy.ToolCall{Input: `{}`})
	if err != nil || !response.IsError {
		t.Fatalf("denial response = %#v, err %v", response, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied MCP command created marker: %v", err)
	}
}
