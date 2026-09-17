package team

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
)

// DecisionRoutingAuthoringConfig is the canonical authoring shape for
// decision routing hints. The unexported presence bit distinguishes
// decision.routing: {} from a mapping that actually authored hints.
type DecisionRoutingAuthoringConfig struct {
	Hints []agent.RoutingHint `yaml:"hints,omitempty"`

	hintsSet bool
}

// DecisionAuthoringConfig contains both the canonical manifest shape and the
// legacy spellings that are normalized into agent.DecisionConfig.
type DecisionAuthoringConfig struct {
	Profile  string                               `yaml:"profile,omitempty"`
	Profiles map[string]agent.DecisionProfileSpec `yaml:"profiles,omitempty"`
	Routing  DecisionRoutingAuthoringConfig       `yaml:"routing,omitempty"`

	// Legacy authoring only.
	DefaultProfile  string                       `yaml:"default-profile,omitempty"`
	RequestContract *agent.RequestContractConfig `yaml:"request-contract,omitempty"`
	RoutingHints    []agent.RoutingHint          `yaml:"routing-hints,omitempty"`

	profileSet        bool
	defaultProfileSet bool
	routingHintsSet   bool
	legacyContractSet bool
}

// RequestAuthoringConfig is the top-level request shape. Phase 1 parses it so
// the manifest schema is shared by both phases; Phase 2 owns its runtime
// materialization into TeamConfig.RequestContract.
type RequestAuthoringConfig struct {
	Objective       string                            `yaml:"objective"`
	SuccessCriteria []agent.RequestSuccessCriterion   `yaml:"success-criteria"`
	Constraints     []agent.RequestConstraint         `yaml:"constraints,omitempty"`
	Assumptions     []agent.RequestContractAssumption `yaml:"assumptions,omitempty"`
}

// DecisionAuthoringMetadata is read-only provenance for inspection output.
// It is never consulted by runtime decision execution.
type DecisionAuthoringMetadata struct {
	ProfileSource      string
	RoutingSource      string
	RequestSource      string
	RequestedProfile   string
	ResolvedProfileRef string
	UsedLegacyDefault  bool
	UsedLegacyHints    bool
	UsedLegacyContract bool
	UsedErgonomicAlias bool
	Deprecations       []string
}

func (r *DecisionRoutingAuthoringConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag == "!!null" {
		return fmt.Errorf("decision.routing must be a mapping")
	}
	var result DecisionRoutingAuthoringConfig
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("decision.routing field %q is duplicated", key.Value)
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "hints":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.Hints); err != nil {
				return err
			}
			result.hintsSet = true
		default:
			return fmt.Errorf("field %s not found in type team.DecisionRoutingAuthoringConfig", key.Value)
		}
	}
	*r = result
	return nil
}

func (r *RequestAuthoringConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag == "!!null" {
		return fmt.Errorf("request must be a non-null mapping")
	}
	var result RequestAuthoringConfig
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("request field %q is duplicated", key.Value)
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "objective":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.Objective); err != nil {
				return err
			}
		case "success-criteria":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.SuccessCriteria); err != nil {
				return err
			}
		case "constraints":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.Constraints); err != nil {
				return err
			}
		case "assumptions":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.Assumptions); err != nil {
				return err
			}
		default:
			return fmt.Errorf("field %s not found in type team.RequestAuthoringConfig", key.Value)
		}
	}
	*r = result
	return nil
}

func (d *DecisionAuthoringConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag == "!!null" {
		return fmt.Errorf("decision must be a mapping")
	}
	var result DecisionAuthoringConfig
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("decision field %q is duplicated", key.Value)
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "profile":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.Profile); err != nil {
				return err
			}
			result.profileSet = true
		case "profiles":
			profiles, err := decodeAuthoringProfiles(value)
			if err != nil {
				return err
			}
			result.Profiles = profiles
		case "routing":
			if err := value.Decode(&result.Routing); err != nil {
				return err
			}
		case "default-profile":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.DefaultProfile); err != nil {
				return err
			}
			result.defaultProfileSet = true
		case "request-contract":
			result.legacyContractSet = true
			if value.Tag == "!!null" {
				continue
			}
			var contract agent.RequestContractConfig
			if err := decodeAuthoringYAMLNodeStrict(value, &contract); err != nil {
				return err
			}
			result.RequestContract = &contract
		case "routing-hints":
			if err := decodeAuthoringYAMLNodeStrict(value, &result.RoutingHints); err != nil {
				return err
			}
			result.routingHintsSet = true
		default:
			return fmt.Errorf("field %s not found in type team.DecisionAuthoringConfig", key.Value)
		}
	}
	*d = result
	return nil
}

func decodeAuthoringProfiles(node *yaml.Node) (map[string]agent.DecisionProfileSpec, error) {
	if node == nil || node.Kind != yaml.MappingNode || node.Tag == "!!null" {
		return nil, fmt.Errorf("decision.profiles must be a mapping")
	}
	profiles := make(map[string]agent.DecisionProfileSpec, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		name := strings.TrimSpace(node.Content[i].Value)
		if name == "" || name == agent.DecisionProfileOff || strings.HasPrefix(name, "builtin/") {
			return nil, fmt.Errorf("decision.profiles contains invalid reserved profile name %q", name)
		}
		if _, exists := profiles[name]; exists {
			return nil, fmt.Errorf("decision profile %q is duplicated", name)
		}
		var spec agent.DecisionProfileSpec
		if err := spec.UnmarshalYAML(node.Content[i+1]); err != nil {
			return nil, fmt.Errorf("decision.profiles.%s: %w", name, err)
		}
		profiles[name] = spec
	}
	return profiles, nil
}

