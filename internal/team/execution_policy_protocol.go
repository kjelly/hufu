package team

import (
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// Historical v3-v6 hashes retain their original serialization. Explicit
// transport selection uses v7 (full views) or v8 (selected views).
const (
	explicitProtocolFullPolicyVersion     = 7
	explicitProtocolSelectedPolicyVersion = 8
)

// SupportsInvocationAccounting reports the snapshot versions that freeze the
// universal provider commit and cost admission boundary.
func (s *ExecutionPolicySnapshot) SupportsInvocationAccounting() bool {
	return s != nil && supportedExecutionPolicySnapshotVersion(s.Version) && s.Version >= executionPolicySnapshotVersion
}

func isSelectedDependencyPolicyVersion(version int) bool {
	return version == selectedDependencyPolicyVersion || version == explicitProtocolSelectedPolicyVersion
}

func explicitProtocolPolicyVersion(c *Coordinator, version int) int {
	if c == nil || c.providerManager == nil {
		return version
	}
	for _, ref := range c.providerManager.EffectiveProviderRefs() {
		protocol, err := c.providerManager.ResolveAPIProtocol(ref.Name + "/execution-policy")
		if err == nil && protocol.Requested != "" {
			if isSelectedDependencyPolicyVersion(version) {
				return explicitProtocolSelectedPolicyVersion
			}
			return explicitProtocolFullPolicyVersion
		}
	}
	return version
}

func frozenProviderProtocol(c *Coordinator, model string, version int) (agent.APIProtocolResolution, error) {
	protocol, err := c.providerManager.ResolveAPIProtocol(model)
	if err != nil {
		return agent.APIProtocolResolution{}, err
	}
	if protocol.Requested == "" {
		return agent.APIProtocolResolution{}, nil
	}
	if version < explicitProtocolFullPolicyVersion {
		return agent.APIProtocolResolution{}, fmt.Errorf("historical execution policy cannot pin API protocol")
	}
	return protocol, nil
}

func validateExecutionProtocolPolicy(snapshot *ExecutionPolicySnapshot) error {
	backends := make(map[string]ExecutionBackendPolicySnapshot, len(snapshot.Backends))
	validate := func(protocol agent.APIProtocolResolution) error {
		if protocol == (agent.APIProtocolResolution{}) {
			return nil
		}
		canonical, err := agent.ResolveAPIProtocol(protocol.Requested)
		if err != nil || protocol.Requested == "" || canonical != protocol || snapshot.Version < explicitProtocolFullPolicyVersion {
			return fmt.Errorf("execution policy snapshot contains invalid API protocol binding")
		}
		return nil
	}
	for _, backend := range snapshot.Backends {
		if err := validate(backend.APIProtocol); err != nil {
			return err
		}
		if backend.Kind != string(execution.BackendKindLLM) && backend.APIProtocol != (agent.APIProtocolResolution{}) {
			return fmt.Errorf("agent backend cannot pin an API protocol")
		}
		backends[backend.Backend] = backend
	}
	for _, route := range snapshot.ModelRoutes {
		if err := validate(route.APIProtocol); err != nil {
			return err
		}
		if snapshot.Version >= explicitProtocolFullPolicyVersion {
			if backend, ok := backends[route.Backend]; !ok || backend.APIProtocol != route.APIProtocol {
				return fmt.Errorf("execution policy route API protocol does not match backend")
			}
		}
	}
	return nil
}

// Dynamic targets use the already frozen provider policy, then persist their
// own binding with the invocation profile before any generation is sent.
func (c *Coordinator) validateInvocationProtocol(request agent.ProviderRequest) error {
	if c == nil || c.executionPolicy == nil {
		return nil
	}
	policy, ok := c.executionPolicy.providerPolicyForModel(request.ModelID)
	if !ok || policy.APIProtocol != request.AdmissionContext.APIProtocol {
		return fmt.Errorf("provider invocation API protocol differs from frozen execution policy")
	}
	return nil
}

func freezeProviderBackendPolicies(c *Coordinator, state *executionPolicyState, version int) error {
	for _, ref := range c.providerManager.EffectiveProviderRefs() {
		backend := execution.CanonicalTargetBackendName(ref.Name)
		if backend == "" {
			continue
		}
		policy, err := c.providerManager.ResolveProviderExecutionPolicy(backend + "/execution-policy")
		if err != nil {
			return fmt.Errorf("resolve execution policy for backend %q: %w", backend, err)
		}
		identity, err := c.providerManager.ResolveProviderExecutionIdentity(backend + "/execution-policy")
		if err != nil {
			return fmt.Errorf("resolve execution identity for backend %q: %w", backend, err)
		}
		if identity.ProviderKey != policy.ProviderKey {
			return fmt.Errorf("execution identity provider mismatch for backend %q: identity=%q policy=%q", backend, identity.ProviderKey, policy.ProviderKey)
		}
		protocol, err := frozenProviderProtocol(c, backend+"/execution-policy", version)
		if err != nil {
			return err
		}
		state.backendByName[backend] = ExecutionBackendPolicySnapshot{
			APIProtocol: protocol,
			Backend:     backend, Kind: string(execution.BackendKindLLM), ProviderKey: policy.ProviderKey, MaxConcurrent: policy.MaxConcurrent, IdentityHash: identity.ConfigurationHash,
		}
	}

	return nil
}
