package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

type MCPTool struct {
	Name              string
	Description       string
	InputSchema       map[string]any
	Parameters        map[string]any
	Required          []string
	ServerName        string
	OrigName          string
	WorkerPolicy      *WorkerToolPolicy
	workerInputSchema *jsonschema.Schema
}

// ToolAuthorizer is injected by the coordinator at the execution boundary so
// MCP transport cannot bypass the same policy used by built-in tools.
type ToolAuthorizer func(context.Context, string, string, string) error

type toolAuthorizationError struct{ cause error }

func (e *toolAuthorizationError) Error() string {
	return "MCP authorization denied: " + e.cause.Error()
}
func (e *toolAuthorizationError) Unwrap() error { return e.cause }

type toolDescriptorMismatchError struct{ name string }

func (e *toolDescriptorMismatchError) Error() string {
	return fmt.Sprintf("MCP tool %q descriptor no longer matches the admitted occurrence", e.name)
}

type toolNotFoundError struct{ name string }

func (e *toolNotFoundError) Error() string { return fmt.Sprintf("MCP tool %q not found", e.name) }

func IsToolAuthorizationError(err error) bool {
	_, ok := errors.AsType[*toolAuthorizationError](err)
	return ok
}

func IsToolDescriptorMismatchError(err error) bool {
	_, ok := errors.AsType[*toolDescriptorMismatchError](err)
	return ok
}

type toolAuthorizerKey struct{}

// WithToolAuthorizer binds an MCP authorization callback to a run context.
func WithToolAuthorizer(ctx context.Context, authorizer ToolAuthorizer) context.Context {
	return context.WithValue(ctx, toolAuthorizerKey{}, authorizer)
}

func toolAuthorizerFromContext(ctx context.Context) ToolAuthorizer {
	authorizer, _ := ctx.Value(toolAuthorizerKey{}).(ToolAuthorizer)
	return authorizer
}

type MCPToolManager struct {
	mu           sync.RWMutex
	tools        []MCPTool
	clients      map[string]*client.Client
	toolMap      map[string]MCPTool
	agentServers map[string]*AgentMCPServer
	// loadErrors keeps each server's LoadTools failure so a runtime binding
	// can report why a tool it needs is missing.
	loadErrors map[string]error
	// reserved names tools bound to a runtime action. They are hidden from
	// every model-facing surface and reachable only through ExecuteRuntimeTool.
	reserved    map[string]bool
	globalShell string
	teamShell   string
}

func NewMCPToolManager(globalShell, teamShell string) *MCPToolManager {
	return &MCPToolManager{
		clients:      make(map[string]*client.Client),
		toolMap:      make(map[string]MCPTool),
		agentServers: make(map[string]*AgentMCPServer),
		loadErrors:   make(map[string]error),
		reserved:     make(map[string]bool),
		globalShell:  globalShell,
		teamShell:    teamShell,
	}
}

func (m *MCPToolManager) LoadTools(ctx context.Context, servers map[string]MCPServerConfig) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var loadErrs []error
	serverErrs := make(map[string]error)

	type serverResult struct {
		name  string
		tools []MCPTool
		cli   *client.Client
	}
	var results []serverResult

	for name, cfg := range servers {
		wg.Add(1)
		go func(name string, cfg MCPServerConfig) {
			defer wg.Done()
			tools, cli, err := m.loadServer(ctx, name, cfg)
			if err != nil {
				mu.Lock()
				loadErrs = append(loadErrs, fmt.Errorf("server %q: %w", name, err))
				serverErrs[name] = err
				mu.Unlock()
				return
			}
			mu.Lock()
			results = append(results, serverResult{name: name, tools: tools, cli: cli})
			mu.Unlock()
		}(name, cfg)
	}
	wg.Wait()

	// A rejected client is closed after the lock is released: closing a stdio
	// client waits for its process.
	var rejected []*client.Client
	defer func() {
		for _, cli := range rejected {
			_ = cli.Close()
		}
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	maps.Copy(m.loadErrors, serverErrs)
	for _, r := range results {
		if err := m.registerServerLocked(r.name, r.cli, r.tools); err != nil {
			rejected = append(rejected, r.cli)
			loadErrs = append(loadErrs, fmt.Errorf("server %q: %w", r.name, err))
			m.loadErrors[r.name] = err
		}
	}

	if len(loadErrs) > 0 && len(m.tools) == 0 {
		return fmt.Errorf("all MCP servers failed: %v", loadErrs)
	}
	return nil
}

