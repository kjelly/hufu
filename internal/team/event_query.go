package team

import (
	"fmt"
	"slices"
)

// EventQuery selects validated events from EventStore's in-memory cache.
// Empty fields are wildcards. Matching is exact and preserves durable order.
type EventQuery struct {
	RunID    string
	Types    []string
	TaskID   string
	Attempt  int
	BranchID string
}

type eventOccurrenceKey struct {
	run, task, kind string
	attempt         int
}

// QueryEvents returns defensive copies of cached events matching query.
// Exact occurrence/type queries use a header-only index rebuilt from the
// validated cache after a reload, then extended only over newly appended rows.
// Neither path rescans the durable log or retains a second copy of payloads.
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
	visit := func(event RunEvent) {
		if query.RunID != "" && event.RunID != query.RunID {
			return
		}
		if query.TaskID != "" && event.TaskID != query.TaskID || query.Attempt != 0 && event.Attempt != query.Attempt || query.BranchID != "" && effectiveBranchID(event.BranchID) != effectiveBranchID(query.BranchID) {
			return
		}
		if len(types) > 0 {
			if _, ok := types[event.Type]; !ok {
				return
			}
		}
		matches = append(matches, cloneRunEvent(event))
	}
	if query.RunID != "" && query.TaskID != "" && query.Attempt > 0 && len(types) > 0 {
		for _, position := range es.occurrencePositions(query, types) {
			visit(es.cachedEvents[position])
		}
	} else {
		for _, event := range es.cachedEvents {
			visit(event)
		}
	}
	return matches, nil
}

// Caller holds the store semaphore and has checked stateValid. All append,
// refresh and rewrite paths publish through cachedEvents; resetting the index
// with publishState/invalidateState preserves that same validation boundary.
func (es *EventStore) occurrencePositions(query EventQuery, types map[string]struct{}) []int {
	if es.occurrenceIndex == nil {
		es.occurrenceIndex = make(map[eventOccurrenceKey][]int)
	}
	for es.occurrenceIndexed < len(es.cachedEvents) {
		position := es.occurrenceIndexed
		event := es.cachedEvents[position]
		if event.TaskID != "" && event.Attempt > 0 {
			key := eventOccurrenceKey{run: event.RunID, task: event.TaskID, attempt: event.Attempt, kind: event.Type}
			es.occurrenceIndex[key] = append(es.occurrenceIndex[key], position)
		}
		es.occurrenceIndexed++
	}
	var positions []int
	for kind := range types {
		key := eventOccurrenceKey{run: query.RunID, task: query.TaskID, attempt: query.Attempt, kind: kind}
		positions = append(positions, es.occurrenceIndex[key]...)
	}
	slices.Sort(positions)
	return positions
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
