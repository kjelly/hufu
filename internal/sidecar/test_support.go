package sidecar

import "charm.land/fantasy"

// NewSidecarForTest constructs a sidecar around a caller-supplied agent so
// integration tests can exercise the generation and observer wiring without a
// live provider.
func NewSidecarForTest(modelID string, agent fantasy.Agent) *Sidecar {
	return &Sidecar{agent: agent, modelID: modelID}
}
