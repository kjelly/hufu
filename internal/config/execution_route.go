package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ExecutionRouteConfig is one hufu.yaml execution route: an ordered list of
// backend-qualified worker targets and the provider failures that may move
// an attempt to the next candidate.
type ExecutionRouteConfig struct {
	Candidates []string `yaml:"candidates"`
	FallbackOn []string `yaml:"fallback-on"`
}

// UnmarshalYAML decodes strictly: a misspelled fallback-on would silently
// disable the route's fallback policy.
func (r *ExecutionRouteConfig) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("execution route must be a mapping with candidates and fallback-on")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch key := node.Content[i].Value; key {
		case "candidates", "fallback-on":
		default:
			return fmt.Errorf("execution route: unknown key %q (supported: candidates, fallback-on)", key)
		}
	}
	type plain ExecutionRouteConfig
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return fmt.Errorf("execution route: %w", err)
	}
	*r = ExecutionRouteConfig(decoded)
	return nil
}
