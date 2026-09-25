package team

import "encoding/json"

// actionProviderIdentity is the configuration identity of the provider bound
// to a capability. Provider is the registry's provider name, which for the
// golang runtime carries the prepared source digest, so a source change is an
// identity change. A command provider's identity is its argv, dir, and
// timeout: the script or program the argv runs is not part of it.
type actionProviderIdentity struct {
	Capability string   `json:"capability"`
	Provider   string   `json:"provider,omitempty"`
	Runtime    string   `json:"runtime,omitempty"`
	Source     string   `json:"source,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Command    []string `json:"command,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	Timeout    int64    `json:"timeout,omitzero"`
}

// configuredActionProviderIdentity returns the identity of the provider
// configured for capability, keeping capability exactly as given.
func configuredActionProviderIdentity(session *TeamSession, capability string) (actionProviderIdentity, bool) {
	if session == nil {
		return actionProviderIdentity{}, false
	}
	provider, ok := configuredActionProvider(session.Config.ActionProviders, capability)
	if !ok {
		return actionProviderIdentity{}, false
	}
	providerName := ""
	if session.ProviderRegistry != nil {
		providerName = session.ProviderRegistry.ProviderName(capability)
	}
	return actionProviderIdentity{
		Capability: capability, Provider: providerName, Runtime: provider.Runtime, Source: provider.Source, Mode: provider.Mode,
		Command: append([]string(nil), provider.Command...), Dir: provider.Dir, Timeout: provider.Timeout,
	}, true
}

// actionProviderIdentityHash hashes the capability's provider identity, or
// returns "" when no provider is configured for it.
func actionProviderIdentityHash(session *TeamSession, capability string) string {
	identity, ok := configuredActionProviderIdentity(session, capability)
	if !ok {
		return ""
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	return runInputHash(encoded)
}
