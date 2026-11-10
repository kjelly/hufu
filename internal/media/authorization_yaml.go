package media

import (
	"bytes"

	"go.yaml.in/yaml/v3"
)

// Unknown restriction fields must not silently become the larger defaults.
func (p *Policy) UnmarshalYAML(node *yaml.Node) error {
	if err := validatePolicyYAMLScalars(node, make(map[*yaml.Node]bool)); err != nil {
		return err
	}
	var encoded bytes.Buffer
	encoder := yaml.NewEncoder(&encoded)
	if err := encoder.Encode(node); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	decoder := yaml.NewDecoder(&encoded)
	decoder.KnownFields(true)
	type policyWire Policy
	var wire policyWire
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	*p = Policy(wire)
	_, err := p.Resolve()
	return err
}

// YAML's integer decoder truncates floats, which could turn a fractional
// restriction into zero and select the hard ceiling. This policy has no float
// fields; reject those scalars before any coercion, including through aliases.
func validatePolicyYAMLScalars(node *yaml.Node, visited map[*yaml.Node]bool) error {
	if node == nil || visited[node] {
		return nil
	}
	visited[node] = true
	if node.ShortTag() == "!!float" {
		return Reject(EgressDenied, "media policy numbers must be YAML integers")
	}
	for _, child := range node.Content {
		if err := validatePolicyYAMLScalars(child, visited); err != nil {
			return err
		}
	}
	return validatePolicyYAMLScalars(node.Alias, visited)
}
