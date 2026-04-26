package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

type MCPTool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Required    []string
	ServerName  string
	OrigName    string
}

type MCPToolManager struct {
	mu      sync.RWMutex
	tools   []MCPTool
	clients map[string]*client.Client
	toolMap map[string]MCPTool
}

func NewMCPToolManager() *MCPToolManager {
	return &MCPToolManager{
		clients: make(map[string]*client.Client),
		toolMap: make(map[string]MCPTool),
	}
}

func (m *MCPToolManager) LoadTools(ctx context.Context, servers map[string]MCPServerConfig) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var loadErrs []error

	for name, cfg := range servers {
		wg.Add(1)
		go func(name string, cfg MCPServerConfig) {
			defer wg.Done()
			tools, cli, err := m.loadServer(ctx, name, cfg)
			if err != nil {
				mu.Lock()
				loadErrs = append(loadErrs, fmt.Errorf("server %q: %w", name, err))
				mu.Unlock()
				return
			}
			mu.Lock()
			m.clients[name] = cli
			for _, t := range tools {
				m.tools = append(m.tools, t)
				m.toolMap[t.Name] = t
			}
			mu.Unlock()
		}(name, cfg)
	}
	wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tools {
		m.toolMap[t.Name] = t
	}

	if len(loadErrs) > 0 && len(m.tools) == 0 {
		return fmt.Errorf("all MCP servers failed: %v", loadErrs)
	}
	return nil
}

func (m *MCPToolManager) loadServer(ctx context.Context, name string, cfg MCPServerConfig) ([]MCPTool, *client.Client, error) {
	switch cfg.Type {
	case "local", "":
		return m.loadLocalServer(ctx, name, cfg)
	case "remote":
		return m.loadRemoteServer(ctx, name, cfg)
	default:
		return nil, nil, fmt.Errorf("unsupported MCP server type: %s", cfg.Type)
	}
}

func (m *MCPToolManager) loadLocalServer(ctx context.Context, name string, cfg MCPServerConfig) ([]MCPTool, *client.Client, error) {
	if len(cfg.Command) == 0 {
		return nil, nil, fmt.Errorf("local MCP server %q requires command", name)
	}

	env := []string{}
	for k, v := range cfg.Environment {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	cli, err := client.NewStdioMCPClientWithOptions(cfg.Command[0], env, cfg.Command[1:])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create stdio client: %w", err)
	}

	initReq := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "hufu",
				Version: "0.1.0",
			},
		},
	}

	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("failed to initialize: %w", err)
	}

	toolsResult, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("failed to list tools: %w", err)
	}

	var tools []MCPTool
	for _, t := range toolsResult.Tools {
		prefixedName := name + "__" + t.Name
		if !isToolAllowed(t.Name, cfg.AllowedTools, cfg.ExcludedTools) {
			continue
		}
		params := map[string]any{}
		if t.InputSchema.Properties != nil {
			params = t.InputSchema.Properties
		}
		tools = append(tools, MCPTool{
			Name:        prefixedName,
			Description: t.Description,
			Parameters:  params,
			Required:    t.InputSchema.Required,
			ServerName:  name,
			OrigName:    t.Name,
		})
	}

	return tools, cli, nil
}

func (m *MCPToolManager) loadRemoteServer(ctx context.Context, name string, cfg MCPServerConfig) ([]MCPTool, *client.Client, error) {
	if cfg.URL == "" {
		return nil, nil, fmt.Errorf("remote MCP server %q requires url", name)
	}

	cli, err := client.NewStreamableHttpClient(cfg.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create HTTP client: %w", err)
	}

	initReq := mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "hufu",
				Version: "0.1.0",
			},
		},
	}

	if _, err := cli.Initialize(ctx, initReq); err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("failed to initialize: %w", err)
	}

	toolsResult, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		cli.Close()
		return nil, nil, fmt.Errorf("failed to list tools: %w", err)
	}

	var tools []MCPTool
	for _, t := range toolsResult.Tools {
		prefixedName := name + "__" + t.Name
		if !isToolAllowed(t.Name, cfg.AllowedTools, cfg.ExcludedTools) {
			continue
		}
		params := map[string]any{}
		if t.InputSchema.Properties != nil {
			params = t.InputSchema.Properties
		}
		tools = append(tools, MCPTool{
			Name:        prefixedName,
			Description: t.Description,
			Parameters:  params,
			Required:    t.InputSchema.Required,
			ServerName:  name,
			OrigName:    t.Name,
		})
	}

	return tools, cli, nil
}

func isToolAllowed(toolName string, allowed, excluded []string) bool {
	if len(excluded) > 0 {
		for _, e := range excluded {
			if e == toolName {
				return false
			}
		}
	}
	if len(allowed) > 0 {
		for _, a := range allowed {
			if a == toolName {
				return true
			}
		}
		return false
	}
	return true
}

func (m *MCPToolManager) GetTools() []MCPTool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tools
}

const mcpDefaultTimeout = 30 * time.Second

func (m *MCPToolManager) ExecuteTool(ctx context.Context, toolName string, args string) (string, bool, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mcpDefaultTimeout)
		defer cancel()
	}

	m.mu.RLock()
	t, ok := m.toolMap[toolName]
	if !ok {
		m.mu.RUnlock()
		return "", false, fmt.Errorf("MCP tool %q not found", toolName)
	}
	cli, ok := m.clients[t.ServerName]
	if !ok {
		m.mu.RUnlock()
		return "", false, fmt.Errorf("MCP server %q not connected", t.ServerName)
	}
	m.mu.RUnlock()

	var argsMap map[string]any
	if args != "" && args != "{}" {
		if err := json.Unmarshal([]byte(args), &argsMap); err != nil {
			return "", false, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      t.OrigName,
			Arguments: argsMap,
		},
	}

	result, err := cli.CallTool(ctx, req)
	if err != nil {
		return "", false, fmt.Errorf("MCP tool call failed: %w", err)
	}

	isError := result.IsError

	var contentParts []string
	for _, c := range result.Content {
		if text, ok := c.(mcp.TextContent); ok {
			contentParts = append(contentParts, text.Text)
		}
	}

	return strings.Join(contentParts, "\n"), isError, nil
}

func (m *MCPToolManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for _, cli := range m.clients {
		if err := cli.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	m.clients = make(map[string]*client.Client)
	m.tools = nil
	m.toolMap = make(map[string]MCPTool)
	if len(errs) > 0 {
		return fmt.Errorf("errors closing MCP clients: %v", errs)
	}
	return nil
}

func (m *MCPToolManager) AsAgentTools() []fantasy.AgentTool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []fantasy.AgentTool
	for _, t := range m.tools {
		tools = append(tools, &mcpAgentTool{
			tool:    t,
			manager: m,
		})
	}
	return tools
}

type mcpAgentTool struct {
	tool    MCPTool
	manager *MCPToolManager
	pOpts   fantasy.ProviderOptions
}

func (t *mcpAgentTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        t.tool.Name,
		Description: t.tool.Description,
		Parameters:  t.tool.Parameters,
		Required:    t.tool.Required,
	}
}

func (t *mcpAgentTool) ProviderOptions() fantasy.ProviderOptions      { return t.pOpts }
func (t *mcpAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *mcpAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	content, isError, err := t.manager.ExecuteTool(ctx, t.tool.Name, call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if isError {
		return fantasy.NewTextErrorResponse(content), nil
	}
	return fantasy.NewTextResponse(content), nil
}