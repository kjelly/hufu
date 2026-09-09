package team

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
)

// BackendBinding is mutable backend session evidence for one admitted task.
// Backend is an assertion that must match the immutable ExecutionTarget.
type BackendBinding struct {
	Backend          string `json:"backend"`
	SessionID        string `json:"session_id,omitempty"`
	TurnID           string `json:"turn_id,omitempty"`
	BackendVersion   string `json:"backend_version,omitempty"`
	EffectiveTarget  string `json:"effective_target,omitempty"`
	ExecutionWorldID string `json:"execution_world_id,omitempty"`
	CWD              string `json:"cwd,omitempty"`
	SandboxMode      string `json:"sandbox_mode,omitempty"`
	ResumeSupported  bool   `json:"resume_supported,omitempty"`
}

func cloneBackendBinding(binding *BackendBinding) *BackendBinding {
	if binding == nil {
		return nil
	}
	clone := *binding
	return &clone
}

func backendBindingFromProviderBinding(binding *ProviderBinding, target execution.ExecutionTarget) *BackendBinding {
	if binding == nil && target.IsZero() {
		return nil
	}
	result := &BackendBinding{Backend: target.Backend}
	if binding == nil {
		return result
	}
	if result.Backend == "" {
		result.Backend = execution.CanonicalBackendName(binding.Provider)
	}
	result.SessionID = binding.SessionID
	result.TurnID = binding.TurnID
	result.BackendVersion = binding.ProviderVersion
	result.EffectiveTarget = binding.EffectiveModel
	result.ExecutionWorldID = binding.ExecutionWorldID
	result.CWD = binding.CWD
	result.SandboxMode = binding.SandboxMode
	result.ResumeSupported = binding.ResumeSupported
	return result
}

func providerBindingFromBackendBinding(binding *BackendBinding) *ProviderBinding {
	if binding == nil {
		return nil
	}
	return &ProviderBinding{
		Provider: binding.Backend, SessionID: binding.SessionID, TurnID: binding.TurnID,
		ProviderVersion: binding.BackendVersion, EffectiveModel: binding.EffectiveTarget,
		ExecutionWorldID: binding.ExecutionWorldID, CWD: binding.CWD,
		SandboxMode: binding.SandboxMode, ResumeSupported: binding.ResumeSupported,
	}
}

func cloneExecutionTopology(topology []execution.ExecutionTarget) []execution.ExecutionTarget {
	return slices.Clone(topology)
}

// materializeLegacyIdentityShadow restores the read-only compatibility view
// used by old in-memory scheduler/adapter callers after a canonical target is
// decoded from a checkpoint or event. It never changes the typed target and
// the compatibility fields are suppressed again by TodoItem.MarshalJSON and
// canonical task-transition payloads.
func materializeLegacyIdentityShadow(item *TodoItem) {
	if item == nil || item.ExecutionTarget.IsZero() {
		return
	}
	if strings.TrimSpace(item.Model) == "" {
		item.Model = item.ExecutionTarget.Model
	}
	if len(item.ModelTopology) == 0 && len(item.ExecutionTopology) > 0 {
		item.ModelTopology = make([]string, 0, len(item.ExecutionTopology))
		for _, target := range item.ExecutionTopology {
			item.ModelTopology = append(item.ModelTopology, target.Model)
		}
	}
	if strings.TrimSpace(item.SubagentProvider) == "" {
		item.SubagentProvider = item.ExecutionTarget.Backend
		if item.ExecutionTarget.Backend == "local" {
			item.SubagentProvider = localSubagentProviderName
		}
	}
	if item.ProviderBinding == nil && item.BackendBinding != nil {
		item.ProviderBinding = providerBindingFromBackendBinding(item.BackendBinding)
	}
}

func modelTopologyFromExecutionTargets(topology []execution.ExecutionTarget) []string {
	if len(topology) == 0 {
		return nil
	}
	models := make([]string, 0, len(topology))
	for _, target := range topology {
		models = append(models, target.String())
	}
	return models
}

// targetFromLegacyIdentity translates legacy fields only while dual-writing.
// It is not used to reinterpret historical replay, which has stricter
// evidence rules in the checked reducer/migration phase.
func targetFromLegacyIdentity(model, subagentProvider string) execution.ExecutionTarget {
	model = strings.TrimSpace(model)
	if model == "" {
		return execution.ExecutionTarget{}
	}
	backend := execution.CanonicalBackendName(subagentProvider)
	if backend != "" && backend != localSubagentProviderName {
		return execution.ExecutionTarget{Backend: backend, Model: model}
	}
	selector, err := execution.ParseExecutionSelector(model)
	if err == nil && selector.Backend != "" {
		return execution.ExecutionTarget{Backend: selector.Backend, Model: selector.Model}
	}
	return execution.ExecutionTarget{Backend: "local", Model: model}
}

func topologyFromLegacyIdentity(modelTopology []string, primary execution.ExecutionTarget, subagentProvider string) []execution.ExecutionTarget {
	if len(modelTopology) == 0 {
		if primary.IsZero() {
			return nil
		}
		return []execution.ExecutionTarget{primary}
	}
	topology := make([]execution.ExecutionTarget, 0, len(modelTopology))
	for _, model := range modelTopology {
		topology = append(topology, targetFromLegacyIdentity(model, subagentProvider))
	}
	return topology
}

func validateExecutionIdentity(target execution.ExecutionTarget, topology []execution.ExecutionTarget, binding *BackendBinding) error {
	if target.IsZero() {
		return nil // legacy-only payloads remain readable during migration.
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if len(topology) == 0 {
		return fmt.Errorf("execution target %q is absent from an empty topology", target)
	}
	found := false
	for _, candidate := range topology {
		if err := candidate.Validate(); err != nil {
			return err
		}
		if candidate == target {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("execution target %q is absent from its frozen topology", target)
	}
	if binding != nil && execution.CanonicalBackendName(binding.Backend) != target.Backend {
		return fmt.Errorf("backend binding %q does not match execution target backend %q", binding.Backend, target.Backend)
	}
	return nil
}
