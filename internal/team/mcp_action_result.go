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
// outputs; otherwise ordered text becomes outputs.result. Image inputs remain
// private and transient until the coordinator validates Outputs and publishes
// canonical media. A tool error is never a successful output.
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
	envelope := ActionResult{}
	var texts []string
	var textBytes int
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			textBytes += len(block.Text)
			if textBytes > maxMCPActionResultBytes {
				return ActionResult{}, fmt.Errorf("%s: text result exceeds the byte limit", mcpActionResultInvalid)
			}
			texts = append(texts, block.Text)
		case "image":
			if envelope.mcpMedia == nil {
				envelope.mcpMedia = &pendingMCPActionMedia{result: result}
			}
		default:
			return ActionResult{}, fmt.Errorf("%s: unsupported result content type", mcpActionResultInvalid)
		}
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
		envelope.Outputs = object
		return envelope, nil
	}
	if len(texts) > 0 {
		text := strings.Join(texts, "\n")
		if len(text) > maxMCPActionResultBytes {
			return ActionResult{}, fmt.Errorf("%s: text result exceeds the byte limit", mcpActionResultInvalid)
		}
		envelope.Outputs = map[string]any{"result": text}
		return envelope, nil
	}
	if envelope.mcpMedia != nil {
		envelope.Outputs = map[string]any{}
		return envelope, nil
	}
	return ActionResult{}, fmt.Errorf("%s: a result without structuredContent must contain text or images", mcpActionResultInvalid)
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
