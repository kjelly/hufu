package agent

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// ResultTransport selects a local worker's statically admitted result protocol.
type ResultTransport string

const (
	ResultTransportRuntime ResultTransport = "runtime"
	ResultTransportTool    ResultTransport = "tool"
)

func (transport ResultTransport) Valid() bool {
	return transport == ResultTransportRuntime || transport == ResultTransportTool
}

// UnmarshalYAML rejects unknown, empty, and non-string authored modes before
// admission. Missing settings retain their zero value for policy inheritance.
func (transport *ResultTransport) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return fmt.Errorf("result-transport must be runtime or tool")
	}
	value := ResultTransport(node.Value)
	if !value.Valid() {
		return fmt.Errorf("invalid result-transport %q: expected runtime or tool", node.Value)
	}
	*transport = value
	return nil
}
