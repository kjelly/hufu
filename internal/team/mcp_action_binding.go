package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kjelly/hufu/internal/mcp"
)

const (
	mcpActionBindFailed           = "mcp_action_bind_failed"
	mcpActionSchemaResourcePrefix = "hufu-mcp-action-tool:///"
)

// mcpActionBinding is the run-scoped target of an MCP action provider: the
// reserved logical tool, the descriptor digest the execution policy snapshot
// pins, and the compiled descriptor input schema.
type mcpActionBinding struct {
	manager          *mcp.MCPToolManager
	logicalName      string
	descriptorSHA256 string
	inputSchema      *jsonschema.Schema
}

func (p *mcpActionProvider) bind(binding mcpActionBinding) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.binding != nil {
		if p.binding.manager == binding.manager && p.binding.descriptorSHA256 == binding.descriptorSHA256 {
			return nil
		}
		return fmt.Errorf("%s: action provider %q is already bound to another MCP manager or descriptor", mcpActionBindFailed, p.capability)
	}
	p.binding = &binding
	return nil
}

func (p *mcpActionProvider) boundTarget() (mcpActionBinding, bool) {
	if p == nil {
		return mcpActionBinding{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.binding == nil {
		return mcpActionBinding{}, false
	}
	return *p.binding, true
}

// sessionMCPActionProviders returns the session's MCP action providers in
// capability order.
func sessionMCPActionProviders(session *TeamSession) []*mcpActionProvider {
	if session == nil || session.ProviderRegistry == nil {
		return nil
	}
	var providers []*mcpActionProvider
	for capability, config := range session.Config.ActionProviders {
		if !isMCPActionProviderConfig(config) {
			continue
		}
		if registered, ok := session.ProviderRegistry.Get(capability); ok {
			if provider, ok := registered.(*mcpActionProvider); ok {
				providers = append(providers, provider)
			}
		}
	}
	slices.SortFunc(providers, func(a, b *mcpActionProvider) int { return strings.Compare(a.capability, b.capability) })
	return providers
}

// bindPolicyTargets binds everything a runtime coordinator's execution
// policy snapshot pins: execution routes and MCP action providers.
func (c *Coordinator) bindPolicyTargets() error {
	if err := c.bindExecutionRoutes(); err != nil {
		return err
	}
	return c.bindMCPActionProviders()
}

// bindMCPActionProviders reserves each MCP action provider's tool on the
// run's MCP manager and fixes its descriptor before the execution policy
// snapshot pins it. Any provider that cannot be bound fails startup: a run
// never starts with a configured MCP action it cannot reach.
func (c *Coordinator) bindMCPActionProviders() error {
	for _, provider := range sessionMCPActionProviders(c.session) {
		if c.mcpManager == nil {
			return fmt.Errorf("%s: action provider %q requires MCP server %q, but no MCP manager is loaded", mcpActionBindFailed, provider.capability, provider.server)
		}
		tool, err := c.mcpManager.ReserveRuntimeTool(provider.server, provider.tool)
		if err != nil {
			return fmt.Errorf("%s: action provider %q: %w", mcpActionBindFailed, provider.capability, err)
		}
		digest, err := mcp.MCPToolDescriptorSHA256(tool)
		if err != nil {
			return fmt.Errorf("%s: action provider %q: fingerprint tool: %w", mcpActionBindFailed, provider.capability, err)
		}
		schema, err := compileMCPToolInputSchema(tool)
		if err != nil {
			return fmt.Errorf("%s: action provider %q: %w", mcpActionBindFailed, provider.capability, err)
		}
		if err := provider.bind(mcpActionBinding{manager: c.mcpManager, logicalName: tool.Name, descriptorSHA256: digest, inputSchema: schema}); err != nil {
			return err
		}
	}
	return nil
}

type denyMCPToolSchemaLoader struct{}

func (denyMCPToolSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("MCP tool input schemas may not load external resource %q", url)
}

// compileMCPToolInputSchema compiles the tool's full JSON Schema. The schema
// may declare its own draft; it may not load any external resource.
func compileMCPToolInputSchema(tool mcp.MCPTool) (*jsonschema.Schema, error) {
	encoded, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("encode input schema of tool %q: %w", tool.Name, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode input schema of tool %q: %w", tool.Name, err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyMCPToolSchemaLoader{})
	url := mcpActionSchemaResourcePrefix + tool.Name
	if err := compiler.AddResource(url, doc); err != nil {
		return nil, fmt.Errorf("load input schema of tool %q: %w", tool.Name, err)
	}
	schema, err := compiler.Compile(url)
	if err != nil {
		return nil, fmt.Errorf("compile input schema of tool %q: %w", tool.Name, err)
	}
	return schema, nil
}
