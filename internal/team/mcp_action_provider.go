package team

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
)

// mcpActionRuntime is the action-providers runtime that binds a capability to
// one tool of a team-declared MCP server (docs/reference/action-providers.md).
const mcpActionRuntime = "mcp"

// Reason codes that prefix every MCP action provider error.
const (
	mcpActionUnbound = "mcp_action_unbound"
)

// mcpActionProvider binds one capability to one tool of a team-declared MCP
// server. LoadTeam creates it unbound; the tool, its descriptor, and the MCP
// manager come only from team configuration, never from a model, proposal,
// catalog argument, or action payload.
type mcpActionProvider struct {
	capability       string
	server           string
	tool             string
	timeout          time.Duration
	serverConfigHash string

	mu      sync.Mutex
	binding *mcpActionBinding
}

func isMCPActionProviderConfig(config agent.ActionProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(config.Runtime), mcpActionRuntime)
}

// newMCPActionProvider validates an mcp runtime configuration against the
// team's declared MCP servers. It never connects to or starts a server.
func newMCPActionProvider(capability string, config agent.ActionProviderConfig, servers map[string]mcp.MCPServerConfig) (*mcpActionProvider, error) {
	if len(config.Command) > 0 || strings.TrimSpace(config.Dir) != "" || strings.TrimSpace(config.Source) != "" || strings.TrimSpace(config.Mode) != "" {
		return nil, fmt.Errorf("action provider %q mcp runtime does not accept command, dir, source, or mode", capability)
	}
	server := strings.TrimSpace(config.Server)
	tool := strings.TrimSpace(config.Tool)
	if server == "" || tool == "" {
		return nil, fmt.Errorf("action provider %q mcp runtime requires server and tool", capability)
	}
	serverConfig, ok := servers[server]
	if !ok {
		return nil, fmt.Errorf("action provider %q names MCP server %q, which mcp-servers does not declare", capability, server)
	}
	switch serverConfig.Type {
	case "", "local", "remote":
	default:
		return nil, fmt.Errorf("action provider %q: MCP server %q has unsupported type %q", capability, server, serverConfig.Type)
	}
	if !mcp.IsToolAllowed(tool, serverConfig.AllowedTools, serverConfig.ExcludedTools) {
		return nil, fmt.Errorf("action provider %q: MCP server %q allowedTools/excludedTools do not allow tool %q", capability, server, tool)
	}
	return &mcpActionProvider{
		capability: capability, server: server, tool: tool,
		timeout:          time.Duration(config.Timeout) * time.Second,
		serverConfigHash: mcpServerConfigHash(serverConfig),
	}, nil
}

// mcpServerConfigHash fingerprints the server settings that decide which
// process or endpoint an MCP action reaches. Environment values contribute
// only their digests, so no raw credential enters durable state.
func mcpServerConfigHash(config mcp.MCPServerConfig) string {
	type environmentValue struct {
		Name      string `json:"name"`
		ValueHash string `json:"value_hash"`
	}
	serverType := config.Type
	if serverType == "" {
		serverType = "local"
	}
	environment := make([]environmentValue, 0, len(config.Environment))
	for name, value := range config.Environment {
		environment = append(environment, environmentValue{Name: name, ValueHash: runInputHash([]byte(value))})
	}
	slices.SortFunc(environment, func(a, b environmentValue) int { return strings.Compare(a.Name, b.Name) })
	allowed := slices.Clone(config.AllowedTools)
	slices.Sort(allowed)
	excluded := slices.Clone(config.ExcludedTools)
	slices.Sort(excluded)
	encoded, err := json.Marshal(struct {
		Type          string             `json:"type"`
		Command       []string           `json:"command,omitempty"`
		URL           string             `json:"url,omitempty"`
		AllowedTools  []string           `json:"allowed_tools,omitempty"`
		ExcludedTools []string           `json:"excluded_tools,omitempty"`
		NoOAuth       bool               `json:"no_oauth,omitempty"`
		Environment   []environmentValue `json:"environment,omitempty"`
	}{serverType, config.Command, config.URL, allowed, excluded, config.NoOAuth, environment})
	if err != nil {
		return ""
	}
	return runInputHash(encoded)
}

func (p *mcpActionProvider) ProviderName() string {
	if p == nil {
		return mcpActionRuntime
	}
	return mcpActionRuntime + ":" + p.server + "/" + p.tool
}

func (p *mcpActionProvider) Validate(action Action) error {
	if p == nil {
		return fmt.Errorf("provider is not configured")
	}
	if normalizeCapability(action.Capability) != p.capability {
		return fmt.Errorf("action capability %q does not match provider capability %q", action.Capability, p.capability)
	}
	if strings.TrimSpace(action.Type) == "" {
		return fmt.Errorf("action type is required")
	}
	return fmt.Errorf("%s: action provider %q is not bound to an MCP manager", mcpActionUnbound, p.capability)
}

func (p *mcpActionProvider) Execute(_ context.Context, action Action) (interface{}, error) {
	if err := p.Validate(action); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%s: action provider %q is not bound to an MCP manager", mcpActionUnbound, p.capability)
}
