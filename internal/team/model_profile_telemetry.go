package team

import (
	"encoding/json"
	"fmt"

	"github.com/kjelly/hufu/internal/modelprofile"
)

// reportModelProfileResolved persists and reports the canonical profile
// projection for a provider-bound invocation. Startup warmups do not call it.
// The status projection is published only after the durable event append
// succeeds, so callers can fail closed before constructing or invoking a
// provider model when persistence is unavailable.
func (c *Coordinator) reportModelProfileResolved(projection modelprofile.TelemetryProjection) error {
	if c == nil {
		return nil
	}
	if err := c.emitEvent(string(EventModelProfileResolved), "coordinator", "", projection); err != nil {
		return fmt.Errorf("persist model profile resolved: %w", err)
	}
	status := c.newEvent(string(EventModelProfileResolved)).
		withModel(projection.ModelID).
		withMessage(fmt.Sprintf("model profile resolved: %s/%s effective context=%d (%s)", projection.Provider, projection.ModelID, projection.Effective.Value, projection.Effective.Source))
	status.ModelProfile = &projection
	c.report(status)
	return nil
}

// LoadModelProfileTelemetry reads only model_profile_resolved events for the
// requested run. The payload is already the canonical secret-free DTO.
func LoadModelProfileTelemetry(workspace, runID string) ([]modelprofile.TelemetryProjection, error) {
	store, err := OpenEventStore(workspace)
	if err != nil {
		return nil, fmt.Errorf("open event store for model profiles: %w", err)
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return nil, fmt.Errorf("read event store for model profiles: %w", err)
	}
	profiles := make([]modelprofile.TelemetryProjection, 0)
	for _, event := range events {
		if event.Type != string(EventModelProfileResolved) || (runID != "" && event.RunID != runID) {
			continue
		}
		var projection modelprofile.TelemetryProjection
		if err := json.Unmarshal(event.Payload, &projection); err != nil {
			return nil, fmt.Errorf("decode model profile telemetry: %w", err)
		}
		profiles = append(profiles, projection)
	}
	return profiles, nil
}
