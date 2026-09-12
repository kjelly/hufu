package executioncompat

import (
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
)

// ClassificationResult is the pure result for one task or policy subject.
type ClassificationResult struct {
	Classification Classification
	Features       []Feature
	ReasonCode     string
}

// ClassifyTask determines whether immutable workspace evidence describes one
// canonical execution identity. It deliberately never accepts current config,
// provider availability, or a caller-selected default as evidence.
func ClassifyTask(input TaskInput) ClassificationResult {
	features := featureSet{}
	candidates := make(map[string]struct{})
	applicable := false

	addBackend := func(backend string) {
		backend = strings.TrimSpace(backend)
		if backend == "" {
			return
		}
		if IsLegacyLocalAlias(backend) {
			features.add(FeatureLocalAlias)
		}
		canonical, _ := CanonicalBackend(backend)
		if canonical != "" {
			candidates[canonical] = struct{}{}
		}
	}
	addTarget := func(target Target) (invalid bool) {
		backend := strings.TrimSpace(target.Backend)
		model := strings.TrimSpace(target.Model)
		if backend == "" && model == "" {
			return false
		}
		applicable = true
		if backend == "" || model == "" {
			return true
		}
		addBackend(backend)
		return false
	}

	if addTarget(input.Target) {
		return result(ClassificationUnmigratable, features, "invalid_typed_target")
	}
	for _, target := range input.Topology {
		if addTarget(target) {
			return result(ClassificationUnmigratable, features, "invalid_typed_topology")
		}
	}
	if backend := strings.TrimSpace(input.BackendBinding); backend != "" {
		applicable = true
		addBackend(backend)
	}
	if provider := strings.TrimSpace(input.ProviderBinding); provider != "" {
		applicable = true
		features.add(FeatureLegacyProviderBinding)
		addBackend(provider)
	}
	if provider := strings.TrimSpace(input.SubagentProvider); provider != "" {
		applicable = true
		features.add(FeatureProviderShadowFields)
		if strings.EqualFold(provider, "hufu-local") {
			addBackend(execution.OllamaBackendName)
		} else {
			addBackend(provider)
		}
	}
	for _, receipt := range input.Receipts {
		backend := strings.TrimSpace(receipt.Backend)
		provider := strings.TrimSpace(receipt.SubagentProvider)
		if backend != "" {
			applicable = true
			addBackend(backend)
		}
		if provider != "" {
			applicable = true
			features.add(FeatureLegacyReceiptProvider)
			if strings.EqualFold(provider, "hufu-local") && backend != "" {
				addBackend(backend)
			} else if strings.EqualFold(provider, "hufu-local") {
				addBackend(execution.OllamaBackendName)
			} else {
				addBackend(provider)
			}
		}
	}

	model := strings.TrimSpace(input.Model)
	if model != "" {
		applicable = true
		features.add(FeatureProviderShadowFields)
		if backend, known := backendFromHistoricalModel(model); known {
			addBackend(backend)
		} else if len(candidates) == 0 {
			return result(ClassificationAmbiguous, features, "qualified_model_without_durable_backend")
		}
	}
	if !applicable {
		return result(ClassificationNotApplicable, features, "")
	}
	if len(candidates) == 0 {
		return result(ClassificationUnmigratable, features, "missing_execution_identity")
	}
	if len(candidates) != 1 {
		return result(ClassificationAmbiguous, features, "conflicting_durable_backend")
	}
	if len(features) == 0 {
		return result(ClassificationCanonical, features, "")
	}
	return result(ClassificationMigratable, features, "canonical_identity_derivable")
}

// ClassifyPolicySnapshot validates v3 legacy routes from only their durable
// data. It treats v4 as canonical and does not infer routes from current team
// configuration.
func ClassifyPolicySnapshot(input PolicyInput) ClassificationResult {
	features := featureSet{}
	if input.Version >= 4 {
		for _, route := range input.Routes {
			if strings.TrimSpace(route.Model) == "" || strings.TrimSpace(route.Backend) == "" || strings.TrimSpace(route.LegacyProvider) != "" {
				if strings.TrimSpace(route.LegacyProvider) != "" {
					features.add(FeatureLegacyPolicyRoute)
				}
				return result(ClassificationUnmigratable, features, "invalid_v4_policy_route")
			}
		}
		return result(ClassificationCanonical, features, "")
	}
	if input.Version != 3 {
		return result(ClassificationUnmigratable, features, "unsupported_policy_snapshot_version")
	}
	for _, route := range input.Routes {
		if strings.TrimSpace(route.Model) == "" || strings.TrimSpace(route.Backend) == "" {
			return result(ClassificationUnmigratable, features, "invalid_policy_route")
		}
		if IsLegacyLocalAlias(route.Backend) {
			features.add(FeatureLocalAlias)
		}
		if legacy := strings.TrimSpace(route.LegacyProvider); legacy != "" {
			features.add(FeatureLegacyPolicyRoute)
			if IsLegacyLocalAlias(legacy) {
				features.add(FeatureLocalAlias)
			}
			backend, _ := CanonicalBackend(route.Backend)
			legacyBackend, _ := CanonicalBackend(legacy)
			if backend == "" || backend != legacyBackend {
				return result(ClassificationAmbiguous, features, "conflicting_policy_route_backend")
			}
		}
	}
	return result(ClassificationMigratable, features, "canonical_policy_route_derivable")
}

func backendFromHistoricalModel(model string) (string, bool) {
	qualifier, _, qualified := strings.Cut(strings.ToLower(strings.TrimSpace(model)), "/")
	if !qualified {
		return execution.OllamaBackendName, true
	}
	if qualifier == execution.LegacyLocalBackendName || qualifier == execution.OllamaBackendName {
		return execution.OllamaBackendName, true
	}
	return "", false
}

type featureSet map[Feature]struct{}

func (set featureSet) add(feature Feature) { set[feature] = struct{}{} }

func result(classification Classification, features featureSet, reason string) ClassificationResult {
	ordered := make([]Feature, 0, len(features))
	for feature := range features {
		ordered = append(ordered, feature)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ClassificationResult{Classification: classification, Features: ordered, ReasonCode: reason}
}
