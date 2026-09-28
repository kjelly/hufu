package mcp

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/mcp"
)

func TestExecuteAuthorizedToolRedactsModelVisibleContent(t *testing.T) {
	const privateKey = "-----BEGIN RSA PRIVATE KEY-----\nMIIEsecretmaterial\n-----END RSA PRIVATE KEY-----"
	const plain = "plain result 7f3c2e9a without secret-like text"
	cases := []struct {
		name       string
		content    string
		isError    bool
		want       string
		wantAbsent string
	}{
		{name: "secret success", content: "key:\n" + privateKey, want: "[REDACTED]", wantAbsent: "MIIEsecretmaterial"},
		{name: "secret error", content: "failed with " + privateKey, isError: true, want: "[REDACTED]", wantAbsent: "MIIEsecretmaterial"},
		{name: "plain passes unchanged", content: plain, want: plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewMCPToolManager("", "")
			fake := newRuntimeTestServer(t, map[string]func(mcp.CallToolRequest) *mcp.CallToolResult{
				"lookup": func(mcp.CallToolRequest) *mcp.CallToolResult {
					result := mcp.NewToolResultText(tc.content)
					result.IsError = tc.isError
					return result
				},
			})
			attachRuntimeTestServer(t, manager, "diagnostics", MCPServerConfig{}, fake)
			visible := manager.toolMap["diagnostics__lookup"]
			digest, err := MCPToolDescriptorSHA256(visible)
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithToolAuthorizer(t.Context(), allowAll)

			content, isError, err := manager.ExecuteAuthorizedTool(ctx, visible.Name, digest, `{}`)
			if err != nil {
				t.Fatalf("gateway path: %v", err)
			}
			if isError != tc.isError || !strings.Contains(content, tc.want) || (tc.wantAbsent != "" && strings.Contains(content, tc.wantAbsent)) {
				t.Fatalf("gateway path content=%q isError=%v", content, isError)
			}
			if tc.wantAbsent == "" && content != tc.content {
				t.Fatalf("content without secrets changed: %q", content)
			}

			direct := &mcpAgentTool{tool: visible, manager: manager}
			response, err := direct.Run(ctx, fantasy.ToolCall{Input: `{}`})
			if err != nil {
				t.Fatalf("direct path: %v", err)
			}
			if response.IsError != tc.isError || !strings.Contains(response.Content, tc.want) || (tc.wantAbsent != "" && strings.Contains(response.Content, tc.wantAbsent)) {
				t.Fatalf("direct path content=%q isError=%v", response.Content, response.IsError)
			}
		})
	}
}
