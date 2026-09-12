package context

import (
	"cmp"
	"fmt"
	"slices"
)

type experienceAggregateState struct {
	aggregate ExperienceAggregate
	tasks     map[string]struct{}
	projects  map[string]struct{}
}

// ReduceExperienceAggregates is the deterministic, in-memory counterpart of
// RebuildExperienceAggregates. It lets read-only verification compare the
// canonical event-derived expectation with SQLite without creating temporary
// tables or mutating the repository.
func ReduceExperienceAggregates(observations []ExperienceObservation) ([]ExperienceAggregate, error) {
	ordered := append([]ExperienceObservation(nil), observations...)
	slices.SortStableFunc(ordered, func(left, right ExperienceObservation) int {
		return cmp.Compare(left.IdempotencyKey, right.IdempotencyKey)
	})
	processed := make(map[string]struct{}, len(ordered))
	states := make(map[string]*experienceAggregateState)
	for _, observation := range ordered {
		if observation.IdempotencyKey == "" || observation.ContextItemID == "" || observation.PolicyVersion == "" {
			return nil, fmt.Errorf("reduce experience aggregates: observation identity is incomplete")
		}
		if observation.ObservedAt.IsZero() {
			return nil, fmt.Errorf("reduce experience aggregates: observation %q has no timestamp", observation.IdempotencyKey)
		}
		if _, duplicate := processed[observation.IdempotencyKey]; duplicate {
			continue
		}
		processed[observation.IdempotencyKey] = struct{}{}
		observation = normalizeExperienceObservation(observation)
		key := observation.PolicyVersion + "\x00" + observation.ContextItemID
		state := states[key]
		if state == nil {
			state = &experienceAggregateState{
				aggregate: ExperienceAggregate{ContextItemID: observation.ContextItemID, PolicyVersion: observation.PolicyVersion, LastObservedAt: observation.ObservedAt},
				tasks:     make(map[string]struct{}), projects: make(map[string]struct{}),
			}
			states[key] = state
		}
		aggregate := &state.aggregate
		aggregate.PositiveWeight += observation.PositiveWeight
		aggregate.NegativeWeight += observation.NegativeWeight
		aggregate.ExposureCount += observation.ExposureDelta
		aggregate.ConsultedCount += observation.ConsultedDelta
		aggregate.AppliedCount += observation.AppliedDelta
		aggregate.RejectedCount += observation.RejectedDelta
		aggregate.VerifiedSupportCount += observation.VerifiedSupportDelta
		aggregate.CausalFailureCount += observation.CausalFailureDelta
		if observation.ObservedAt.After(aggregate.LastObservedAt) {
			aggregate.LastObservedAt = observation.ObservedAt
		}
		aggregate.Revision++
		if observation.TaskID != "" && (observation.AppliedDelta > 0 || observation.PositiveWeight > 0 || observation.NegativeWeight > 0) {
			state.tasks[observation.TaskID] = struct{}{}
			state.projects[observation.ProjectID] = struct{}{}
		}
		aggregate.IndependentTaskCount = len(state.tasks)
		aggregate.IndependentProjectCount = len(state.projects)
		aggregate.UtilityLowerBound = BetaQuantile(observation.PriorAlpha+aggregate.PositiveWeight, observation.PriorBeta+aggregate.NegativeWeight, observation.UtilityPercentile)
	}
	out := make([]ExperienceAggregate, 0, len(states))
	for _, state := range states {
		out = append(out, state.aggregate)
	}
	slices.SortFunc(out, func(left, right ExperienceAggregate) int {
		if order := cmp.Compare(left.PolicyVersion, right.PolicyVersion); order != 0 {
			return order
		}
		return cmp.Compare(left.ContextItemID, right.ContextItemID)
	})
	return out, nil
}
