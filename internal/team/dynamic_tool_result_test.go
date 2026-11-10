package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
)

type gatewayTypedTestExecutor struct {
	gatewayTestExecutor
	descriptors []internalmcp.MCPTool
	result      internalmcp.RuntimeToolResult
}

func (e *gatewayTypedTestExecutor) SnapshotToolDescriptors() []internalmcp.MCPTool {
	return e.descriptors
}

func (e *gatewayTypedTestExecutor) ExecuteAuthorizedToolResult(_ context.Context, name, _ string, input string) (internalmcp.RuntimeToolResult, error) {
	e.calls++
	e.name, e.input = name, input
	return e.result, e.err
}

func TestDynamicToolGatewayPublishesTypedResultAfterAuthorization(t *testing.T) {
	for _, scenario := range []string{"published", "unsupported", "denied", "changed", "missing", "publication failure"} {
		t.Run(scenario, func(t *testing.T) {
			c := newDirectTypedCoordinator(t, "view", nil, nil)
			target := gatewayTestTarget()
			descriptor := internalmcp.MCPTool{Name: target.Name, ServerName: target.Server, OrigName: target.NativeName, InputSchema: target.InputSchema}
			var err error
			target.DescriptorSHA256, err = internalmcp.MCPToolDescriptorSHA256(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			executor := &gatewayTypedTestExecutor{descriptors: []internalmcp.MCPTool{descriptor}, result: internalmcp.RuntimeToolResult{
				Content: []internalmcp.RuntimeToolContent{{Type: "text", Text: "caption"}, {Type: "image", Data: "untrusted encoded bytes", MIMEType: "image/png"}},
			}}
			if scenario == "changed" {
				executor.descriptors[0].Description = "changed"
			} else if scenario == "missing" {
				executor.descriptors = nil
			}
			gateway := newDynamicToolGateway(c, executor, []DynamicToolTarget{target})
			ctx := gatewayTestContext(t, c, dynamicToolGatewayName, target.Name)
			if scenario == "denied" {
				ctx = gatewayTestContext(t, c, dynamicToolGatewayName)
			}
			published := 0
			if scenario != "unsupported" {
				ctx = internalmcp.WithToolResultPublisher(ctx, func(_ context.Context, tool internalmcp.MCPTool, call fantasy.ToolCall, result internalmcp.RuntimeToolResult) (fantasy.ToolResponse, error) {
					published++
					if executor.calls != 1 || tool.Name != target.Name || tool.ServerName != target.Server || call.ID != "call" || call.Name != target.Name || len(result.Content) != 2 {
						t.Fatal("publication lost descriptor, pairing or ordered content")
					}
					response := fantasy.NewTextResponse(strings.Repeat("caption ", maxDynamicGatewayOutputBytes/8+2))
					response.Metadata = "reference-only fixture metadata"
					response.StopTurn = scenario == "publication failure"
					response.IsError = response.StopTurn
					return response, nil
				})
			}
			response, err := gateway.Run(ctx, fantasy.ToolCall{ID: "call", Input: `{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1,"labels":[],"metadata":{"open":true}}}`})
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "published", "publication failure":
				if published != 1 || executor.calls != 1 || response.Metadata != "reference-only fixture metadata" || len(response.Content) > maxDynamicGatewayOutputBytes || response.IsError != (scenario == "publication failure") || response.StopTurn != response.IsError {
					t.Fatal("gateway lost publication metadata, error or bounds")
				}
			case "unsupported":
				if !response.IsError || !response.StopTurn || executor.calls != 1 || published != 0 || strings.Contains(response.Content, "untrusted encoded bytes") {
					t.Fatal("unpublished image was dropped, leaked or retried")
				}
			default:
				if !response.IsError || executor.calls != 0 || published != 0 {
					t.Fatal("unadmitted call reached execution or publication")
				}
			}
		})
	}
}
