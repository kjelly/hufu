package team

import (
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
	"go.yaml.in/yaml/v3"
)

// YAML skips scalar unmarshallers for null. Check authored presence before
// decoding so null cannot silently become an inherited execution policy.
func validateResultTransportAuthoring(data []byte, manifest bool) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return nil
	}
	fields, err := manifestMapping(document.Content[0], make(map[*yaml.Node]bool))
	if err != nil {
		return err
	}
	if manifest {
		if version, ok := fields["apiVersion"]; ok && version[1].Value != "" {
			if spec, ok := fields["spec"]; ok {
				fields, err = manifestMapping(spec[1], make(map[*yaml.Node]bool))
				if err != nil {
					return err
				}
			}
		}
	}
	if pair, ok := fields["result-transport"]; ok {
		var transport agent.ResultTransport
		if err := transport.UnmarshalYAML(pair[1]); err != nil {
			return fmt.Errorf("result-transport: %w", err)
		}
	}
	return nil
}
