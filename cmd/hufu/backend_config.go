package main

import (
	"fmt"
	"maps"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

// applyConfiguredBackends translates canonical backends entries into the
// existing provider adapters before they are constructed. It is intentionally
// in-memory only, so it is safe to run in the pre-mutation setup phase.
func applyConfiguredBackends(session *team.TeamSession, global *config.Config) error {
	if session == nil {
		return fmt.Errorf("backend configuration requires a team session")
	}
	// Fold global legacy providers into the team view before checking the
	// unified namespace. Named global providers are runtime configuration too;
	// omitting them here would make a target pass selection yet fail only after
	// coordinator construction. A team entry with the identical raw key is the
	// normal configuration-layer override, while aliases are rejected below.
	legacyProviders := make(map[string]config.ProviderConfig)
	if global != nil {
		maps.Copy(legacyProviders, global.Providers)
	}
	maps.Copy(legacyProviders, session.Config.Providers)
	providers, err := canonicalLegacyProviders(legacyProviders)
	if err != nil {
		return err
	}
	agentBackends, err := canonicalLegacyAgentBackends(session.Config.SubagentProviders)
	if err != nil {
		return err
	}
	if _, exists := agentBackends["local"]; exists {
		return fmt.Errorf("reserved backend name %q is the built-in LLM backend and cannot be configured as an agent backend", "local")
	}
	for name := range providers {
		if _, exists := agentBackends[name]; exists {
			return fmt.Errorf("backend name %q is defined as both an LLM provider and an agent provider; rename the LLM provider key before enabling unified execution targets", name)
		}
	}

	backends := map[string]config.BackendConfig{}
	if global != nil {
		maps.Copy(backends, global.Backends)
		if session.Config.DefaultLLMBackend == "" && global.DefaultLLMBackend != "" {
			session.Config.DefaultLLMBackend = execution.CanonicalBackendName(global.DefaultLLMBackend)
		}
	}
	maps.Copy(backends, session.Config.Backends)
	canonicalBackends := make(map[string]config.BackendConfig, len(backends))
	for rawName, backend := range backends {
		name := execution.CanonicalBackendName(rawName)
		if name == "" {
			return fmt.Errorf("backend name is required")
		}
		if _, exists := canonicalBackends[name]; exists {
			return fmt.Errorf("backend aliases resolve to the same canonical name %q", name)
		}
		canonicalBackends[name] = backend
	}
	for name, backend := range canonicalBackends {
		if name == "local" && strings.EqualFold(strings.TrimSpace(backend.Kind), "agent") {
			return fmt.Errorf("reserved backend name %q is the built-in LLM backend and cannot be configured as an agent backend", name)
		}
		if _, exists := providers[name]; exists {
			return fmt.Errorf("backend name %q is defined by both backends and legacy providers", name)
		}
		if _, exists := agentBackends[name]; exists {
			return fmt.Errorf("backend name %q is defined by both backends and legacy subagent-providers", name)
		}
		switch strings.ToLower(strings.TrimSpace(backend.Kind)) {
		case "llm":
			if backend.Type != "" && backend.Type != "openai-compatible" && backend.Type != "ollama" {
				return fmt.Errorf("LLM backend %q has unsupported type %q", name, backend.Type)
			}
			providers[name] = config.ProviderConfig{
				ProviderURL:       backend.BaseURL,
				ProviderAPIKey:    backend.ProviderAPIKey,
				IntrospectionType: backend.Type,
				MaxConcurrent:     backend.MaxConcurrent,
			}
		case "agent":
			if backend.Type != "codex-app-server" {
				return fmt.Errorf("agent backend %q has unsupported type %q", name, backend.Type)
			}
			providerConfig := agent.SubagentProviderConfig{
				Type:           backend.Type,
				Command:        backend.Command,
				Protocol:       backend.Protocol,
				StartupTimeout: backend.StartupTimeout,
				InterruptGrace: backend.InterruptGrace,
				ShutdownGrace:  backend.ShutdownGrace,
				ExecutionWorld: backend.ExecutionWorld,
				InheritEnv:     backend.InheritEnv,
				MaxConcurrent:  backend.MaxConcurrent,
			}
			if name == "codex" {
				var err error
				providerConfig, err = mergeBuiltInCodexBackendConfig(backend, providerConfig)
				if err != nil {
					return err
				}
			}
			agentBackends[name] = providerConfig
		default:
			return fmt.Errorf("backend %q has unsupported kind %q (want llm or agent)", name, backend.Kind)
		}
	}
	session.Config.Providers = providers
	session.Config.SubagentProviders = agentBackends
	return nil
}

func canonicalLegacyProviders(source map[string]config.ProviderConfig) (map[string]config.ProviderConfig, error) {
	result := make(map[string]config.ProviderConfig, len(source))
	for rawName, provider := range source {
		name := execution.CanonicalBackendName(rawName)
		if name == "" {
			return nil, fmt.Errorf("legacy provider name is required")
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("provider aliases resolve to the same canonical backend name %q", name)
		}
		result[name] = provider
	}
	return result, nil
}

func canonicalLegacyAgentBackends(source map[string]agent.SubagentProviderConfig) (map[string]agent.SubagentProviderConfig, error) {
	result := make(map[string]agent.SubagentProviderConfig, len(source))
	for rawName, backend := range source {
		name := execution.CanonicalBackendName(rawName)
		if name == "" {
			return nil, fmt.Errorf("legacy subagent-provider name is required")
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("subagent-provider aliases resolve to the same canonical backend name %q", name)
		}
		if strings.TrimSpace(backend.Type) != "codex-app-server" {
			return nil, fmt.Errorf("legacy agent backend %q has unsupported type %q", name, backend.Type)
		}
		result[name] = backend
	}
	return result, nil
}

// mergeBuiltInCodexBackendConfig applies a canonical `backends.codex` overlay
// using syntax-presence semantics. Command and inherit-env are atomic: an
// explicit empty command is invalid, while an explicit empty environment list
// is retained as an intentional hardened allowlist.
func mergeBuiltInCodexBackendConfig(backend config.BackendConfig, override agent.SubagentProviderConfig) (agent.SubagentProviderConfig, error) {
	result := agent.SubagentProviderConfig{
		Type:           "codex-app-server",
		Command:        []string{"codex", "app-server"},
		Protocol:       "app-server-v2",
		ExecutionWorld: "local-sandbox",
		InheritEnv:     []string{"PATH", "CODEX_HOME"},
	}
	if backend.Has("command") {
		if len(override.Command) == 0 || strings.TrimSpace(override.Command[0]) == "" {
			return agent.SubagentProviderConfig{}, fmt.Errorf("codex backend command must contain an executable when explicitly configured")
		}
		result.Command = append([]string(nil), override.Command...)
	}
	if backend.Has("protocol") {
		if override.Protocol != "app-server-v2" {
			return agent.SubagentProviderConfig{}, fmt.Errorf("codex backend protocol %q is incompatible with codex-app-server", override.Protocol)
		}
		result.Protocol = override.Protocol
	}
	if backend.Has("startup-timeout") {
		result.StartupTimeout = override.StartupTimeout
	}
	if backend.Has("interrupt-grace") {
		result.InterruptGrace = override.InterruptGrace
	}
	if backend.Has("shutdown-grace") {
		result.ShutdownGrace = override.ShutdownGrace
	}
	if backend.Has("execution-world") {
		result.ExecutionWorld = override.ExecutionWorld
	}
	if backend.Has("inherit-env") {
		result.InheritEnv = cloneConfiguredStrings(override.InheritEnv)
	}
	if backend.Has("max-concurrent") {
		result.MaxConcurrent = override.MaxConcurrent
	}
	return result, nil
}

// cloneConfiguredStrings retains an explicit empty YAML list as a non-nil
// empty slice, which is distinct from an omitted list for overlay semantics.
func cloneConfiguredStrings(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	copy(result, values)
	return result
}
