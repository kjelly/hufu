package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

const (
	maxBeliefAdviceGenerations = 1024
	maxBeliefAdviceRecords     = 4096
)

// BeliefPolicyInput is the read token required for an advisory append. A
// caller cannot supply a choice or advance a generation by constructing one.
type BeliefPolicyInput struct {
	SnapshotDigest     string `json:"snapshot_digest"`
	DecisionGeneration uint64 `json:"decision_generation"`
}

func (j *BeliefJournal) ReadPolicyInput(ctx context.Context, epochID string) (BeliefPolicyInput, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, _, err := j.policyEvents(ctx)
	if err != nil {
		return BeliefPolicyInput{}, err
	}
	input, err := policyContext(ctx, events, j.artifacts, j.runID, epochID)
	if err != nil {
		return BeliefPolicyInput{}, err
	}
	views, err := ProjectBeliefSuggestions(ctx, events, j.artifacts, j.runID)
	if err != nil {
		return BeliefPolicyInput{}, err
	}
	result := BeliefPolicyInput{SnapshotDigest: beliefPolicySnapshotDigest(input.epoch, input.actor)}
	for _, view := range views {
		if view.Record.EpochID == epochID {
			result.DecisionGeneration = view.Record.DecisionGeneration
		}
	}
	return result, nil
}

// SuggestPolicy computes and persists an advisory fact. The caller supplies
// its observed generation and snapshot, not a candidate or executable action.
// Changed inputs are refused; durable retries return the same recorded fact.
func (j *BeliefJournal) SuggestPolicy(ctx context.Context, epochID, snapshotDigest string, expectedGeneration uint64) (BeliefPolicySuggestion, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	events, head, err := j.policyEvents(ctx)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	input, err := policyContext(ctx, events, j.artifacts, j.runID, epochID)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	if input.epoch.Epoch.Scope.BranchID != j.branchID || input.epoch.Epoch.Scope.SessionID != "" && input.epoch.Epoch.Scope.SessionID != j.sessionID {
		return BeliefPolicySuggestion{}, fmt.Errorf("cannot suggest on an inherited epoch")
	}
	if beliefPolicySnapshotDigest(input.epoch, input.actor) != snapshotDigest {
		return BeliefPolicySuggestion{}, fmt.Errorf("stale belief policy snapshot")
	}
	views, err := ProjectBeliefSuggestions(ctx, events, j.artifacts, j.runID)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	var previous BeliefPolicySuggestion
	for _, view := range views {
		if view.Record.EpochID == epochID {
			previous = view.Record
		}
	}
	if previous.SnapshotDigest == snapshotDigest {
		return previous, nil
	}
	if previous.DecisionGeneration != expectedGeneration {
		return BeliefPolicySuggestion{}, fmt.Errorf("stale belief advisory generation")
	}
	if err := validateBeliefAdviceCapacity(len(views), expectedGeneration); err != nil {
		return BeliefPolicySuggestion{}, err
	}
	result, err := buildBeliefSuggestion(ctx, input, j.artifacts, expectedGeneration+1)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	conditional, ok := j.journal.(ConditionalEventJournal)
	if !ok {
		return BeliefPolicySuggestion{}, fmt.Errorf("belief policy requires a conditional journal adapter")
	}
	key := fmt.Sprintf("belief-policy:%s:%s:%d", j.runID, epochID, result.DecisionGeneration)
	_, err = conditional.AppendAtHead(ctx, RunEvent{Type: EventBeliefPolicySuggested, Actor: beliefActor, RunID: j.runID, SessionID: j.sessionID, BranchID: j.branchID, TaskID: input.epoch.Epoch.Scope.TaskID, Attempt: input.epoch.Epoch.Scope.Attempt, IdempotencyKey: key, Payload: payload}, head)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	return result, nil
}

func validateBeliefAdviceCapacity(records int, generation uint64) error {
	if records >= maxBeliefAdviceRecords || generation >= maxBeliefAdviceGenerations {
		return fmt.Errorf("belief advisory history capacity exhausted")
	}
	return nil
}

func (j *BeliefJournal) policyEvents(ctx context.Context) ([]RunEvent, string, error) {
	if err := j.journal.VerifyHashChain(ctx); err != nil {
		return nil, "", err
	}
	global, err := j.journal.ReadEvents(ctx)
	if err != nil {
		return nil, "", err
	}
	head := ""
	if len(global) > 0 {
		head = global[len(global)-1].ID
	}
	visible, err := ProjectValidatedEventsForBranch(global, j.tree, j.branchID)
	return visible, head, err
}
