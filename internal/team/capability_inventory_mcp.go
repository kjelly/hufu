package team

import (
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/mcp"
)

func capabilityMCP(session *TeamSession) []StaticCapability {
	var result []StaticCapability
	for _, name := range slices.Sorted(maps.Keys(session.MCPServers)) {
		config := session.MCPServers[name]
		row := capabilityBase("mcp_declaration", session.Config.Name, "server:"+name, name)
		row.DeclaredTools = &CapabilityToolDeclaration{
			Allowed: capabilityNames(config.AllowedTools), Excluded: capabilityNames(config.ExcludedTools),
		}
		row.Diagnostics = []CapabilityDiagnostic{{"remote_descriptor_unavailable", "info"}}
		result = append(result, row)
		validPolicies := mcp.ValidateWorkerToolPolicies(config) == nil
		for _, tool := range slices.Sorted(maps.Keys(config.ToolPolicies)) {
			declared := capabilityBase("mcp_declaration", "server:"+capabilityLabel(name), tool, tool)
			declared.DeclaredTools = &CapabilityToolDeclaration{Allowed: []string{capabilityLabel(tool)}}
			declared.Diagnostics = []CapabilityDiagnostic{{"remote_descriptor_unavailable", "info"}}
			// This is an additional, maintainer-authored policy, not the live
			// descriptor's schema. Do not fabricate MCPToolDescriptorSHA256
			// from it or claim that the remote server implements it.
			if validPolicies {
				declared.EffectClass = "none"
				declared.DeclaredGrants = map[string][]string{"worker_policy": {"unbound_workers"}}
			} else {
				declared.Availability = "unavailable"
				declared.Diagnostics = append(declared.Diagnostics, CapabilityDiagnostic{"invalid_mcp_worker_policy", "error"})
			}
			result = append(result, declared)
		}
	}
	seen := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(session.Agents)) {
		def := session.Agents[key]
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		for _, name := range slices.Sorted(maps.Keys(def.MCPTools)) {
			row := capabilityBase("mcp_declaration", "agent:"+capabilityLabel(def.Name), name, name)
			row.DeclaredTools = &CapabilityToolDeclaration{Allowed: []string{capabilityLabel(name)}}
			row.Diagnostics = []CapabilityDiagnostic{{"local_tool_schema_unverified", "info"}}
			result = append(result, row)
		}
	}
	return result
}
