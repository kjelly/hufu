package agent

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ResultContractSpec is the authoring shape of a task result contract: a
// JSON Schema file, relative to the team directory, that a worker's
// structured_payload must satisfy. It is declared only in agent frontmatter
// or on a static team.yaml contract task, never by a coordinator payload.
type ResultContractSpec struct {
	Schema            string `yaml:"schema" json:"schema"`
	RequireStructured bool   `yaml:"require-structured" json:"require_structured,omitempty"`
}

// Clone returns an independent copy, or nil for a nil spec.
func (s *ResultContractSpec) Clone() *ResultContractSpec {
	if s == nil {
		return nil
	}
	clone := *s
	return &clone
}

// UnmarshalYAML decodes strictly. Agent frontmatter is otherwise lenient,
// but a misspelled key here (for example require_structured) would silently
// disable enforcement, so unknown keys are an error.
func (s *ResultContractSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("result-contract must be a mapping with schema and require-structured")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch key := node.Content[i].Value; key {
		case "schema", "require-structured":
		default:
			return fmt.Errorf("result-contract: unknown key %q (supported: schema, require-structured)", key)
		}
	}
	type plain ResultContractSpec
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return fmt.Errorf("result-contract: %w", err)
	}
	*s = ResultContractSpec(decoded)
	return nil
}
