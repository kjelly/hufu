package agent

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// CollectorResponseChannel selects a data-only result collection protocol.
type CollectorResponseChannel string

const (
	CollectorResponseFunction CollectorResponseChannel = "function"
	CollectorResponseTextJSON CollectorResponseChannel = "text_json"
	CollectorResponseAuto     CollectorResponseChannel = "auto"
)

func (channel CollectorResponseChannel) Valid() bool {
	return channel == CollectorResponseFunction || channel == CollectorResponseTextJSON || channel == CollectorResponseAuto
}

func (channel *CollectorResponseChannel) UnmarshalYAML(node *yaml.Node) error {
	value := CollectorResponseChannel(node.Value)
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" || !value.Valid() {
		return fmt.Errorf("collector-response-channel must be function, text_json, or auto")
	}
	*channel = value
	return nil
}
