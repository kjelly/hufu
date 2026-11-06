package mcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestMCPAgentToolPublishesOnlyAuthorizedCompletedResults(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
		"capture": func(mcp.CallToolRequest) *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewImageContent("aGk=", "image/png")}}
		},
	})
	attachRuntimeTestServer(t, manager, "screen", MCPServerConfig{}, fake)
	tool := manager.SnapshotToolDescriptors()[0]
	agentTool := &mcpAgentTool{tool: tool, manager: manager}
	call := fantasy.ToolCall{ID: "capture-call", Name: tool.Name, Input: `{}`}
	published := 0
	ctx := WithToolResultPublisher(WithToolAuthorizer(t.Context(), allowAll), func(_ context.Context, descriptor MCPTool, received fantasy.ToolCall, result RuntimeToolResult) (fantasy.ToolResponse, error) {
		published++
		if descriptor.ServerName != "screen" || descriptor.OrigName != "capture" || received != call || len(result.Content) != 1 || result.Content[0].Data != "aGk=" {
			t.Fatal("publisher lost verified tool/call identity or untrusted content")
		}
		response := fantasy.NewTextResponse("published")
		response.Metadata = "runtime-owned references"
		return response, nil
	})
	response, err := agentTool.Run(ctx, call)
	if err != nil || response.IsError || response.Metadata != "runtime-owned references" || response.Data != nil || published != 1 || fake.calls.Load() != 1 {
		t.Fatal("completed MCP result did not use the host publisher", err)
	}
	denied := WithToolAuthorizer(ctx, func(context.Context, string, string, string) error { return errors.New("denied") })
	response, err = agentTool.Run(denied, call)
	if err != nil || !response.IsError || published != 1 || fake.calls.Load() != 1 {
		t.Fatal("denied MCP call reached the publisher or transport", err)
	}
	failed := WithToolResultPublisher(ctx, func(context.Context, MCPTool, fantasy.ToolCall, RuntimeToolResult) (fantasy.ToolResponse, error) {
		return fantasy.ToolResponse{}, errors.New("private payload must not reach model")
	})
	response, err = agentTool.Run(failed, call)
	if err != nil || !response.IsError || !response.StopTurn || response.Data != nil || strings.Contains(response.Content, "private payload") || fake.calls.Load() != 2 {
		t.Fatal("failed publication exposed data or allowed another tool turn", err)
	}
	raw := WithToolResultPublisher(ctx, func(context.Context, MCPTool, fantasy.ToolCall, RuntimeToolResult) (fantasy.ToolResponse, error) {
		return fantasy.NewImageResponse([]byte("unpublished image"), "image/png"), nil
	})
	response, err = agentTool.Run(raw, call)
	if err != nil || !response.IsError || !response.StopTurn || response.Data != nil || response.MediaType != "" || fake.calls.Load() != 3 {
		t.Fatal("publisher bypassed the reference-only result contract", err)
	}
}

func TestAuthorizedToolResultPreservesOrderedUntrustedMedia(t *testing.T) {
	for _, isError := range []bool{false, true} {
		name := "success"
		if isError {
			name = "tool_error"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewMCPToolManager("", "")
			fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
				"capture": func(mcp.CallToolRequest) *mcp.CallToolResult {
					result := mcp.NewToolResultStructured(map[string]any{"complete": true}, "")
					result.IsError = isError
					result.Content = []mcp.Content{
						mcp.NewTextContent("before"), mcp.NewImageContent("aGk=", "image/png"),
						mcp.NewTextContent("after"), mcp.NewImageContent("aGk=", "image/png"),
					}
					return result
				},
			})
			attachRuntimeTestServer(t, manager, "screen", MCPServerConfig{}, fake)
			tool := manager.SnapshotToolDescriptors()[0]
			digest, err := MCPToolDescriptorSHA256(tool)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithToolAuthorizer(t.Context(), allowAll)
			result, err := manager.ExecuteAuthorizedToolResult(ctx, tool.Name, digest, `{}`)
			if err != nil {
				t.Fatal(err)
			}
			want := []RuntimeToolContent{
				{Type: "text", Text: "before"}, {Type: "image", Data: "aGk=", MIMEType: "image/png"},
				{Type: "text", Text: "after"}, {Type: "image", Data: "aGk=", MIMEType: "image/png"},
			}
			if result.IsError != isError || string(result.StructuredContent) != `{"complete":true}` || !slices.Equal(result.Content, want) || fake.calls.Load() != 1 {
				t.Fatal("typed result changed ordered content, structure, error status, or call count")
			}
			for _, authorized := range []bool{false, true} {
				var text string
				var gotError bool
				if authorized {
					text, gotError, err = manager.ExecuteAuthorizedTool(ctx, tool.Name, digest, `{}`)
				} else {
					text, gotError, err = manager.ExecuteTool(ctx, tool.Name, `{}`)
				}
				if !errors.Is(err, ErrUnsupportedToolContent) || text != "" || gotError != isError || strings.Contains(err.Error(), "aGk=") {
					t.Fatal("legacy text API silently lost media or exposed its data", authorized, err)
				}
			}
			if fake.calls.Load() != 3 {
				t.Fatal("a text conversion failure replayed the completed MCP call")
			}
		})
	}
}

func TestAuthorizedToolResultRejectsChangedDescriptorAndDeniedCall(t *testing.T) {
	manager := NewMCPToolManager("", "")
	fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{"capture": textResult("unused")})
	attachRuntimeTestServer(t, manager, "screen", MCPServerConfig{}, fake)
	tool := manager.SnapshotToolDescriptors()[0]
	digest, err := MCPToolDescriptorSHA256(tool)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"", "forged"} {
		if _, err := manager.ExecuteAuthorizedToolResult(t.Context(), tool.Name, expected, `{}`); !IsToolDescriptorMismatchError(err) {
			t.Fatal("typed API accepted a missing or changed descriptor", err)
		}
	}
	ctx := WithToolAuthorizer(t.Context(), func(context.Context, string, string, string) error { return errors.New("denied") })
	if _, err := manager.ExecuteAuthorizedToolResult(ctx, tool.Name, digest, `{}`); !IsToolAuthorizationError(err) {
		t.Fatal("typed API bypassed its current authorizer", err)
	}
	if fake.calls.Load() != 0 {
		t.Fatal("denied typed call reached the MCP server")
	}
}
