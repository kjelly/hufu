package executioncompat

import (
	"fmt"
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

// TaskDerivation is the complete canonical task identity that may be
// materialized after ClassifyTask has established that the evidence is unique.
// It never contains runtime configuration or provider availability data.
type TaskDerivation struct {
	Classification ClassificationResult
	Target         Target
	Topology       []Target
	BackendBinding string
	ReceiptBackend []string
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
	addTarget := func(target Target, contributesPrimary bool) (invalid bool) {
		backend := strings.TrimSpace(target.Backend)
		model := strings.TrimSpace(target.Model)
		if backend == "" && model == "" {
			return false
		}
		applicable = true
		if backend == "" || model == "" {
			return true
		}
		if contributesPrimary {
			addBackend(backend)
		} else if IsLegacyLocalAlias(backend) {
			features.add(FeatureLocalAlias)
		}
		return false
	}

	primary := input.Target
	if primary == (Target{}) && len(input.Topology) > 0 {
		primary = input.Topology[0]
	}
	if addTarget(primary, true) {
		return result(ClassificationUnmigratable, features, "invalid_typed_target")
	}
	for _, target := range input.Topology {
		if addTarget(target, false) {
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
		if strings.EqualFold(provider, "hufu-local") {
			addBackend(execution.OllamaBackendName)
		} else {
			addBackend(provider)
		}
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
	if reason := addTaskReceiptCandidates(input, features, candidates, &applicable); reason != "" {
		return result(ClassificationAmbiguous, features, reason)
	}

	model := strings.TrimSpace(input.Model)
	if model != "" {
		applicable = true
		features.add(FeatureProviderShadowFields)
		if backend, known := backendFromHistoricalModel(model); known {
			// A bare historical model is the final fallback only. An explicit
			// local/ollama qualifier is durable identity evidence and therefore
			// still participates in conflict detection.
			if strings.Contains(model, "/") || len(candidates) == 0 {
				addBackend(backend)
			}
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
	if input.Target == (Target{}) && len(input.Topology) == 0 && strings.TrimSpace(input.Model) == "" {
		return result(ClassificationUnmigratable, features, "missing_executable_model")
	}
	if len(candidates) != 1 {
		return result(ClassificationAmbiguous, features, "conflicting_durable_backend")
	}
	if len(features) == 0 {
		return result(ClassificationCanonical, features, "")
	}
	return result(ClassificationMigratable, features, "canonical_identity_derivable")
}

func addTaskReceiptCandidates(input TaskInput, features featureSet, candidates map[string]struct{}, applicable *bool) string {
	for _, receipt := range input.Receipts {
		backend := strings.TrimSpace(receipt.Backend)
		provider := strings.TrimSpace(receipt.SubagentProvider)
		receiptCandidates := make(map[string]struct{})
		addReceiptBackend := func(value string) {
			if strings.EqualFold(value, "hufu-local") {
				value = execution.OllamaBackendName
			}
			canonical, _ := CanonicalBackend(value)
			if canonical != "" {
				receiptCandidates[canonical] = struct{}{}
			}
		}
		if backend != "" {
			*applicable = true
			addReceiptBackend(backend)
		}
		if provider != "" {
			*applicable = true
			features.add(FeatureLegacyReceiptProvider)
			addReceiptBackend(provider)
		}
		if len(receiptCandidates) != 1 {
			return "conflicting_receipt_backend"
		}
		for receiptBackend := range receiptCandidates {
			// A receipt may describe an explicit topology leaf rather than the
			// primary task target. Without such a leaf it is evidence about the
			// only execution identity and must agree with the primary evidence.
			if len(input.Topology) == 0 || !topologyHasBackend(input.Topology, receiptBackend) {
				candidates[receiptBackend] = struct{}{}
			}
		}
	}
	return ""
}

func topologyHasBackend(topology []Target, want string) bool {
	for _, target := range topology {
		backend, _ := CanonicalBackend(target.Backend)
		if backend == want {
			return true
		}
	}
	return false
}

// DeriveTask returns a canonical identity only for a canonical or uniquely
// migratable subject. Ambiguous and unmigratable source data is rejected
// rather than being supplemented from caller state.
func DeriveTask(input TaskInput) (TaskDerivation, error) {
	classification := ClassifyTask(input)
	if classification.Classification != ClassificationCanonical && classification.Classification != ClassificationMigratable {
		return TaskDerivation{Classification: classification}, fmt.Errorf("task identity is %s: %s", classification.Classification, classification.ReasonCode)
	}
	backend, err := uniqueTaskBackend(input)
	if err != nil {
		return TaskDerivation{Classification: classification}, err
	}
	primary := input.Target
	if primary == (Target{}) && len(input.Topology) > 0 {
		primary = input.Topology[0]
	}
	if primary == (Target{}) {
		model, modelErr := canonicalTaskModel(input.Model, backend)
		if modelErr != nil {
			return TaskDerivation{Classification: classification}, modelErr
		}
		primary = Target{Backend: backend, Model: model}
	} else {
		primary.Backend, _ = CanonicalBackend(primary.Backend)
	}
	result := TaskDerivation{Classification: classification, Target: primary, BackendBinding: backend}
	if len(input.Topology) == 0 {
		result.Topology = []Target{primary}
	} else {
		result.Topology = make([]Target, 0, len(input.Topology))
		for _, target := range input.Topology {
			target.Backend, _ = CanonicalBackend(target.Backend)
			if target.Model == "" {
				return TaskDerivation{Classification: classification}, fmt.Errorf("execution topology has empty model")
			}
			result.Topology = append(result.Topology, target)
		}
		if !containsTarget(result.Topology, primary) {
			return TaskDerivation{Classification: classification}, fmt.Errorf("execution topology omits primary target")
		}
	}
	for _, receipt := range input.Receipts {
		receiptBackend := strings.TrimSpace(receipt.Backend)
		if receiptBackend == "" {
			receiptBackend = strings.TrimSpace(receipt.SubagentProvider)
		}
		if strings.EqualFold(receiptBackend, "hufu-local") || receiptBackend == "" {
			receiptBackend = backend
		}
		receiptBackend, _ = CanonicalBackend(receiptBackend)
		result.ReceiptBackend = append(result.ReceiptBackend, receiptBackend)
	}
	return result, nil
}

func uniqueTaskBackend(input TaskInput) (string, error) {
	candidates := make(map[string]struct{})
	add := func(value string) {
		if value == "" {
			return
		}
		if strings.EqualFold(value, "hufu-local") {
			value = execution.OllamaBackendName
		}
		canonical, _ := CanonicalBackend(value)
		if canonical != "" {
			candidates[canonical] = struct{}{}
		}
	}
	primary := input.Target
	if primary == (Target{}) && len(input.Topology) > 0 {
		primary = input.Topology[0]
	}
	add(primary.Backend)
	add(input.BackendBinding)
	add(input.ProviderBinding)
	add(input.SubagentProvider)
	// Receipt identity is a topology-leaf fact when a typed topology is
	// present. It is deliberately not folded into the primary target here.
	if len(input.Topology) == 0 {
		for _, receipt := range input.Receipts {
			add(receipt.Backend)
			add(receipt.SubagentProvider)
		}
	}
	if backend, known := backendFromHistoricalModel(input.Model); known && (strings.Contains(input.Model, "/") || len(candidates) == 0) {
		add(backend)
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("cannot derive a unique execution backend")
	}
	for backend := range candidates {
		return backend, nil
	}
	return "", fmt.Errorf("cannot derive a unique execution backend")
}

func canonicalTaskModel(model, backend string) (string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", fmt.Errorf("missing executable model")
	}
	qualifier, remainder, qualified := strings.Cut(model, "/")
	if !qualified {
		return model, nil
	}
	canonicalQualifier, _ := CanonicalBackend(qualifier)
	if canonicalQualifier == backend && strings.TrimSpace(remainder) != "" {
		return remainder, nil
	}
	return "", fmt.Errorf("qualified model cannot be mapped to durable backend")
}

func containsTarget(targets []Target, want Target) bool {
	for _, target := range targets {
		if target == want {
			return true
		}
	}
	return false
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
