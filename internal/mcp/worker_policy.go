package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"charm.land/fantasy"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// WorkerToolPolicy is a maintainer-owned declaration, never a server annotation
// or model claim. It grants an explicitly named tool to unbound workers only.
// The server is trusted to honor the declared effect and filesystem boundary;
// the additional schema is enforced locally before transport.
type WorkerToolPolicy struct {
	SideEffect  string         `json:"sideEffect" yaml:"sideEffect"`
	Filesystem  string         `json:"filesystem" yaml:"filesystem"`
	InputSchema map[string]any `json:"inputSchema" yaml:"inputSchema"`
}

type denyWorkerPolicySchemaLoader struct{}

func (denyWorkerPolicySchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("MCP worker policy schemas may not load external resource %q", url)
}

func compileWorkerPolicy(policy WorkerToolPolicy) (*jsonschema.Schema, error) {
	if policy.SideEffect != "none" || policy.Filesystem != "none" {
		return nil, fmt.Errorf("MCP worker policy requires sideEffect: none and filesystem: none")
	}
	if policy.InputSchema["type"] != "object" || policy.InputSchema["additionalProperties"] != false {
		return nil, fmt.Errorf("MCP worker policy inputSchema requires type: object and additionalProperties: false")
	}
	data, err := json.Marshal(policy.InputSchema)
	if err != nil {
		return nil, err
	}
	if len(data) > 64<<10 {
		return nil, fmt.Errorf("MCP worker policy schema exceeds 64 KiB")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(denyWorkerPolicySchemaLoader{})
	const resource = "hufu-mcp-worker-policy:///schema"
	if err := compiler.AddResource(resource, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

// ValidateWorkerToolPolicies performs offline validation without starting MCP.
func ValidateWorkerToolPolicies(config MCPServerConfig) error {
	for name, policy := range config.ToolPolicies {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) || !IsToolAllowed(name, config.AllowedTools, config.ExcludedTools) {
			return fmt.Errorf("MCP worker policy tool %q is not admitted by allowedTools/excludedTools", name)
		}
		if _, err := compileWorkerPolicy(policy); err != nil {
			return fmt.Errorf("MCP worker policy tool %q: %w", name, err)
		}
	}
	return nil
}

func cloneWorkerToolPolicy(policy *WorkerToolPolicy) *WorkerToolPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	clone.InputSchema, _ = canonicalJSONMap(policy.InputSchema)
	return &clone
}

// WorkerPolicy returns a detached declaration only for a live model-facing tool.
func (m *MCPToolManager) WorkerPolicy(name string) (*WorkerToolPolicy, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	tool, ok := m.toolMap[name]
	if !ok || m.reserved[name] || tool.WorkerPolicy == nil {
		return nil, false
	}
	return cloneWorkerToolPolicy(tool.WorkerPolicy), true
}

// BindWorkerToolPolicy checks that startup configuration describes the live
// policy compiled when the server loaded, rather than just a same-name tool.
func (m *MCPToolManager) BindWorkerToolPolicy(name string, expected WorkerToolPolicy) error {
	actual, ok := m.WorkerPolicy(name)
	if !ok {
		return fmt.Errorf("MCP worker policy tool %q is unavailable; check server startup and tool listing", name)
	}
	actualJSON, err := json.Marshal(actual)
	if err != nil {
		return err
	}
	expectedJSON, err := json.Marshal(cloneWorkerToolPolicy(&expected))
	if err != nil {
		return err
	}
	if !bytes.Equal(actualJSON, expectedJSON) {
		return fmt.Errorf("MCP worker policy tool %q differs from loaded server policy", name)
	}
	return nil
}

// OwnsAgentTool prevents a custom handler with the same name from inheriting
// the authentic manager adapter's worker policy.
func (m *MCPToolManager) OwnsAgentTool(tool fantasy.AgentTool) bool {
	adapter, ok := tool.(*mcpAgentTool)
	return ok && adapter != nil && m != nil && adapter.manager == m
}

func validateWorkerPolicyArguments(tool MCPTool, input string) error {
	if tool.workerInputSchema == nil {
		if tool.WorkerPolicy != nil {
			return fmt.Errorf("MCP worker policy is not compiled")
		}
		return nil
	}
	if len(input) > 64<<10 {
		return fmt.Errorf("MCP worker policy arguments exceed 64 KiB")
	}
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	if err := checkWorkerPolicyJSON(decoder, 0); err != nil {
		return fmt.Errorf("MCP worker policy arguments: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("MCP worker policy arguments require one JSON object")
	}
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(input))
	if err != nil {
		return fmt.Errorf("MCP worker policy arguments: %w", err)
	}
	if err := tool.workerInputSchema.Validate(value); err != nil {
		return fmt.Errorf("MCP worker policy arguments: %w", err)
	}
	return nil
}

// ValidateWorkerToolArguments checks the live maintainer schema without sending
// a request. Execution repeats this check under the descriptor authorization.
func (m *MCPToolManager) ValidateWorkerToolArguments(name, input string) error {
	m.mu.RLock()
	tool, ok := m.toolMap[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("MCP worker policy tool %q is unavailable", name)
	}
	return validateWorkerPolicyArguments(tool, input)
}

// Duplicate keys must not acquire different meanings at policy and transport.
func checkWorkerPolicyJSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds 64 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return fmt.Errorf("unexpected JSON delimiter")
	}
	seen := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON object key")
			}
			seen[name] = true
		}
		if err := checkWorkerPolicyJSON(decoder, depth+1); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}
