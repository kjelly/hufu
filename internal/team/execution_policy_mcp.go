package team

import (
	"fmt"
	"strings"
)

// ExecutionMCPActionProviderSnapshot pins one MCP action provider: the
// configured server and tool, a digest of the server configuration, and the
// descriptor bound at startup. It keeps no endpoint, credential, or schema.
type ExecutionMCPActionProviderSnapshot struct {
	Capability       string `json:"capability"`
	Server           string `json:"server"`
	Tool             string `json:"tool"`
	ServerConfigHash string `json:"server_config_hash"`
	DescriptorSHA256 string `json:"descriptor_sha256"`
}

// executionPolicyMCPActionProviders lists the session's MCP action providers
// in capability order. A provider is bound before the snapshot of any
// coordinator that runs actions; only a dry run, which never executes an
// action, sees an unbound provider and records an empty descriptor.
func executionPolicyMCPActionProviders(session *TeamSession) []ExecutionMCPActionProviderSnapshot {
	providers := sessionMCPActionProviders(session)
	if len(providers) == 0 {
		return nil
	}
	entries := make([]ExecutionMCPActionProviderSnapshot, 0, len(providers))
	for _, provider := range providers {
		entry := ExecutionMCPActionProviderSnapshot{
			Capability: provider.capability, Server: provider.server, Tool: provider.tool,
			ServerConfigHash: provider.serverConfigHash,
		}
		if binding, ok := provider.boundTarget(); ok {
			entry.DescriptorSHA256 = binding.descriptorSHA256
		}
		entries = append(entries, entry)
	}
	return entries
}

// validateExecutionPolicyMCPActionProviders checks a durable snapshot's MCP
// provider pins: complete, bound, and in strict capability order.
func validateExecutionPolicyMCPActionProviders(version int, entries []ExecutionMCPActionProviderSnapshot) error {
	if len(entries) > 0 && version != executionPolicySnapshotVersion {
		return fmt.Errorf("execution policy snapshot v%d cannot pin MCP action providers", version)
	}
	for index, entry := range entries {
		if strings.TrimSpace(entry.Capability) == "" || strings.TrimSpace(entry.Server) == "" || strings.TrimSpace(entry.Tool) == "" ||
			strings.TrimSpace(entry.ServerConfigHash) == "" || strings.TrimSpace(entry.DescriptorSHA256) == "" {
			return fmt.Errorf("execution policy snapshot MCP action provider %q is incomplete", entry.Capability)
		}
		if index > 0 && entries[index-1].Capability >= entry.Capability {
			return fmt.Errorf("execution policy snapshot MCP action providers are not in strict capability order")
		}
	}
	return nil
}

// checkLegacySnapshotMCPActionProviders refuses to resume MCP action
// providers from a v3 snapshot. v3 predates them, and its compatibility
// rebuild leaves them out, so it would otherwise match without pinning them.
func (c *Coordinator) checkLegacySnapshotMCPActionProviders(snapshot *ExecutionPolicySnapshot) error {
	if snapshot == nil || snapshot.Version != executionPolicyLegacySnapshotVersion || c == nil || len(sessionMCPActionProviders(c.session)) == 0 {
		return nil
	}
	return fmt.Errorf("execution policy snapshot v%d cannot pin MCP action providers; start a new session with --new", snapshot.Version)
}
