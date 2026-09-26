package team

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/utils"
)

// maxMCPActionResultBytes bounds the raw structuredContent or text an MCP
// action result may carry before output canonicalization applies its own
// limits (docs/reference/action-providers.md).
const maxMCPActionResultBytes = 1024 * 1024

// mcpActionResult turns a tool result into the provider-neutral envelope.
// structuredContent, when present, must be a JSON object and becomes the
// outputs; otherwise exactly one text block becomes outputs.result. A tool
// error is never a successful output, and an MCP result declares no artifacts.
func mcpActionResult(result mcp.RuntimeToolResult) (ActionResult, error) {
	if result.IsError {
		var texts []string
		for _, content := range result.Content {
			if content.Type == "text" {
				texts = append(texts, content.Text)
			}
		}
		return ActionResult{}, fmt.Errorf("%s: %s", mcpActionToolError, utils.TruncateRunes(utils.RedactSecrets(strings.Join(texts, "\n")), 1000))
	}
	if structured := bytes.TrimSpace(result.StructuredContent); len(structured) > 0 && !bytes.Equal(structured, []byte("null")) {
		if len(structured) > maxMCPActionResultBytes {
			return ActionResult{}, fmt.Errorf("%s: structuredContent is %d bytes, above the %d byte limit", mcpActionResultInvalid, len(structured), maxMCPActionResultBytes)
		}
		decoded, err := decodeUniqueJSON(structured)
		if err != nil {
			return ActionResult{}, fmt.Errorf("%s: structuredContent is not valid JSON: %w", mcpActionResultInvalid, err)
		}
		object, ok := decoded.(map[string]any)
		if !ok {
			return ActionResult{}, fmt.Errorf("%s: structuredContent must be a JSON object", mcpActionResultInvalid)
		}
		return ActionResult{Outputs: object}, nil
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		types := make([]string, 0, len(result.Content))
		for _, content := range result.Content {
			types = append(types, content.Type)
		}
		return ActionResult{}, fmt.Errorf("%s: a result without structuredContent must have exactly one text block, got %d block(s) %v", mcpActionResultInvalid, len(result.Content), types)
	}
	text := result.Content[0].Text
	if len(text) > maxMCPActionResultBytes {
		return ActionResult{}, fmt.Errorf("%s: text result is %d bytes, above the %d byte limit", mcpActionResultInvalid, len(text), maxMCPActionResultBytes)
	}
	return ActionResult{Outputs: map[string]any{"result": text}}, nil
}

type runtimeActionMCPAuthorizerKey struct{}

func runtimeActionMCPAuthorizerFromContext(ctx context.Context) mcp.ToolAuthorizer {
	if ctx == nil {
		return nil
	}
	authorize, _ := ctx.Value(runtimeActionMCPAuthorizerKey{}).(mcp.ToolAuthorizer)
	return authorize
}

// withRuntimeActionMCPAuthorization attaches the authorizer an MCP action
// provider requires. It grants exactly the server:tool the provider calls,
// on behalf of the task's agent, through the coordinator's authorization
// policy; the native tool is never exposed to that agent.
func (c *Coordinator) withRuntimeActionMCPAuthorization(ctx context.Context, task TaskDef) context.Context {
	return context.WithValue(ctx, runtimeActionMCPAuthorizerKey{}, mcp.ToolAuthorizer(func(callCtx context.Context, server, tool, _ string) error {
		canonical := server + ":" + tool
		decision, err := c.AuthorizeMCPCall(callCtx, MCPAuthorizationRequest{
			Agent: task.Agent, Server: server, Tool: tool,
			AllowedTools: map[string]bool{canonical: true},
			FailureMode:  c.ExecutionProfile().PolicyFailureMode,
		})
		if err != nil {
			return fmt.Errorf("authorize MCP action %s: %w", canonical, err)
		}
		if decision.Code != DecisionAllow {
			return fmt.Errorf("MCP authorization denied for %s: %s", canonical, decision.Reason)
		}
		return nil
	}))
}

// providerDescriptorDigest returns the descriptor digest a bound MCP action
// provider pins for capability, for the runtime action receipt. Other
// providers have none.
func (w *runtimeWorkflow) providerDescriptorDigest(capability string) string {
	if w == nil {
		return ""
	}
	w.mu.RLock()
	registry := w.registry
	w.mu.RUnlock()
	provider, ok := registry.Get(capability)
	if !ok {
		return ""
	}
	mcpProvider, ok := provider.(*mcpActionProvider)
	if !ok {
		return ""
	}
	binding, ok := mcpProvider.boundTarget()
	if !ok {
		return ""
	}
	return binding.descriptorSHA256
}
