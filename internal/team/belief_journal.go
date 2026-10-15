package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"sync"

	"github.com/kjelly/hufu/internal/belief"
)

// BeliefJournal owns only belief bookkeeping. Its pinned branch view never
// grants tool execution or edits the execution status of a task.
type BeliefJournal struct {
	mu                         sync.Mutex
	journal                    EventJournal
	artifacts                  ArtifactStore
	tree                       *SessionTree
	branchID, runID, sessionID string
}

func NewBeliefJournal(journal EventJournal, artifacts ArtifactStore, tree *SessionTree, branchID, runID, sessionID string) (*BeliefJournal, error) {
	if journal == nil || artifacts == nil || tree == nil || tree.Branches[branchID] == nil || runID == "" {
		return nil, fmt.Errorf("belief journal requires a pinned runtime scope")
	}
	return &BeliefJournal{journal: journal, artifacts: artifacts, tree: cloneSessionTree(tree), branchID: branchID, runID: runID, sessionID: sessionID}, nil
}
func (j *BeliefJournal) events(ctx context.Context) ([]RunEvent, error) {
	if err := j.journal.VerifyHashChain(ctx); err != nil {
		return nil, err
	}
	events, err := j.journal.ReadEvents(ctx)
	if err != nil {
		return nil, err
	}
	return ProjectValidatedEventsForBranch(events, j.tree, j.branchID)
}
func (j *BeliefJournal) append(ctx context.Context, kind, taskID string, attempt int, key string, payload beliefEvent) error {
	payload.SchemaVersion = 1
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = j.journal.Append(ctx, RunEvent{Type: kind, Actor: beliefActor, RunID: j.runID, SessionID: j.sessionID, BranchID: j.branchID, TaskID: taskID, Attempt: attempt, IdempotencyKey: "belief:" + key, Payload: data})
	return err
}
func (j *BeliefJournal) BindModel(ctx context.Context, model belief.Model) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	hash, err := belief.ModelDigest(model)
	if err != nil {
		return err
	}
	data, err := json.Marshal(model)
	if err != nil {
		return err
	}
	stored, err := j.artifacts.Put(ctx, PutArtifactRequest{Content: data, Kind: "belief_model", Role: "model", MediaType: "application/json", Path: "belief/models/" + hash + ".json", RunID: j.runID})
	if err != nil {
		return err
	}
	ref := ArtifactRef{ID: stored.ID, SHA256: stored.SHA256, Kind: "belief_model"}
	return j.append(ctx, EventBeliefModelBound, "", 0, "model:"+hash, beliefEvent{ModelRef: &ref, ModelDigest: hash, ReducerVersion: belief.ReducerVersion})
}
func (j *BeliefJournal) OpenEpoch(ctx context.Context, epoch belief.Epoch) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if epoch.Scope.RunID != j.runID || epoch.Scope.BranchID != j.branchID || epoch.Scope.SessionID != "" && epoch.Scope.SessionID != j.sessionID {
		return fmt.Errorf("belief epoch does not match journal scope")
	}
	events, err := j.events(ctx)
	if err != nil {
		return err
	}
	states, err := ProjectBeliefEpochs(ctx, events, j.artifacts, j.runID)
	if err != nil {
		return err
	}
	for _, state := range states {
		if state.Epoch.ID == epoch.ID {
			if state.Epoch != epoch {
				return fmt.Errorf("conflicting belief epoch identity")
			}
			return nil
		}
	}
	var bound *belief.Model
	for _, event := range events {
		if event.Type != EventBeliefModelBound || event.RunID != j.runID {
			continue
		}
		var payload beliefEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("invalid model binding")
		}
		if payload.ModelDigest == epoch.ModelDigest && payload.ModelRef != nil {
			model, reason := loadBoundBeliefModel(ctx, j.artifacts, *payload.ModelRef, epoch.ModelDigest, payload.ReducerVersion)
			if reason != "" {
				return fmt.Errorf("belief model unavailable: %s", reason)
			}
			bound = model
		}
	}
	if bound == nil {
		return fmt.Errorf("belief epoch has no bound model")
	}
	if _, err := belief.Project(*bound, epoch, nil); err != nil {
		return err
	}
	return j.append(ctx, EventBeliefEpochOpened, epoch.Scope.TaskID, epoch.Scope.Attempt, "epoch:"+epoch.ID, beliefEvent{Epoch: &epoch})
}
func (j *BeliefJournal) AdmitVerification(ctx context.Context, epochID, sourceEventID, experimentID, receiptKey string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, err := j.events(ctx)
	if err != nil {
		return err
	}
	epoch, model, err := j.findEpoch(ctx, events, epochID)
	if err != nil {
		return err
	}
	var sourceEvent *RunEvent
	for i := range events {
		if events[i].ID == sourceEventID {
			sourceEvent = &events[i]
			break
		}
	}
	if sourceEvent == nil {
		return fmt.Errorf("belief source event is unavailable")
	}
	observation, source, err := beliefVerificationObservation(epoch.Epoch, *sourceEvent, model, experimentID, receiptKey)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type != EventBeliefObservationAdmitted || event.RunID != j.runID {
			continue
		}
		var previous beliefEvent
		if err := json.Unmarshal(event.Payload, &previous); err != nil {
			return fmt.Errorf("invalid observation admission")
		}
		if previous.EpochID == epochID && previous.Observation != nil && previous.Observation.ID == observation.ID {
			if *previous.Observation != observation {
				return fmt.Errorf("conflicting observation identity")
			}
			return nil
		}
	}
	payload := beliefEvent{EpochID: epochID, Observation: &observation, Source: &source}
	// Validate the prospective event with the exact same reducer before writing.
	data, err := json.Marshal(beliefEvent{SchemaVersion: 1, EpochID: epochID, Observation: &observation, Source: &source})
	if err != nil {
		return err
	}
	candidate := RunEvent{ID: "belief-admission-preflight", Type: EventBeliefObservationAdmitted, Actor: beliefActor, RunID: j.runID, SessionID: j.sessionID, BranchID: j.branchID, TaskID: epoch.Epoch.Scope.TaskID, Attempt: epoch.Epoch.Scope.Attempt, Payload: data}
	prospective, err := ProjectBeliefEpochs(ctx, append(events, candidate), j.artifacts, j.runID)
	if err != nil {
		return err
	}
	for _, state := range prospective {
		if state.Epoch.ID == epochID && state.Unavailable != "" {
			return fmt.Errorf("belief admission exceeds valid projection boundary")
		}
	}
	return j.append(ctx, EventBeliefObservationAdmitted, epoch.Epoch.Scope.TaskID, epoch.Epoch.Scope.Attempt, "observation:"+epochID+":"+observation.ID, payload)
}
func (j *BeliefJournal) findEpoch(ctx context.Context, events []RunEvent, id string) (BeliefEpochState, belief.Model, error) {
	states, err := ProjectBeliefEpochs(ctx, events, j.artifacts, j.runID)
	if err != nil {
		return BeliefEpochState{}, belief.Model{}, err
	}
	for _, state := range states {
		if state.Epoch.ID != id {
			continue
		}
		if state.Unavailable != "" {
			return BeliefEpochState{}, belief.Model{}, fmt.Errorf("belief epoch unavailable: %s", state.Unavailable)
		}
		for _, event := range events {
			if event.Type != EventBeliefModelBound || event.RunID != j.runID {
				continue
			}
			var payload beliefEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return BeliefEpochState{}, belief.Model{}, fmt.Errorf("invalid belief model binding")
			}
			if payload.ModelDigest == state.Epoch.ModelDigest && payload.ModelRef != nil {
				model, reason := loadBoundBeliefModel(ctx, j.artifacts, *payload.ModelRef, payload.ModelDigest, payload.ReducerVersion)
				if reason != "" {
					return BeliefEpochState{}, belief.Model{}, fmt.Errorf("belief model unavailable")
				}
				return state, *model, nil
			}
		}
	}
	return BeliefEpochState{}, belief.Model{}, fmt.Errorf("belief epoch not found")
}
func (j *BeliefJournal) Retract(ctx context.Context, epochID, observationID, triggerID string) error {
	return j.lifecycle(ctx, EventBeliefObservationRetracted, epochID, observationID, triggerID, "source_retracted")
}
func (j *BeliefJournal) CloseEpoch(ctx context.Context, epochID, triggerID string) error {
	return j.lifecycle(ctx, EventBeliefEpochClosed, epochID, "", triggerID, "scope_closed")
}
func (j *BeliefJournal) lifecycle(ctx context.Context, kind, epochID, observationID, triggerID, reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, err := j.events(ctx)
	if err != nil {
		return err
	}
	epoch, _, err := j.findEpoch(ctx, events, epochID)
	if err != nil {
		return err
	}
	index := map[string]int{}
	for i, event := range events {
		index[event.ID] = i
	}
	if err := validateBeliefTrigger(epoch.Epoch, triggerID, events, index, len(events)); err != nil {
		return err
	}
	if kind == EventBeliefObservationRetracted && observationID == "" {
		return fmt.Errorf("retraction requires observation identity")
	}
	return j.append(ctx, kind, epoch.Epoch.Scope.TaskID, epoch.Epoch.Scope.Attempt, kind+":"+epochID+":"+observationID, beliefEvent{EpochID: epochID, ObservationID: observationID, TriggerID: triggerID, Reason: reason})
}
func (j *BeliefJournal) Replay(ctx context.Context) ([]BeliefEpochState, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, err := j.events(ctx)
	if err != nil {
		return nil, err
	}
	return ProjectBeliefEpochs(ctx, events, j.artifacts, j.runID)
}
