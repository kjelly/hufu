package agent

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

func (r *DecisionProfileRef) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || strings.TrimSpace(node.Value) == "" {
		return fmt.Errorf("decision profile preset must be a non-empty string")
	}
	r.Name = strings.TrimSpace(node.Value)
	if !strings.HasPrefix(r.Name, "builtin/") || !strings.Contains(r.Name, "@") {
		return fmt.Errorf("decision profile preset %q must be an exact builtin/<name>@<version> reference", r.Name)
	}
	return nil
}

func (r DecisionProfileRef) MarshalYAML() (any, error) {
	if strings.TrimSpace(r.Name) == "" {
		return nil, fmt.Errorf("decision profile preset must not be empty")
	}
	return r.Name, nil
}

func (c *DecisionConfig) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.MappingNode {
		return fmt.Errorf("decision must be a mapping")
	}
	var result DecisionConfig
	seen := make(map[string]struct{}, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if _, ok := seen[key.Value]; ok {
			return fmt.Errorf("decision field %q is duplicated", key.Value)
		}
		seen[key.Value] = struct{}{}
		switch key.Value {
		case "default-profile":
			if err := decodeDecisionYAMLNodeStrict(value, &result.DefaultProfile); err != nil {
				return err
			}
		case "profiles":
			profiles, specs, err := decodeDecisionProfilesYAML(value)
			if err != nil {
				return err
			}
			result.Profiles, result.ProfileSpecs = profiles, specs
		case "request-contract":
			if err := decodeDecisionYAMLNodeStrict(value, &result.RequestContract); err != nil {
				return err
			}
		case "routing-hints":
			if err := decodeDecisionYAMLNodeStrict(value, &result.RoutingHints); err != nil {
				return err
			}
		default:
			return fmt.Errorf("field %s not found in type agent.DecisionConfig", key.Value)
		}
	}
	materialized, err := MaterializeDecisionConfig(result, BuiltInDecisionProfileCatalog())
	if err != nil {
		return err
	}
	*c = materialized
	return nil
}

func (c DecisionConfig) MarshalYAML() (any, error) {
	materialized, err := MaterializeDecisionConfig(c, BuiltInDecisionProfileCatalog())
	if err != nil {
		return nil, err
	}
	type decisionConfigYAML struct {
		DefaultProfile  string                         `yaml:"default-profile,omitempty"`
		Profiles        map[string]DecisionProfileSpec `yaml:"profiles,omitempty"`
		RequestContract RequestContractConfig          `yaml:"request-contract,omitempty"`
		RoutingHints    []RoutingHint                  `yaml:"routing-hints,omitempty"`
	}
	return decisionConfigYAML{
		DefaultProfile: materialized.DefaultProfile, Profiles: materialized.ProfileSpecs,
		RequestContract: materialized.RequestContract, RoutingHints: materialized.RoutingHints,
	}, nil
}

func decodeDecisionProfilesYAML(node *yaml.Node) (map[string]DecisionPolicy, map[string]DecisionProfileSpec, error) {
	if node.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("decision.profiles must be a mapping")
	}
	profiles := make(map[string]DecisionPolicy, len(node.Content)/2)
	specs := make(map[string]DecisionProfileSpec, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		name, value := strings.TrimSpace(node.Content[i].Value), node.Content[i+1]
		if name == "" || name == DecisionProfileOff || strings.HasPrefix(name, "builtin/") {
			return nil, nil, fmt.Errorf("decision.profiles contains invalid reserved name %q", name)
		}
		if _, ok := specs[name]; ok {
			return nil, nil, fmt.Errorf("decision profile %q is duplicated", name)
		}
		if value.Kind != yaml.MappingNode || value.Tag == "!!null" {
			return nil, nil, fmt.Errorf("decision profile %q must be a mapping", name)
		}
		spec, err := decodeDecisionProfileSpecYAML(value)
		if err != nil {
			return nil, nil, fmt.Errorf("decision.profiles.%s: %w", name, err)
		}
		policy, _, err := resolveDeclaredDecisionProfileSpec(spec, BuiltInDecisionProfileCatalog())
		if err != nil {
			return nil, nil, fmt.Errorf("decision.profiles.%s: %w", name, err)
		}
		profiles[name], specs[name] = policy, spec
	}
	return profiles, specs, nil
}

func decodeDecisionProfileSpecYAML(node *yaml.Node) (DecisionProfileSpec, error) {
	seen := make(map[string]struct{}, len(node.Content)/2)
	isTagged := false
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if _, ok := seen[key]; ok {
			return DecisionProfileSpec{}, fmt.Errorf("field %q is duplicated", key)
		}
		seen[key] = struct{}{}
		if key == "preset" || key == "policy" {
			isTagged = true
		}
	}
	if !isTagged {
		var policy DecisionPolicy
		if err := decodeDecisionYAMLNodeStrict(node, &policy); err != nil {
			return DecisionProfileSpec{}, err
		}
		return DecisionProfileSpec{Policy: &policy}, nil
	}
	for key := range seen {
		if key != "preset" && key != "policy" {
			return DecisionProfileSpec{}, fmt.Errorf("tagged profile contains unknown or legacy field %q", key)
		}
	}
	type taggedProfile struct {
		Preset *DecisionProfileRef `yaml:"preset,omitempty"`
		Policy *DecisionPolicy     `yaml:"policy,omitempty"`
	}
	var tagged taggedProfile
	if err := decodeDecisionYAMLNodeStrict(node, &tagged); err != nil {
		return DecisionProfileSpec{}, err
	}
	spec := DecisionProfileSpec(tagged)
	if (spec.Preset == nil) == (spec.Policy == nil) {
		return DecisionProfileSpec{}, fmt.Errorf("profile spec requires exactly one of preset or policy")
	}
	return spec, nil
}

func decodeDecisionYAMLNodeStrict(node *yaml.Node, target any) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	return decoder.Decode(target)
}