// registerServerLocked adds a loaded server's client and tools. A server
// name is registered at most once: a second client for it would replace the
// first without closing it and duplicate its tools. A successful
// registration clears any earlier load error for that server. m.mu must be
// held for writing.
func (m *MCPToolManager) registerServerLocked(name string, cli *client.Client, tools []MCPTool) error {
	if _, exists := m.clients[name]; exists {
		return fmt.Errorf("MCP server %q is already loaded", name)
	}
	m.clients[name] = cli
	for _, t := range tools {
		m.tools = append(m.tools, t)
		m.toolMap[t.Name] = t
	}
	delete(m.loadErrors, name)
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

var blockedEnvVars = map[string]bool{
	"LD_PRELOAD":            true,
	"LD_LIBRARY_PATH":       true,
	"DYLD_INSERT_LIBRARIES": true,
	"DYLD_LIBRARY_PATH":     true,
	"__AFL_PRELOAD":         true,
}

func (m *MCPToolManager) loadLocalServer(ctx context.Context, name string, cfg MCPServerConfig) ([]MCPTool, *client.Client, error) {
	if len(cfg.Command) == 0 {
		return nil, nil, fmt.Errorf("local MCP server %q requires command", name)
	}

	env := []string{}
	for k, v := range cfg.Environment {
		if blockedEnvVars[strings.ToUpper(k)] {
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	cli, err := client.NewStdioMCPClientWithOptions(cfg.Command[0], env, cfg.Command[1:])
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create stdio client: %w", err)
	}
	tools, err := initializeServerTools(ctx, name, cfg, cli)
	if err != nil {
		return nil, nil, err
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
	tools, err := initializeServerTools(ctx, name, cfg, cli)
	if err != nil {
		return nil, nil, err
	}
	return tools, cli, nil
}

// initializeServerTools initializes a connected client and lists the tools
// the server's allowedTools/excludedTools admit. It closes cli on failure.
func initializeServerTools(ctx context.Context, name string, cfg MCPServerConfig, cli *client.Client) ([]MCPTool, error) {
	if err := ValidateWorkerToolPolicies(cfg); err != nil {
		_ = cli.Close()
		return nil, err
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
		_ = cli.Close()
		return nil, fmt.Errorf("failed to initialize: %w", err)
	}

	toolsResult, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	var tools []MCPTool
	seenPolicies := make(map[string]bool)
	for _, t := range toolsResult.Tools {
		prefixedName := name + "__" + t.Name
		if !IsToolAllowed(t.Name, cfg.AllowedTools, cfg.ExcludedTools) {
			continue
		}
		inputSchema, err := captureMCPInputSchema(t.RawInputSchema, t.InputSchema)
		if err != nil {
			_ = cli.Close()
			return nil, fmt.Errorf("tool %q input schema: %w", prefixedName, err)
		}
		params := map[string]any{}
		if t.InputSchema.Properties != nil {
			params = t.InputSchema.Properties
		}
		var workerPolicy *WorkerToolPolicy
		var workerSchema *jsonschema.Schema
		if policy, ok := cfg.ToolPolicies[t.Name]; ok {
			workerPolicy = cloneWorkerToolPolicy(&policy)
			workerSchema, err = compileWorkerPolicy(*workerPolicy)
			if err != nil {
				_ = cli.Close()
				return nil, err
			}
			seenPolicies[t.Name] = true
		}
		tools = append(tools, MCPTool{
			WorkerPolicy: workerPolicy, workerInputSchema: workerSchema,
			Name:        prefixedName,
			Description: t.Description,
			InputSchema: inputSchema,
			Parameters:  params,
			Required:    t.InputSchema.Required,
			ServerName:  name,
			OrigName:    t.Name,
		})
	}
	for native := range cfg.ToolPolicies {
		if !seenPolicies[native] {
			_ = cli.Close()
			return nil, fmt.Errorf("MCP worker policy tool %q was not listed by server %q", native, name)
		}
	}
	return tools, nil
}

// IsToolAllowed reports whether a server's allowedTools/excludedTools admit
// the native tool name. Offline configuration checks use the same rule as
// server loading.
func IsToolAllowed(toolName string, allowed, excluded []string) bool {
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
	return m.SnapshotToolDescriptors()
}

// SnapshotToolDescriptors returns an immutable, name-sorted copy of the
// manager-owned MCP catalog. It deliberately carries no client handles and
// omits tools reserved for runtime actions.
func (m *MCPToolManager) SnapshotToolDescriptors() []MCPTool {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	tools := make([]MCPTool, 0, len(m.tools))
	for i := range m.tools {
		if !m.reserved[m.tools[i].Name] {
			tools = append(tools, cloneMCPTool(m.tools[i]))
		}
	}
	slices.SortFunc(tools, func(a, b MCPTool) int { return cmp.Compare(a.Name, b.Name) })
	return tools
}

const mcpDefaultTimeout = 30 * time.Second

func (m *MCPToolManager) ExecuteTool(ctx context.Context, toolName string, args string) (string, bool, error) {
	t, cli, err := m.resolveToolForExecution(toolName)
	if err != nil {
		return "", false, err
	}
	if t.WorkerPolicy != nil {
		return "", false, &toolAuthorizationError{cause: fmt.Errorf("MCP worker policy requires ExecuteAuthorizedTool")}
	}
	return executeMCPTool(ctx, t, cli, args)
}

// ExecuteAuthorizedTool is the shared direct/gateway MCP boundary. The
// descriptor assertion and current ToolAuthorizer both run before transport.
func (m *MCPToolManager) ExecuteAuthorizedTool(ctx context.Context, logicalName, expectedDescriptorSHA256, input string) (string, bool, error) {
	if expectedDescriptorSHA256 == "" {
		return "", false, &toolDescriptorMismatchError{name: logicalName}
	}
	t, cli, err := m.resolveToolForExecution(logicalName)
	if err != nil {
		if _, missing := errors.AsType[*toolNotFoundError](err); missing {
			return "", false, &toolDescriptorMismatchError{name: logicalName}
		}
		return "", false, err
	}
	fingerprint, err := MCPToolDescriptorSHA256(t)
	if err != nil {
		return "", false, fmt.Errorf("fingerprint MCP tool %q: %w", logicalName, err)
	}
	if fingerprint != expectedDescriptorSHA256 {
		return "", false, &toolDescriptorMismatchError{name: logicalName}
	}
	if err := validateWorkerPolicyArguments(t, input); err != nil {
		return "", false, &toolAuthorizationError{cause: err}
	}
	authorize := toolAuthorizerFromContext(ctx)
	if t.WorkerPolicy != nil && authorize == nil {
		return "", false, &toolAuthorizationError{cause: fmt.Errorf("MCP worker policy requires a runtime authorizer")}
	}
	if authorize != nil {
		if err := authorize(ctx, t.ServerName, t.OrigName, input); err != nil {
			return "", false, &toolAuthorizationError{cause: err}
		}
	}
	content, isError, err := executeMCPTool(ctx, t, cli, input)
	if err != nil {
		return "", false, err
	}
	// Both callers hand this text to a model, so redact it here like bash
	// output. Runtime action providers use a separate path and keep raw text.
	return utils.RedactSecrets(content), isError, nil
}

func (m *MCPToolManager) resolveToolForExecution(toolName string) (MCPTool, *client.Client, error) {
	return m.resolveTool(toolName, false)
}

// resolveTool finds a tool on the model-facing surface, or with runtime set,
// only among the tools reserved for runtime actions. The two sets are
// disjoint, so neither entry point can reach the other's tools.
func (m *MCPToolManager) resolveTool(toolName string, runtime bool) (MCPTool, *client.Client, error) {
	if m == nil {
		return MCPTool{}, nil, fmt.Errorf("MCP tool manager is unavailable")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.toolMap[toolName]
	if !ok || m.reserved[toolName] != runtime {
		return MCPTool{}, nil, &toolNotFoundError{name: toolName}
	}
	cli, ok := m.clients[t.ServerName]
	if !ok {
		return MCPTool{}, nil, fmt.Errorf("MCP server %q not connected", t.ServerName)
	}
	return cloneMCPTool(t), cli, nil
}

func executeMCPTool(ctx context.Context, t MCPTool, cli *client.Client, args string) (string, bool, error) {
	result, err := callMCPTool(ctx, t, cli, args)
	if err != nil {
		return "", false, err
	}

	var contentParts []string
	for _, c := range result.Content {
		if text, ok := c.(mcp.TextContent); ok {
			contentParts = append(contentParts, text.Text)
		}
	}

	return strings.Join(contentParts, "\n"), result.IsError, nil
}

// callMCPTool sends one CallTool request. Without a caller deadline the call
// is bounded by mcpDefaultTimeout.
func callMCPTool(ctx context.Context, t MCPTool, cli *client.Client, args string) (*mcp.CallToolResult, error) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, mcpDefaultTimeout)
		defer cancel()
	}

	argsMap, err := decodeToolArguments(args)
	if err != nil {
		return nil, err
	}

	req := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      t.OrigName,
			Arguments: argsMap,
		},
	}

	result, err := cli.CallTool(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("MCP tool call failed: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("MCP tool call returned no result")
	}
	return result, nil
}

