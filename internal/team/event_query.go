package team

import "fmt"

// EventQuery selects validated events from EventStore's in-memory cache.
// Empty fields are wildcards. Matching is exact and preserves durable order.
type EventQuery struct {
	RunID string
	Types []string
}

// QueryEvents returns defensive copies of cached events matching query.
// The query never rescans the durable log or creates a second event index.
func (es *EventStore) QueryEvents(query EventQuery) ([]RunEvent, error) {
	types := make(map[string]struct{}, len(query.Types))
	for _, eventType := range query.Types {
		types[eventType] = struct{}{}
	}

	es.lock()
	defer es.release()
	if es.path == "" {
		return nil, nil
	}
	if !es.stateValid {
		if es.stateErr != nil {
			return nil, fmt.Errorf("event store state invalid: %w", es.stateErr)
		}
		return nil, fmt.Errorf("event store state invalid")
	}
	es.cacheHitCount++

	var matches []RunEvent
	for _, event := range es.cachedEvents {
		if query.RunID != "" && event.RunID != query.RunID {
			continue
		}
		if len(types) > 0 {
			if _, ok := types[event.Type]; !ok {
				continue
			}
		}
		matches = append(matches, cloneRunEvent(event))
	}
	return matches, nil
}