// NormalizeDecisionAuthoring converts canonical and legacy authoring into
// runtime decision and request configuration. Both outputs are deep copies;
// callers may safely retain them as immutable team configuration.
func NormalizeDecisionAuthoring(
	decision DecisionAuthoringConfig,
	request RequestAuthoringConfig,
	requestSet bool,
	catalog agent.DecisionProfileCatalog,
) (agent.DecisionConfig, agent.RequestContractConfig, DecisionAuthoringMetadata, error) {
	if catalog == nil {
		return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision profile catalog is unavailable")
	}
	if decision.profileSet && decision.defaultProfileSet {
		return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision_authoring_conflict: decision.profile conflicts with deprecated decision.default-profile")
	}
	if decision.Routing.hintsSet && decision.routingHintsSet {
		return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision_authoring_conflict: decision.routing.hints conflicts with deprecated decision.routing-hints")
	}

	cfg := agent.DecisionConfig{
		Profiles:     make(map[string]agent.DecisionPolicy, len(decision.Profiles)),
		ProfileSpecs: make(map[string]agent.DecisionProfileSpec, len(decision.Profiles)),
		RoutingHints: nil,
	}
	for _, name := range slices.Sorted(maps.Keys(decision.Profiles)) {
		spec := decision.Profiles[name]
		cfg.ProfileSpecs[name] = spec
		policy, _, ok, err := agent.ResolveDecisionProfileSpec(cfg, name, catalog)
		if err != nil {
			return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision.profiles.%s: %w", name, err)
		}
		if !ok {
			return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision.profiles.%s is not resolvable", name)
		}
		cfg.Profiles[name] = policy
	}

	metadata := DecisionAuthoringMetadata{}
	requested := ""
	if decision.profileSet {
		metadata.ProfileSource = "decision.profile"
		requested = strings.TrimSpace(decision.Profile)
	} else if decision.defaultProfileSet {
		metadata.ProfileSource = "decision.default-profile"
		metadata.UsedLegacyDefault = true
		requested = strings.TrimSpace(decision.DefaultProfile)
	}
	if (decision.profileSet || decision.defaultProfileSet) && requested == "" {
		return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision profile must not be blank when authored")
	}
	metadata.RequestedProfile = requested

	if decision.Routing.hintsSet {
		metadata.RoutingSource = "decision.routing.hints"
		cfg.RoutingHints = slices.Clone(decision.Routing.Hints)
	} else if decision.routingHintsSet {
		metadata.RoutingSource = "decision.routing-hints"
		metadata.UsedLegacyHints = true
		cfg.RoutingHints = slices.Clone(decision.RoutingHints)
	}
	for i, hint := range cfg.RoutingHints {
		if err := hint.Validate(); err != nil {
			return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision.routing-hints[%d]: %w", i, err)
		}
	}

	var requestContract agent.RequestContractConfig
	if requestSet && decision.legacyContractSet {
		return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("request_authoring_conflict: request conflicts with deprecated decision.request-contract")
	}
	if requestSet {
		metadata.RequestSource = "request"
		requestContract = agent.RequestContractConfig{
			Enabled:         true,
			Objective:       request.Objective,
			SuccessCriteria: slices.Clone(request.SuccessCriteria),
			Constraints:     slices.Clone(request.Constraints),
			Assumptions:     slices.Clone(request.Assumptions),
		}
		if err := requestContract.Validate(); err != nil {
			return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("request: %w", err)
		}
	} else if decision.legacyContractSet {
		metadata.RequestSource = "decision.request-contract"
		metadata.UsedLegacyContract = true
		if decision.RequestContract != nil {
			requestContract = cloneRequestContractConfig(*decision.RequestContract)
		}
		if err := requestContract.Validate(); err != nil {
			return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision.request-contract: %w", err)
		}
	}
	if metadata.UsedLegacyDefault {
		metadata.Deprecations = append(metadata.Deprecations, "decision.default-profile")
	}
	if metadata.UsedLegacyContract {
		metadata.Deprecations = append(metadata.Deprecations, "decision.request-contract")
	}
	if metadata.UsedLegacyHints {
		metadata.Deprecations = append(metadata.Deprecations, "decision.routing-hints")
	}

	if requested != "" {
		cfg.DefaultProfile = requested
		if aliasRef, isAlias := ergonomicDecisionAliases[requested]; isAlias {
			if _, local := cfg.ProfileSpecs[requested]; !local {
				metadata.UsedErgonomicAlias = true
				ref := agent.DecisionProfileRef{Name: aliasRef}
				cfg.ProfileSpecs[requested] = agent.DecisionProfileSpec{Preset: &ref}
				policy, _, err := catalog.Resolve(ref)
				if err != nil {
					return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, fmt.Errorf("decision profile %q: %w", requested, err)
				}
				cfg.Profiles[requested] = policy
			}
		}
	}

	materialized, err := agent.MaterializeDecisionConfig(cfg, catalog)
	if err != nil {
		return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, err
	}
	if requested != "" {
		if policy, metadataRef, ok, resolveErr := agent.ResolveDecisionProfileSpec(materialized, requested, catalog); resolveErr != nil {
			return agent.DecisionConfig{}, agent.RequestContractConfig{}, DecisionAuthoringMetadata{}, resolveErr
		} else if ok {
			_ = policy
			metadata.ResolvedProfileRef = metadataRef.Ref
		}
	}
	return materialized, requestContract, metadata, nil
}

func cloneRequestContractConfig(in agent.RequestContractConfig) agent.RequestContractConfig {
	out := in
	out.SuccessCriteria = slices.Clone(in.SuccessCriteria)
	out.Constraints = slices.Clone(in.Constraints)
	out.Assumptions = slices.Clone(in.Assumptions)
	return out
}

func decodeAuthoringYAMLNodeStrict(node *yaml.Node, target any) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
