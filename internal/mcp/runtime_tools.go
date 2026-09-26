package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// RuntimeToolContent is one content block of a runtime tool result. Text is
// set only for text blocks; other blocks keep their type so a caller can
// reject them instead of silently dropping them.
type RuntimeToolContent struct {
	Type string
	Text string
}

// RuntimeToolResult preserves the structure of an MCP CallTool result for a
// runtime action: the error flag, the raw structuredContent, and every content
// block in order.
type RuntimeToolResult struct {
	IsError           bool
	StructuredContent json.RawMessage
	Content           []RuntimeToolContent
}

// AttachClient registers an already started client as server name: it
// initializes the client, lists its tools, and applies the server's
// allowedTools/excludedTools. The manager owns cli afterwards; on failure cli
// is closed.
func (m *MCPToolManager) AttachClient(ctx context.Context, name string, cfg MCPServerConfig, cli *client.Client) error {
	if m == nil || cli == nil {
		return errors.New("attach MCP client: manager and client are required")
	}
	m.mu.RLock()
	_, exists := m.clients[name]
	m.mu.RUnlock()
	if exists {
		_ = cli.Close()
		return fmt.Errorf("attach MCP client: server %q is already loaded", name)
	}
	tools, err := initializeServerTools(ctx, name, cfg, cli)
	if err != nil {
		return fmt.Errorf("attach MCP server %q: %w", name, err)
	}
	// The check above only avoids initializing a duplicate. Another attach or
	// LoadTools may have registered the name while this one initialized.
	m.mu.Lock()
	err = m.registerServerLocked(name, cli, tools)
	m.mu.Unlock()
	if err != nil {
		_ = cli.Close()
		return fmt.Errorf("attach MCP client: %w", err)
	}
	return nil
}

// ReserveRuntimeTool binds the native tool of server to a runtime action. The
// match is exact on both names; a tool of the same name on another server does
// not match. A reserved tool leaves every model-facing surface. Reserving the
// same tool again is idempotent.
func (m *MCPToolManager) ReserveRuntimeTool(server, native string) (MCPTool, error) {
	if m == nil {
		return MCPTool{}, errors.New("MCP tool manager is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tools {
		if t.ServerName == server && t.OrigName == native {
			m.reserved[t.Name] = true
			return cloneMCPTool(t), nil
		}
	}
	if err := m.loadErrors[server]; err != nil {
		return MCPTool{}, fmt.Errorf("MCP server %q failed to load: %w", server, err)
	}
	if _, loaded := m.clients[server]; !loaded {
		return MCPTool{}, fmt.Errorf("MCP server %q is not loaded", server)
	}
	return MCPTool{}, fmt.Errorf("MCP server %q does not expose tool %q (not listed or excluded)", server, native)
}

// ExecuteRuntimeTool calls a reserved tool for a runtime action. The descriptor
// assertion and the mandatory authorizer both run before transport, so a
// mismatch or denial never reaches the server.
func (m *MCPToolManager) ExecuteRuntimeTool(ctx context.Context, logicalName, expectedDescriptorSHA256, arguments string, authorize ToolAuthorizer) (RuntimeToolResult, error) {
	if authorize == nil {
		return RuntimeToolResult{}, &toolAuthorizationError{cause: errors.New("a runtime MCP tool call requires an authorizer")}
	}
	if expectedDescriptorSHA256 == "" {
		return RuntimeToolResult{}, &toolDescriptorMismatchError{name: logicalName}
	}
	t, cli, err := m.resolveTool(logicalName, true)
	if err != nil {
		if _, missing := errors.AsType[*toolNotFoundError](err); missing {
			return RuntimeToolResult{}, &toolDescriptorMismatchError{name: logicalName}
		}
		return RuntimeToolResult{}, err
	}
	fingerprint, err := MCPToolDescriptorSHA256(t)
	if err != nil {
		return RuntimeToolResult{}, fmt.Errorf("fingerprint MCP tool %q: %w", logicalName, err)
	}
	if fingerprint != expectedDescriptorSHA256 {
		return RuntimeToolResult{}, &toolDescriptorMismatchError{name: logicalName}
	}
	if err := authorize(ctx, t.ServerName, t.OrigName, arguments); err != nil {
		return RuntimeToolResult{}, &toolAuthorizationError{cause: err}
	}
	result, err := callMCPTool(ctx, t, cli, arguments)
	if err != nil {
		return RuntimeToolResult{}, err
	}
	return newRuntimeToolResult(result)
}

func newRuntimeToolResult(result *mcp.CallToolResult) (RuntimeToolResult, error) {
	out := RuntimeToolResult{IsError: result.IsError, Content: make([]RuntimeToolContent, 0, len(result.Content))}
	switch {
	case len(result.RawStructuredContent) > 0:
		out.StructuredContent = append(json.RawMessage(nil), result.RawStructuredContent...)
	case result.StructuredContent != nil:
		// The in-process transport hands over Go values instead of JSON.
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			return RuntimeToolResult{}, fmt.Errorf("encode MCP structured content: %w", err)
		}
		out.StructuredContent = encoded
	}
	for _, content := range result.Content {
		out.Content = append(out.Content, runtimeToolContent(content))
	}
	return out, nil
}

func runtimeToolContent(content mcp.Content) RuntimeToolContent {
	if text, ok := mcp.AsTextContent(content); ok {
		return RuntimeToolContent{Type: "text", Text: text.Text}
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if encoded, err := json.Marshal(content); err == nil {
		_ = json.Unmarshal(encoded, &envelope)
	}
	return RuntimeToolContent{Type: cmp.Or(envelope.Type, "unknown")}
}
