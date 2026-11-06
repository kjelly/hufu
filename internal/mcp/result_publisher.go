package mcp

import (
	"context"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/utils"
)

// ToolResultPublisher is a trusted host hook, invoked only after descriptor
// and tool authorization checks and the completed MCP call. It must preserve
// provenance and return reference-only metadata, never raw image responses.
type ToolResultPublisher func(context.Context, MCPTool, fantasy.ToolCall, RuntimeToolResult) (fantasy.ToolResponse, error)

type toolResultPublisherKey struct{}

func WithToolResultPublisher(ctx context.Context, publisher ToolResultPublisher) context.Context {
	return context.WithValue(ctx, toolResultPublisherKey{}, publisher)
}

// PublishToolResult shares result publication between direct and gateway
// callers. Publication failure ends the tool turn so the completed operation
// cannot be silently retried by the model. The host hook owns durable failure
// diagnostics; only a content-free error is returned to the model here.
func PublishToolResult(ctx context.Context, tool MCPTool, call fantasy.ToolCall, result RuntimeToolResult) (fantasy.ToolResponse, error) {
	if publisher, _ := ctx.Value(toolResultPublisherKey{}).(ToolResultPublisher); publisher != nil {
		response, err := publisher(ctx, cloneMCPTool(tool), call, result)
		if err == nil && response.Data == nil && response.MediaType == "" && response.Type != "image" && response.Type != "media" {
			return response, nil
		}
		response = fantasy.NewTextErrorResponse("MCP tool result could not be safely published.")
		response.StopTurn = true
		return response, nil
	}
	text, isError, err := textOnlyToolResult(result)
	if err != nil {
		response := fantasy.NewTextErrorResponse(ErrUnsupportedToolContent.Error())
		response.StopTurn = true
		return response, nil
	}
	text = utils.RedactSecrets(text)
	if isError {
		return fantasy.NewTextErrorResponse(text), nil
	}
	return fantasy.NewTextResponse(text), nil
}