// decodeToolArguments decodes one JSON object of tool arguments. Numbers stay
// json.Number, so the request carries each number exactly as the caller wrote
// it: decoding into float64 would silently change integers above 2^53.
func decodeToolArguments(args string) (map[string]any, error) {
	if args == "" || args == "{}" {
		// An interface containing a nil map serializes as null, which strict
		// MCP servers reject. Empty argument lists are still JSON objects.
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	var argsMap map[string]any
	if err := decoder.Decode(&argsMap); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid tool arguments: trailing data after the JSON object")
	}
	if argsMap == nil {
		return nil, fmt.Errorf("invalid tool arguments: expected JSON object")
	}
	return argsMap, nil
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
		if m.reserved[t.Name] {
			continue
		}
		tools = append(tools, &mcpAgentTool{
			tool:    cloneMCPTool(t),
			manager: m,
		})
	}
	if tools == nil {
		return []fantasy.AgentTool{}
	}
	return tools
}

// LoadAgentMCPServer loads and starts an agent's MCP server
func (m *MCPToolManager) LoadAgentMCPServer(agentName string, tools map[string]agent.MCPToolConfig, agentShell string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	server := NewAgentMCPServer(agentName, tools, m.globalShell)
	m.agentServers[agentName] = server
	return nil
}

