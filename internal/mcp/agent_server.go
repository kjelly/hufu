package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/anomalyco/hufu/internal/agent"
)

// ShellConfig defines shell behavior
type ShellConfig struct {
	Name    string
	Command string
	Args    []string
}

var shellConfigs = map[string]ShellConfig{
	"bash":    {Name: "bash", Command: "bash", Args: []string{"-c"}},
	"sh":      {Name: "sh", Command: "sh", Args: []string{"-c"}},
	"zsh":     {Name: "zsh", Command: "zsh", Args: []string{"-c"}},
	"fish":    {Name: "fish", Command: "fish", Args: []string{"-c"}},
	"nu":      {Name: "nu", Command: "nu", Args: []string{"-c"}},
	"nushell": {Name: "nu", Command: "nu", Args: []string{"-c"}},
}

// AgentMCPServer is a lightweight in-process MCP server for agent-specific tools
type AgentMCPServer struct {
	mu           sync.RWMutex
	agentName    string
	tools        map[string]agent.MCPToolConfig
	defaultShell string
}

// NewAgentMCPServer creates a new agent MCP server
func NewAgentMCPServer(agentName string, tools map[string]agent.MCPToolConfig, defaultShell string) *AgentMCPServer {
	return &AgentMCPServer{
		agentName:    strings.ToLower(strings.TrimSpace(agentName)),
		tools:        tools,
		defaultShell: defaultShell,
	}
}

// resolveShell resolves the final shell based on priority: tool > agent > team > global > default
func resolveShell(toolShell, agentShell, teamShell, globalShell, defaultShell string) (ShellConfig, error) {
	shell := toolShell
	if shell == "" {
		shell = agentShell
	}
	if shell == "" {
		shell = teamShell
	}
	if shell == "" {
		shell = globalShell
	}
	if shell == "" {
		shell = defaultShell
	}

	// Handle nushell alias
	if shell == "nushell" {
		shell = "nu"
	}

	// Check predefined configs
	if cfg, ok := shellConfigs[shell]; ok {
		// Verify shell exists in PATH
		_, err := exec.LookPath(cfg.Command)
		if err != nil {
			return ShellConfig{}, fmt.Errorf("shell %q not found in PATH: %w", shell, err)
		}
		return cfg, nil
	}

	// Custom shell (could be executable name or full path)
	_, err := exec.LookPath(shell)
	if err != nil {
		return ShellConfig{}, fmt.Errorf("shell %q not found in PATH: %w", shell, err)
	}

	// Default to -c argument
	return ShellConfig{
		Name:    shell,
		Command: shell,
		Args:    []string{"-c"},
	}, nil
}

// RegisterTools registers MCP tools to fantasy.AgentTool list
func (s *AgentMCPServer) RegisterTools(agentShell, teamShell, globalShell string) []fantasy.AgentTool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var agentTools []fantasy.AgentTool
	for name, cfg := range s.tools {
		desc := cfg.Desc
		if desc == "" {
			desc = fmt.Sprintf("Run %s command", name)
		}

		tool := &agentMCPServerTool{
			name:        name,
			description: desc,
			cfg:         cfg,
			server:      s,
			agentShell:  agentShell,
			teamShell:   teamShell,
			globalShell: globalShell,
		}
		agentTools = append(agentTools, tool)
	}
	return agentTools
}

// agentMCPServerTool implements fantasy.AgentTool for agent MCP server tools
type agentMCPServerTool struct {
	name        string
	description string
	cfg         agent.MCPToolConfig
	server      *AgentMCPServer
	agentShell  string
	teamShell   string
	globalShell string
}

func (t *agentMCPServerTool) Info() fantasy.ToolInfo {
	params := make(map[string]any)
	var required []string

	for _, input := range t.cfg.Inputs {
		inputSchema := map[string]any{
			"type": input.Type,
		}
		if input.Description != "" {
			inputSchema["description"] = input.Description
		}
		params[input.Name] = inputSchema
		if input.Required {
			required = append(required, input.Name)
		}
	}

	return fantasy.ToolInfo{
		Name:        t.name,
		Description: t.description,
		Parameters:  params,
		Required:    required,
	}
}

func (t *agentMCPServerTool) ProviderOptions() fantasy.ProviderOptions        { return nil }
func (t *agentMCPServerTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *agentMCPServerTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if authorize := toolAuthorizerFromContext(ctx); authorize != nil {
		if err := authorize(ctx, t.server.agentName, t.name, call.Input); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	result, err := t.server.executeTool(ctx, t.name, t.cfg, t.agentShell, t.teamShell, t.globalShell, call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if result.IsError {
		// Extract text from error result
		text := extractTextFromResult(result)
		return fantasy.NewTextErrorResponse(text), nil
	}
	text := extractTextFromResult(result)
	return fantasy.NewTextResponse(text), nil
}

// extractTextFromResult extracts text content from CallToolResult
func extractTextFromResult(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}

	var texts []string
	for _, content := range result.Content {
		if textContent, ok := content.(mcp.TextContent); ok {
			texts = append(texts, textContent.Text)
		}
	}

	return strings.Join(texts, "\n")
}

// executeTool executes a tool with the given arguments
func (s *AgentMCPServer) executeTool(ctx context.Context, toolName string, cfg agent.MCPToolConfig, agentShell, teamShell, globalShell string, input string) (*mcp.CallToolResult, error) {
	// Parse input JSON
	args := make(map[string]any)
	if input != "" {
		if err := json.Unmarshal([]byte(input), &args); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("invalid input JSON: %v", err)), nil
		}
	}

	// 1. Resolve shell (follow priority)
	shellCfg, err := resolveShell(cfg.Shell, agentShell, teamShell, globalShell, s.defaultShell)
	if err != nil {
		// Shell not found, return tool error
		return mcp.NewToolResultError(fmt.Sprintf("shell error: %v", err)), nil
	}

	// 2. Build environment variables
	env := make([]string, 0)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "V") || len(e) < 3 || e[1] < '0' || e[1] > '9' {
			env = append(env, e)
		}
	}

	for i, input := range cfg.Inputs {
		val, exists := args[input.Name]
		if !exists {
			continue
		}

		// Convert to string and validate
		sVal := fmt.Sprint(val)
		if strings.ContainsAny(sVal, "\x00\n") {
			return mcp.NewToolResultError(
				fmt.Sprintf("invalid characters in input '%s': contains null or newline", input.Name)), nil
		}

		// Set environment variable: INPUT_NAME=value
		envKey := strings.ToUpper(strings.ReplaceAll(input.Name, "-", "_"))
		env = append(env, fmt.Sprintf("%s=%s", envKey, sVal))

		// Set positional parameter: V1, V2...
		posKey := fmt.Sprintf("V%d", i+1)
		env = append(env, fmt.Sprintf("%s=%s", posKey, sVal))
	}

	// 3. Build command
	cmdArgs := append(shellCfg.Args, cfg.Cmd)
	cmd := exec.CommandContext(ctx, shellCfg.Command, cmdArgs...)
	cmd.Env = env
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}

	// 4. Execute
	output, err := cmd.CombinedOutput()
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("exit %v: %s", err, string(output))), nil
	}
	return mcp.NewToolResultText(string(output)), nil
}
