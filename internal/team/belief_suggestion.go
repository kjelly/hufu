package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/belief"
)

const EventBeliefPolicySuggested = "belief_policy_suggested"

type BeliefPolicyState struct {
	ModelDigest string `json:"model_digest"`
	Digest      string `json:"digest"`
	Version     string `json:"version"`
	ArtifactID  string `json:"artifact_id,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}

// BeliefPolicySuggestion is a historical advisory decision. Its generation is
// local to the epoch and is never an active decision engine's routing lease.
type BeliefPolicySuggestion struct {
	SchemaVersion        int    `json:"schema_version"`
	Mode                 string `json:"mode"`
	EpochID              string `json:"epoch_id"`
	SnapshotDigest       string `json:"snapshot_digest"`
	StateDigest          string `json:"state_digest,omitempty"`
	PolicyDigest         string `json:"policy_digest"`
	PolicyVersion        string `json:"policy_version"`
	DecisionGeneration   uint64 `json:"decision_generation"`
	SourceCursorID       string `json:"source_cursor_id"`
	SourceCursorHash     string `json:"source_cursor_hash"`
	SourceActor          string `json:"source_actor"`
	SelectedCandidateID  string `json:"selected_candidate_id,omitempty"`
	SelectedHypothesisID string `json:"selected_hypothesis_id,omitempty"`
	Unavailable          string `json:"unavailable,omitempty"`
	BeliefUnavailable    string `json:"belief_unavailable,omitempty"`
	PolicyUnavailable    string `json:"policy_unavailable,omitempty"`
	Synthetic            *bool  `json:"synthetic"`
}

type BeliefSuggestionView struct {
	Record      BeliefPolicySuggestion `json:"record"`
	Validation  string                 `json:"validation"`
	Unavailable string                 `json:"unavailable,omitempty"`
}

type beliefPolicyContext struct {
	epoch   BeliefEpochState
	binding beliefEvent
	cursor  RunEvent
	actor   string
}

func policyContext(ctx context.Context, events []RunEvent, artifacts ArtifactStore, runID, epochID string) (beliefPolicyContext, error) {
	r, err := reduceBeliefRun(ctx, events, artifacts, runID)
	if err != nil {
		return beliefPolicyContext{}, err
	}
	return r.policyContext(epochID)
}

func beliefPolicySnapshotDigest(epoch BeliefEpochState, actor string) string {
	stateDigest := ""
	if epoch.State != nil {
		stateDigest = epoch.State.Digest
	}
	return beliefIdentity("policy_snapshot_v1", struct {
		Epoch                           belief.Epoch
		Observations                    []belief.Observation
		Actor, StateDigest, Unavailable string
	}{epoch.Epoch, epoch.Observations, actor, stateDigest, epoch.Unavailable})
}

func buildBeliefSuggestion(ctx context.Context, input beliefPolicyContext, artifacts ArtifactStore, generation uint64) (BeliefPolicySuggestion, error) {
	result := BeliefPolicySuggestion{SchemaVersion: 1, Mode: "advisory", EpochID: input.epoch.Epoch.ID, SnapshotDigest: beliefPolicySnapshotDigest(input.epoch, input.actor), PolicyDigest: input.binding.PolicyDigest, PolicyVersion: input.binding.PolicyVersion, DecisionGeneration: generation, SourceCursorID: input.cursor.ID, SourceCursorHash: input.cursor.Hash, SourceActor: input.actor, BeliefUnavailable: input.epoch.Unavailable}
	if input.epoch.State != nil {
		result.StateDigest = input.epoch.State.Digest
		result.Synthetic = new(input.epoch.State.Synthetic)
	}
	var policy *BoundBeliefPolicy
	reason := "policy_unavailable"
	if input.binding.PolicyRef != nil {
		policy, reason = loadBoundBeliefPolicy(ctx, artifacts, *input.binding.PolicyRef, input.binding.PolicyDigest)
	}
	if policy != nil && (policy.Policy.Version != input.binding.PolicyVersion || policy.ModelDigest != input.epoch.Epoch.ModelDigest) {
		return BeliefPolicySuggestion{}, fmt.Errorf("belief policy version mismatch")
	}
	result.PolicyUnavailable = reason
	advice, err := boundBeliefAdvice(policy, input.epoch.State, input.actor, reason)
	if err != nil {
		return BeliefPolicySuggestion{}, err
	}
	result.SelectedCandidateID, result.SelectedHypothesisID, result.Unavailable = advice.CandidateID, advice.HypothesisID, advice.Unavailable
	return result, nil
}

// ProjectBeliefSuggestions verifies every generation against its historical
// input cursor. Missing artifacts retain recorded decisions as unavailable for
// validation; no historical result is replaced by a newly computed choice.
func ProjectBeliefSuggestions(ctx context.Context, events []RunEvent, artifacts ArtifactStore, runID string) ([]BeliefSuggestionView, error) {
	views := []BeliefSuggestionView{}
	last := map[string]BeliefPolicySuggestion{}
	reducer := newBeliefRunReducer(runID)
	for _, event := range events {
		if err := reducer.apply(ctx, event, artifacts); err != nil {
			return nil, err
		}
		if event.RunID != runID || event.Type != EventBeliefPolicySuggested {
			continue
		}
		if len(views) >= maxBeliefAdviceRecords {
			return nil, fmt.Errorf("belief advice history exceeds replay limit")
		}
		var record BeliefPolicySuggestion
		if err := json.Unmarshal(event.Payload, &record, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("invalid belief policy suggestion")
		}
		input, err := reducer.policyContext(record.EpochID)
		if err != nil {
			return nil, err
		}
		if err := validateBeliefSuggestionBinding(event, record, input, last[record.EpochID]); err != nil {
			return nil, err
		}
		view, err := validateBeliefSuggestionResult(ctx, record, input, artifacts)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
		last[record.EpochID] = record
	}
	return views, nil
}

func validateBeliefSuggestionBinding(event RunEvent, record BeliefPolicySuggestion, input beliefPolicyContext, previous BeliefPolicySuggestion) error {
	if event.Actor != beliefActor || !beliefEventMatchesScope(event, input.epoch.Epoch.Scope) || record.SchemaVersion != 1 || record.Mode != "advisory" || record.DecisionGeneration == 0 || record.DecisionGeneration > maxBeliefAdviceGenerations || record.DecisionGeneration != previous.DecisionGeneration+1 {
		return fmt.Errorf("invalid belief advisory generation")
	}
	if record.PolicyDigest != input.binding.PolicyDigest || record.PolicyVersion != input.binding.PolicyVersion || record.SourceCursorID != input.cursor.ID || record.SourceCursorHash != input.cursor.Hash || record.SourceActor != input.actor || !decisionDigestPattern.MatchString(record.SnapshotDigest) {
		return fmt.Errorf("belief suggestion input binding mismatch")
	}
	if record.SnapshotDigest == previous.SnapshotDigest {
		return fmt.Errorf("duplicate belief advisory generation")
	}
	if record.BeliefUnavailable != "" && !slices.Contains([]string{"model_unavailable", "model_binding_invalid", "model_digest_mismatch", "model_invalid", "unsupported_version", "invalid_projection"}, record.BeliefUnavailable) {
		return fmt.Errorf("invalid belief availability fact")
	}
	if record.PolicyUnavailable != "" && !slices.Contains([]string{"policy_unavailable", "policy_invalid", "policy_digest_mismatch"}, record.PolicyUnavailable) {
		return fmt.Errorf("invalid policy availability fact")
	}
	if err := validateBeliefSuggestionShape(record); err != nil {
		return err
	}
	return nil
}

func validateBeliefSuggestionShape(record BeliefPolicySuggestion) error {
	if record.StateDigest != "" && !decisionDigestPattern.MatchString(record.StateDigest) {
		return fmt.Errorf("invalid belief state digest")
	}
	if record.Unavailable == "" {
		if !actionCatalogIDPattern.MatchString(record.SelectedCandidateID) || record.SelectedHypothesisID == "" || record.StateDigest == "" || record.Synthetic == nil {
			return fmt.Errorf("incomplete belief advisory selection")
		}
	} else {
		if record.SelectedCandidateID != "" || record.SelectedHypothesisID != "" || !slices.Contains([]string{"distribution_unavailable", "candidate_not_authorized", "belief_unavailable", "policy_unavailable", "policy_invalid", "policy_digest_mismatch"}, record.Unavailable) {
			return fmt.Errorf("invalid unavailable belief selection")
		}
	}
	if record.BeliefUnavailable != "" && (record.StateDigest != "" || record.Synthetic != nil) {
		return fmt.Errorf("unavailable belief invented a snapshot")
	}
	if record.BeliefUnavailable == "" && (record.StateDigest == "" || record.Synthetic == nil) {
		return fmt.Errorf("available belief lacks a state identity")
	}
	if record.PolicyUnavailable != "" && record.Unavailable != record.PolicyUnavailable {
		return fmt.Errorf("inconsistent policy availability fact")
	}
	return nil
}

func validateBeliefSuggestionResult(ctx context.Context, record BeliefPolicySuggestion, input beliefPolicyContext, artifacts ArtifactStore) (BeliefSuggestionView, error) {
	view := BeliefSuggestionView{Record: record, Validation: "verified"}
	var policy *BoundBeliefPolicy
	policyReason := "policy_unavailable"
	if input.binding.PolicyRef != nil {
		policy, policyReason = loadBoundBeliefPolicy(ctx, artifacts, *input.binding.PolicyRef, record.PolicyDigest)
	}
	if policy != nil {
		if policy.Policy.Version != record.PolicyVersion || policy.ModelDigest != input.epoch.Epoch.ModelDigest {
			return BeliefSuggestionView{}, fmt.Errorf("belief policy identity mismatch")
		}
		// Even if the model is currently missing, a known policy still rules
		// out a forged candidate, hypothesis mapping or proposer grant.
		if record.SelectedCandidateID != "" {
			choice := slices.IndexFunc(policy.Policy.Choices, func(c belief.PolicyChoice) bool {
				return c.HypothesisID == record.SelectedHypothesisID && c.CandidateID == record.SelectedCandidateID
			})
			candidate := slices.IndexFunc(policy.Candidates, func(c BeliefPolicyCandidate) bool { return c.ID == record.SelectedCandidateID })
			if choice < 0 || candidate < 0 || !slices.Contains(policy.Candidates[candidate].Proposers, strings.ToLower(strings.TrimSpace(record.SourceActor))) {
				return BeliefSuggestionView{}, fmt.Errorf("belief suggestion lacks frozen eligibility")
			}
		}
	}
	if record.BeliefUnavailable != "" {
		// Data availability is an owner-recorded historical fact, like a
		// verifier's recorded exit. Restoring an artifact cannot rewrite it.
		input.epoch.State = nil
		input.epoch.Unavailable = record.BeliefUnavailable
	}
	if input.epoch.State == nil && record.BeliefUnavailable == "" {
		view.Validation = "unavailable"
		view.Unavailable = input.epoch.Unavailable
		return view, nil
	}
	if beliefPolicySnapshotDigest(input.epoch, input.actor) != record.SnapshotDigest {
		return BeliefSuggestionView{}, fmt.Errorf("belief snapshot digest mismatch")
	}
	if input.epoch.State != nil {
		if record.StateDigest != input.epoch.State.Digest || record.Synthetic == nil || *record.Synthetic != input.epoch.State.Synthetic {
			return BeliefSuggestionView{}, fmt.Errorf("belief state identity mismatch")
		}
	} else if record.StateDigest != "" || record.Synthetic != nil {
		return BeliefSuggestionView{}, fmt.Errorf("unavailable belief invented a state")
	}
	var advice belief.Advice
	if record.PolicyUnavailable != "" {
		advice.Unavailable = record.PolicyUnavailable
	} else {
		if policyReason != "" {
			view.Validation = "unavailable"
			view.Unavailable = policyReason
			return view, nil
		}
		var err error
		advice, err = boundBeliefAdvice(policy, input.epoch.State, input.actor, "")
		if err != nil {
			return BeliefSuggestionView{}, err
		}
	}
	if advice.CandidateID != record.SelectedCandidateID || advice.HypothesisID != record.SelectedHypothesisID || advice.Unavailable != record.Unavailable {
		return BeliefSuggestionView{}, fmt.Errorf("belief suggestion differs from locked policy")
	}
	return view, nil
}
