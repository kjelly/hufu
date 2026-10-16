package team

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/belief"
)

// beliefRunReducer consumes one verified lineage in order. Historical policy
// validation snapshots only its target epoch, instead of rebuilding all earlier
// epochs for every suggestion. No cache survives an invocation of the reader.
type beliefRunReducer struct {
	runID, sessionID string
	models           map[string]*belief.Model
	modelErrors      map[string]string
	adapters         map[string]string
	bindings         map[string]beliefEvent
	epochs           map[string]*beliefEpochReducer
	events           []RunEvent
	eventIndex       map[string]int
}

func newBeliefRunReducer(runID string) *beliefRunReducer {
	return &beliefRunReducer{runID: runID, models: map[string]*belief.Model{}, modelErrors: map[string]string{}, adapters: map[string]string{}, bindings: map[string]beliefEvent{}, epochs: map[string]*beliefEpochReducer{}, eventIndex: map[string]int{}}
}

func (r *beliefRunReducer) apply(ctx context.Context, event RunEvent, artifacts ArtifactStore) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.eventIndex[event.ID] = len(r.events)
	r.events = append(r.events, event)
	if event.RunID != r.runID {
		return nil
	}
	if event.SessionID != "" {
		if r.sessionID != "" && r.sessionID != event.SessionID {
			return fmt.Errorf("belief run has ambiguous sessions")
		}
		r.sessionID = event.SessionID
	}
	if !isBeliefEvent(event.Type) {
		return nil
	}
	if event.Actor != beliefActor {
		return fmt.Errorf("belief event has invalid authority")
	}
	var payload beliefEvent
	if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid belief event payload")
	}
	if payload.SchemaVersion != 1 {
		return fmt.Errorf("unsupported belief event version")
	}
	switch event.Type {
	case EventBeliefModelBound:
		return r.bindModel(ctx, payload, artifacts)
	case EventBeliefEpochOpened:
		return r.openEpoch(event, payload)
	default:
		epoch := r.epochs[payload.EpochID]
		if epoch == nil {
			return fmt.Errorf("belief event has no epoch")
		}
		if !beliefEventMatchesScope(event, epoch.value.Epoch.Scope) {
			return fmt.Errorf("belief event scope mismatch")
		}
		if event.Type == EventBeliefObservationRetracted {
			epoch.cursor = event
		}
		if event.Type == EventBeliefObservationAdmitted && payload.Observation != nil && payload.Source != nil {
			if _, seen := epoch.observations[payload.Observation.ID]; !seen {
				epoch.cursor = event
				if index, ok := r.eventIndex[payload.Source.EventID]; ok {
					epoch.actor = r.events[index].Actor
				}
			}
		}
		if err := applyBeliefLifecycle(epoch, event, payload, r.events, r.eventIndex); err != nil {
			return err
		}
	}
	return nil
}

func (r *beliefRunReducer) policyContext(epochID string) (beliefPolicyContext, error) {
	epoch := r.epochs[epochID]
	if epoch == nil {
		return beliefPolicyContext{}, fmt.Errorf("belief policy epoch unavailable")
	}
	binding := r.bindings[epoch.value.Epoch.ModelDigest]
	if binding.PolicyDigest == "" || binding.PolicyVersion == "" || !decisionDigestPattern.MatchString(binding.PolicyDigest) {
		return beliefPolicyContext{}, fmt.Errorf("belief epoch has no policy binding")
	}
	return beliefPolicyContext{epoch: epoch.snapshot(), binding: binding, cursor: epoch.cursor, actor: epoch.actor}, nil
}

func reduceBeliefRun(ctx context.Context, events []RunEvent, artifacts ArtifactStore, runID string) (*beliefRunReducer, error) {
	r := newBeliefRunReducer(runID)
	for _, event := range events {
		if err := r.apply(ctx, event, artifacts); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *beliefRunReducer) bindModel(ctx context.Context, payload beliefEvent, artifacts ArtifactStore) error {
	if err := validateBeliefRuntimeBinding(payload, r.bindings); err != nil {
		return err
	}
	model, reason := loadBoundBeliefModel(ctx, artifacts, *payload.ModelRef, payload.ModelDigest, payload.ReducerVersion)
	if previous, ok := r.models[payload.ModelDigest]; ok && previous != nil && model != nil {
		old, _ := belief.ModelDigest(*previous)
		next, _ := belief.ModelDigest(*model)
		if old != next {
			return fmt.Errorf("conflicting belief model binding")
		}
	}
	r.models[payload.ModelDigest] = model
	r.modelErrors[payload.ModelDigest] = reason
	if previous, exists := r.adapters[payload.ModelDigest]; exists && previous != payload.ExperimentID {
		return fmt.Errorf("conflicting belief adapter binding")
	}
	r.adapters[payload.ModelDigest] = payload.ExperimentID
	return nil
}

func (r *beliefRunReducer) openEpoch(event RunEvent, payload beliefEvent) error {
	if payload.Epoch == nil || payload.Epoch.ID == "" || !beliefEventMatchesScope(event, payload.Epoch.Scope) {
		return fmt.Errorf("invalid belief epoch binding")
	}
	epoch := *payload.Epoch
	if _, ok := r.models[epoch.ModelDigest]; !ok {
		return fmt.Errorf("belief epoch has no bound model")
	}
	if previous := r.epochs[epoch.ID]; previous != nil {
		if previous.value.Epoch != epoch {
			return fmt.Errorf("conflicting belief epoch identity")
		}
		previous.cursor = event
		return nil
	}
	r.epochs[epoch.ID] = &beliefEpochReducer{value: BeliefEpochState{Epoch: epoch, Unavailable: r.modelErrors[epoch.ModelDigest], Sources: []BeliefSource{}, Retracted: []string{}}, model: r.models[epoch.ModelDigest], experimentID: r.adapters[epoch.ModelDigest], observations: map[string]belief.Observation{}, sources: map[string]BeliefSource{}, tombstones: map[string]bool{}, closeSourceIndex: -1, cursor: event}
	return nil
}
