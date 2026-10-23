package catalog

import "github.com/kjelly/hufu/internal/decisionrt/settings"

func resolveTransport(entry Entry, opts options) (settings.Transport, error) {
	config := opts.defaults
	explicit := settings.Transport{Backend: entry.Backend, Model: entry.Model, Endpoint: entry.Endpoint, APIKeyEnv: entry.APIKeyEnv, Timeout: entry.Timeout}
	if opts.validationOnly && opts.allowInherited {
		// Only missing inherited transport fields use placeholders, and only in
		// a validation service that cannot be used to invoke a provider.
		backend := explicit.Backend
		if backend == "" {
			backend = config.Backend
		}
		if backend == "systemone" && explicit.Model == "" && config.SystemOne.Model == "" {
			explicit.Model = "inherited-model"
		}
	}
	return settings.Resolve(config, explicit, "sidecar")
}

// Transports is a content-free diagnostic projection. Credential values are
// never stored here; environment variable names are omitted from JSON too.
func (s *Service) Transports() map[string]settings.Transport {
	result := make(map[string]settings.Transport)
	if s == nil {
		return result
	}
	for name, transport := range s.transports {
		copySources := make(map[string]string, len(transport.Sources))
		for key, source := range transport.Sources {
			copySources[key] = source
		}
		transport.Sources = copySources
		result[name] = transport
	}
	return result
}
