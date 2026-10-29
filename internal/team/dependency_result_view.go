package team

import (
	"fmt"
	"slices"

	"go.yaml.in/yaml/v3"
)

// DependencyResultViewSpec is repository-authored presentation policy. It
// never changes the accepted source result or its evidence authorization.
type DependencyResultViewSpec struct {
	Mode   string   `json:"mode" yaml:"mode"`
	Fields []string `json:"fields,omitempty" yaml:"fields,omitempty"`
}

var dependencyResultViewFields = []string{"summary", "findings", "artifacts", "files_modified", "verification"}

// UnmarshalYAML keeps nested policy decoding strict, including explicit null
// fields and YAML aliases/merge keys. Full is normalized at contract binding.
func (s *DependencyResultViewSpec) UnmarshalYAML(node *yaml.Node) error {
	fields, err := manifestMapping(node, make(map[*yaml.Node]bool))
	if err != nil {
		return fmt.Errorf("dependency-result-view: %w", err)
	}
	for key := range fields {
		if key != "mode" && key != "fields" {
			return fmt.Errorf("dependency-result-view: unknown field %q", key)
		}
	}
	mode := dependencyViewYAMLValue(fields["mode"][1])
	if mode == nil || mode.Kind != yaml.ScalarNode || mode.Tag != "!!str" {
		return fmt.Errorf("dependency-result-view requires a string mode")
	}
	decoded := DependencyResultViewSpec{Mode: mode.Value}
	if list, present := fields["fields"]; present {
		if decoded.Mode == "full" {
			return fmt.Errorf("dependency-result-view full cannot declare fields")
		}
		listValue := dependencyViewYAMLValue(list[1])
		if listValue == nil || listValue.Kind != yaml.SequenceNode {
			return fmt.Errorf("dependency-result-view fields must be an array")
		}
		for _, value := range listValue.Content {
			value = dependencyViewYAMLValue(value)
			if value == nil || value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
				return fmt.Errorf("dependency-result-view fields must contain strings")
			}
			decoded.Fields = append(decoded.Fields, value.Value)
		}
	}
	canonical, err := decoded.normalized()
	if err != nil {
		return err
	}
	if canonical == nil {
		*s = DependencyResultViewSpec{Mode: "full"}
	} else {
		*s = *canonical
	}
	return nil
}

func (s *DependencyResultViewSpec) normalized() (*DependencyResultViewSpec, error) {
	if s == nil {
		return nil, nil
	}
	switch s.Mode {
	case "full":
		if len(s.Fields) != 0 {
			return nil, fmt.Errorf("dependency-result-view full cannot declare fields")
		}
		return nil, nil
	case "selected":
		if len(s.Fields) == 0 {
			return nil, fmt.Errorf("dependency-result-view selected requires fields")
		}
	default:
		return nil, fmt.Errorf("dependency-result-view mode must be full or selected")
	}
	seen := make(map[string]bool, len(s.Fields))
	for _, field := range s.Fields {
		if !slices.Contains(dependencyResultViewFields, field) || seen[field] {
			return nil, fmt.Errorf("dependency-result-view field %q is unknown or duplicated", field)
		}
		seen[field] = true
	}
	result := &DependencyResultViewSpec{Mode: "selected"}
	for _, field := range dependencyResultViewFields {
		if seen[field] {
			result.Fields = append(result.Fields, field)
		}
	}
	return result, nil
}

func (s *DependencyResultViewSpec) clone() *DependencyResultViewSpec {
	if s == nil {
		return nil
	}
	return &DependencyResultViewSpec{Mode: s.Mode, Fields: slices.Clone(s.Fields)}
}

// Follow aliases without allowing a cycle to disguise a missing/null value.
func dependencyViewYAMLValue(node *yaml.Node) *yaml.Node {
	seen := make(map[*yaml.Node]bool)
	for node != nil && node.Kind == yaml.AliasNode {
		if seen[node] {
			return nil
		}
		seen[node] = true
		node = node.Alias
	}
	return node
}
