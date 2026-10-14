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

// LookupEvent returns the branch (the implicit main lineage for an untagged
// event) and 1-based journal position of event id from the validated cache,
// without copying the cache or reaching the database.
func (es *EventStore) LookupEvent(id string) (branchID string, sequence int, ok bool) {
	if es == nil || es.mu == nil || id == "" {
		return "", 0, false
	}
	es.lock()
	defer es.release()
	if !es.stateValid {
		return "", 0, false
	}
	for index := len(es.cachedEvents) - 1; index >= 0; index-- {
		if event := es.cachedEvents[index]; event.ID == id {
			return effectiveBranchID(event.BranchID), index + 1, true
		}
	}
	return "", 0, false
}

// lastEventOnBranch returns the ID of the latest validated event recorded on
// branchID, counting untagged events as main's.
func (es *EventStore) lastEventOnBranch(branchID string) (string, bool) {
	if es == nil || es.mu == nil {
		return "", false
	}
	es.lock()
	defer es.release()
	if !es.stateValid {
		return "", false
	}
	for index := len(es.cachedEvents) - 1; index >= 0; index-- {
		if event := es.cachedEvents[index]; effectiveBranchID(event.BranchID) == effectiveBranchID(branchID) {
			return event.ID, true
		}
	}
	return "", false
}
