package team

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/kjelly/hufu/internal/belief"
)

const (
	EventBeliefModelBound           = "belief_model_bound"
	EventBeliefEpochOpened          = "belief_epoch_opened"
	EventBeliefObservationAdmitted  = "belief_observation_admitted"
	EventBeliefObservationRetracted = "belief_observation_retracted"
	EventBeliefEpochClosed          = "belief_epoch_closed"
)
const beliefActor = "belief-runtime"

// BeliefSource binds a typed observation to a verified canonical receipt occurrence.
type BeliefSource struct {
	EventID    string `json:"event_id"`
	EventHash  string `json:"event_hash"`
	ReceiptKey string `json:"receipt_key"`
}
type beliefEvent struct {
	SchemaVersion  int                 `json:"schema_version"`
	ModelRef       *ArtifactRef        `json:"model_ref,omitempty"`
	ModelDigest    string              `json:"model_digest,omitempty"`
	ReducerVersion string              `json:"reducer_version,omitempty"`
	ExperimentID   string              `json:"experiment_id,omitempty"`
	PolicyRef      *ArtifactRef        `json:"policy_ref,omitempty"`
	PolicyDigest   string              `json:"policy_digest,omitempty"`
	PolicyVersion  string              `json:"policy_version,omitempty"`
	Epoch          *belief.Epoch       `json:"epoch,omitempty"`
	EpochID        string              `json:"epoch_id,omitempty"`
	Observation    *belief.Observation `json:"observation,omitempty"`
	ObservationID  string              `json:"observation_id,omitempty"`
	Source         *BeliefSource       `json:"source,omitempty"`
	TriggerID      string              `json:"trigger_id,omitempty"`
	Reason         string              `json:"reason,omitempty"`
}

// BeliefEpochState is rebuilt solely from a caller-validated event lineage and immutable artifacts.
// Unavailable means the inputs cannot support a calculation, not that execution failed.
type BeliefEpochState struct {
	Epoch        belief.Epoch         `json:"epoch"`
	State        *belief.State        `json:"state"`
	Closed       bool                 `json:"closed"`
	Unavailable  string               `json:"unavailable,omitempty"`
	Sources      []BeliefSource       `json:"sources"`
	Observations []belief.Observation `json:"observations"`
	Retracted    []string             `json:"retracted"`
}

type beliefEpochReducer struct {
	value            BeliefEpochState
	model            *belief.Model
	experimentID     string
	observations     map[string]belief.Observation
	sources          map[string]BeliefSource
	tombstones       map[string]bool
	closeSourceIndex int
	cursor           RunEvent
	actor            string
}

// ProjectBeliefEpochs never reads the current team definition or executes source actions.
// events must already be globally validated and restricted to the selected branch lineage.
func ProjectBeliefEpochs(ctx context.Context, events []RunEvent, artifacts ArtifactStore, runID string) ([]BeliefEpochState, error) {
	r, err := reduceBeliefRun(ctx, events, artifacts, runID)
	if err != nil {
		return nil, err
	}
	out := make([]BeliefEpochState, 0, len(r.epochs))
	for _, epoch := range r.epochs {
		out = append(out, epoch.snapshot())
	}
	slices.SortFunc(out, func(a, b BeliefEpochState) int { return cmp.Compare(a.Epoch.ID, b.Epoch.ID) })
	return out, nil
}

// snapshot owns all projected slices; later events cannot mutate a historical
// policy input and repeated snapshots cannot duplicate sources or tombstones.
func (epoch *beliefEpochReducer) snapshot() BeliefEpochState {
	value := epoch.value
	value.Sources = []BeliefSource{}
	value.Retracted = []string{}
	observations := make([]belief.Observation, 0, len(epoch.observations))
	for id, observation := range epoch.observations {
		if !epoch.tombstones[id] {
			observations = append(observations, observation)
			value.Sources = append(value.Sources, epoch.sources[id])
		}
	}
	slices.SortFunc(observations, func(a, b belief.Observation) int { return cmp.Compare(a.ID, b.ID) })
	value.Observations = observations
	if epoch.model != nil {
		state, err := belief.Project(*epoch.model, value.Epoch, observations)
		if err != nil {
			value.Unavailable = "invalid_projection"
		} else {
			value.State = &state
		}
	}
	for id := range epoch.tombstones {
		value.Retracted = append(value.Retracted, id)
	}
	slices.Sort(value.Retracted)
	slices.SortFunc(value.Sources, func(a, b BeliefSource) int {
		return cmp.Or(cmp.Compare(a.ReceiptKey, b.ReceiptKey), cmp.Compare(a.EventID, b.EventID), cmp.Compare(a.EventHash, b.EventHash))
	})
	return value
}

func validateBeliefRuntimeBinding(payload beliefEvent, bindings map[string]beliefEvent) error {
	if payload.ModelRef == nil || payload.ModelDigest == "" {
		return fmt.Errorf("incomplete belief model binding")
	}
	if (payload.PolicyRef == nil) != (payload.PolicyDigest == "") || (payload.PolicyVersion == "") != (payload.PolicyDigest == "") || payload.PolicyDigest != "" && !decisionDigestPattern.MatchString(payload.PolicyDigest) {
		return fmt.Errorf("incomplete belief policy binding")
	}
	if previous, exists := bindings[payload.ModelDigest]; exists && !sameRuntimeBeliefBinding(previous, payload) {
		return fmt.Errorf("conflicting belief runtime binding")
	}
	bindings[payload.ModelDigest] = payload
	return nil
}

