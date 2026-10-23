package control

import "github.com/kjelly/hufu/internal/decisionrt/settings"

func resolveTransport(config Config, defaults settings.Config) (settings.Transport, error) {
	explicit := settings.Transport{Backend: config.Backend, Model: config.Model, Endpoint: config.Endpoint, APIKeyEnv: config.APIKeyEnv, Timeout: config.Timeout}
	// Legacy sidecar control settings never used System One fields. Keep them
	// inert even when an older configuration contains both sets of values.
	backend := explicit.Backend
	if backend == "" {
		backend = defaults.Backend
	}
	if backend == "sidecar" {
		explicit.Model, explicit.Endpoint, explicit.APIKeyEnv = "", "", ""
	}
	return settings.Resolve(defaults, explicit, "systemone")
}