// GetAgentMCPServer gets an agent's MCP server
func (m *MCPToolManager) GetAgentMCPServer(agentName string) *AgentMCPServer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agentServers[agentName]
}

// UnloadAgentMCPServer unloads an agent's MCP server
func (m *MCPToolManager) UnloadAgentMCPServer(agentName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.agentServers, agentName)
	return nil
}

// GetAgentMCPTools gets an agent's MCP tools list
func (m *MCPToolManager) GetAgentMCPTools(agentName string, agentShell string) []fantasy.AgentTool {
	server := m.GetAgentMCPServer(agentName)
	if server == nil {
		return nil
	}
	return server.RegisterTools(agentShell, m.teamShell, m.globalShell)
}

type mcpAgentTool struct {
	tool    MCPTool
	manager *MCPToolManager
	pOpts   fantasy.ProviderOptions
}

func (t *mcpAgentTool) Info() fantasy.ToolInfo {
	parameters, required := t.tool.Parameters, t.tool.Required
	if policy := t.tool.WorkerPolicy; policy != nil {
		parameters, _ = policy.InputSchema["properties"].(map[string]any)
		required = nil
		if fields, ok := policy.InputSchema["required"].([]any); ok {
			for _, field := range fields {
				if name, ok := field.(string); ok {
					required = append(required, name)
				}
			}
		}
	}
	return fantasy.ToolInfo{
		Name:        t.tool.Name,
		Description: t.tool.Description,
		Parameters:  parameters,
		Required:    required,
	}
}

func (t *mcpAgentTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *mcpAgentTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *mcpAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	fingerprint, err := MCPToolDescriptorSHA256(t.tool)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	content, isError, err := t.manager.ExecuteAuthorizedTool(ctx, t.tool.Name, fingerprint, call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if isError {
		return fantasy.NewTextErrorResponse(content), nil
	}
	return fantasy.NewTextResponse(content), nil
}