func beliefEventMatchesScope(event RunEvent, scope belief.Scope) bool {
	return event.RunID == scope.RunID && event.TaskID == scope.TaskID &&
		effectiveBranchID(event.BranchID) == effectiveBranchID(scope.BranchID) &&
		(scope.SessionID == "" || event.SessionID == scope.SessionID) && event.Attempt == scope.Attempt
}

func applyBeliefLifecycle(epoch *beliefEpochReducer, event RunEvent, payload beliefEvent, events []RunEvent, index map[string]int) error {
	switch event.Type {
	case EventBeliefObservationAdmitted:
		if payload.Observation == nil || payload.Source == nil {
			return fmt.Errorf("incomplete belief observation")
		}
		observation, source := *payload.Observation, *payload.Source
		if epoch.experimentID != "" && observation.ExperimentID != epoch.experimentID {
			return fmt.Errorf("belief observation adapter mismatch")
		}
		sourceIndex, ok := index[source.EventID]
		if !ok {
			return fmt.Errorf("belief source event is unavailable")
		}
		if sourceIndex >= index[event.ID] || events[sourceIndex].Hash != source.EventHash {
			return fmt.Errorf("belief source event binding mismatch")
		}
		if epoch.closeSourceIndex >= 0 && sourceIndex > epoch.closeSourceIndex {
			return fmt.Errorf("closed belief epoch cannot admit later source")
		}
		receipt, sourceErr := beliefSourceReceipt(events[sourceIndex], epoch.value.Epoch.Scope, source.ReceiptKey)
		if sourceErr != nil || beliefReceiptKey(receipt) != observation.SourceID || source.ReceiptKey != observation.SourceID || observation.ID != beliefIdentity("observation", []string{epoch.value.Epoch.ID, source.ReceiptKey, observation.ExperimentID}) || observation.OutcomeID != beliefVerificationOutcome(receipt) || epoch.value.Epoch.SubjectRevision != receipt.RunInputSnapshotHash || epoch.value.Epoch.EnvironmentRevision != beliefEnvironmentRevision(receipt) {
			return fmt.Errorf("belief observation source scope mismatch")
		}
		if epoch.model != nil {
			expected, expectedSource, err := beliefVerificationObservation(epoch.value.Epoch, events[sourceIndex], *epoch.model, observation.ExperimentID, source.ReceiptKey)
			if err != nil || expected != observation || expectedSource != source {
				return fmt.Errorf("belief observation does not match canonical verification")
			}
		}
		if previous, exists := epoch.observations[observation.ID]; exists {
			if previous != observation {
				return fmt.Errorf("conflicting belief observation identity")
			}
			return nil
		}
		epoch.observations[observation.ID] = observation
		epoch.sources[observation.ID] = source
	case EventBeliefObservationRetracted:
		if payload.ObservationID == "" || payload.Reason != "source_retracted" {
			return fmt.Errorf("invalid belief retraction")
		}
		if err := validateBeliefTrigger(epoch.value.Epoch, payload.TriggerID, events, index, index[event.ID]); err != nil {
			return err
		}
		epoch.tombstones[payload.ObservationID] = true
	case EventBeliefEpochClosed:
		if payload.Reason != "scope_closed" {
			return fmt.Errorf("invalid belief closure")
		}
		if err := validateBeliefTrigger(epoch.value.Epoch, payload.TriggerID, events, index, index[event.ID]); err != nil {
			return err
		}
		epoch.value.Closed = true
		if epoch.closeSourceIndex < 0 {
			epoch.closeSourceIndex = index[payload.TriggerID]
		}
	}
	return nil
}
func validateBeliefTrigger(epoch belief.Epoch, id string, events []RunEvent, index map[string]int, before int) error {
	i, ok := index[id]
	if !ok || i >= before || events[i].RunID != epoch.Scope.RunID || events[i].TaskID != epoch.Scope.TaskID || isBeliefEvent(events[i].Type) {
		return fmt.Errorf("belief lifecycle trigger does not belong to epoch")
	}
	return nil
}
func loadBoundBeliefModel(ctx context.Context, store ArtifactStore, ref ArtifactRef, expected, version string) (*belief.Model, string) {
	if version != belief.ReducerVersion {
		return nil, "unsupported_version"
	}
	if store == nil {
		return nil, "model_unavailable"
	}
	if ref.ID == "" || ref.SHA256 == "" || ref.Kind != "belief_model" {
		return nil, "model_binding_invalid"
	}
	canonical, err := store.Resolve(ctx, ref)
	if err != nil || canonical.Kind != "belief_model" {
		return nil, "model_unavailable"
	}
	reader, err := store.Open(ctx, canonical.ID)
	if err != nil {
		return nil, "model_unavailable"
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, belief.MaxModelBytes+1))
	if err != nil {
		return nil, "model_unavailable"
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != canonical.SHA256 {
		return nil, "model_digest_mismatch"
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		if typed, ok := errors.AsType[*belief.Error](err); ok && typed.Code == belief.UnsupportedVersion {
			return nil, "unsupported_version"
		}
		return nil, "model_invalid"
	}
	hash, err := belief.ModelDigest(model)
	if err != nil || hash != expected {
		return nil, "model_digest_mismatch"
	}
	return &model, ""
}
func isBeliefEvent(kind string) bool {
	return slices.Contains([]string{EventBeliefModelBound, EventBeliefEpochOpened, EventBeliefObservationAdmitted, EventBeliefObservationRetracted, EventBeliefEpochClosed}, kind)
}
func beliefIdentity(domain string, value any) string {
	data, _ := json.Marshal(value, json.Deterministic(true))
	sum := sha256.Sum256(append([]byte(domain+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}
