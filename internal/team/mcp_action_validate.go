package team

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// maxMCPActionPayloadBytes bounds the JSON object an MCP action sends as tool
// arguments (docs/reference/action-providers.md).
const maxMCPActionPayloadBytes = 64 * 1024

// mcpActionProviderConfig returns the mcp runtime configuration bound to
// capability, if any.
func mcpActionProviderConfig(session *TeamSession, capability string) (agent.ActionProviderConfig, bool) {
	if session == nil {
		return agent.ActionProviderConfig{}, false
	}
	config, ok := configuredActionProvider(session.Config.ActionProviders, capability)
	if !ok || !isMCPActionProviderConfig(config) {
		return agent.ActionProviderConfig{}, false
	}
	return config, true
}

// decodeMCPActionPayload decodes one JSON object without duplicate keys.
func decodeMCPActionPayload(payload string) (map[string]any, error) {
	if len(payload) > maxMCPActionPayloadBytes {
		return nil, fmt.Errorf("payload is %d bytes, above the %d byte limit", len(payload), maxMCPActionPayloadBytes)
	}
	decoded, err := decodeUniqueJSON([]byte(payload))
	if err != nil {
		return nil, fmt.Errorf("payload must be one JSON object: %w", err)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("payload must be a JSON object")
	}
	return object, nil
}

// validateMCPActionTask checks a static action task bound to an MCP provider.
// Its side effect must be declared none; an omitted class is not enough. A
// payload without input bindings is final at load time, so it is checked here.
func validateMCPActionTask(field string, task TaskDef, session *TeamSession) []ContractFinding {
	if task.Action == nil {
		return nil
	}
	if _, ok := mcpActionProviderConfig(session, task.Action.Capability); !ok {
		return nil
	}
	var findings []ContractFinding
	if task.SideEffect != SideEffectNone {
		findings = append(findings, errorFinding(field+".side_effect", FindingMCPActionSideEffectUnsupported,
			fmt.Sprintf("action capability %q uses an MCP provider, so the task must declare side_effect: none", task.Action.Capability)))
	}
	if len(task.Action.InputBindings) == 0 && len(task.ActionInputBindings) == 0 {
		if _, err := decodeMCPActionPayload(task.Action.Payload); err != nil {
			findings = append(findings, errorFinding(field+".action.payload", FindingMCPActionPayloadInvalid, err.Error()))
		}
	}
	return findings
}

// validateMCPActionCatalogEntry restricts a catalog entry bound to an MCP
// provider to side-effect none.
func validateMCPActionCatalogEntry(session *TeamSession, entry ActionCatalogEntry) []ContractFinding {
	if _, ok := mcpActionProviderConfig(session, entry.Capability); !ok || entry.SideEffect == SideEffectNone {
		return nil
	}
	return []ContractFinding{errorFinding(actionCatalogField(entry.ID, "side-effect"), FindingMCPActionSideEffectUnsupported,
		fmt.Sprintf("action %q capability %q uses an MCP provider, so the entry must be side-effect: none", entry.ID, entry.Capability))}
}

// validateMCPActionProviders rejects a run-input resolver backed by an MCP
// provider and a worker that declares a tool an MCP provider reserves.
func validateMCPActionProviders(session *TeamSession) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	for _, definition := range session.RunInputDefinitions {
		if definition.Resolver == nil {
			continue
		}
		if _, ok := mcpActionProviderConfig(session, definition.Resolver.Capability); ok {
			findings = append(findings, errorFinding(fmt.Sprintf("inputs.%s.resolver.capability", definition.Name), FindingMCPActionResolverUnsupported,
				fmt.Sprintf("run input resolver capability %q uses an MCP provider, which cannot resolve run inputs", definition.Resolver.Capability)))
		}
	}
	reserved := make(map[string]string)
	for capability, config := range session.Config.ActionProviders {
		if isMCPActionProviderConfig(config) {
			reserved[normalizedName(strings.TrimSpace(config.Server))+"__"+strings.TrimSpace(config.Tool)] = capability
		}
	}
	if len(reserved) == 0 {
		return findings
	}
	names := make([]string, 0, len(session.Agents))
	for name := range session.Agents {
		names = append(names, name)
	}
	slices.Sort(names)
	seen := make(map[*agent.AgentDef]bool)
	for _, name := range names {
		def := session.Agents[name]
		if def == nil || seen[def] {
			continue
		}
		seen[def] = true
		for declared := range strings.SplitSeq(def.Tools, ",") {
			server, tool, ok := strings.Cut(strings.TrimSpace(declared), "__")
			if !ok {
				continue
			}
			if capability, isReserved := reserved[normalizedName(server)+"__"+tool]; isReserved {
				findings = append(findings, errorFinding(fmt.Sprintf("agents.%s.tools", normalizedName(def.Name)), FindingMCPActionToolReserved,
					fmt.Sprintf("tool %q is reserved for action provider %q and cannot be granted to a worker", strings.TrimSpace(declared), capability)))
			}
		}
	}
	return findings
}
