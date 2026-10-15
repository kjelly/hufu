package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
)

const EventBeliefUnavailable = "belief_unavailable"

type BeliefModelState struct {
	Digest       string `json:"digest"`
	ID           string `json:"id,omitempty"`
	Version      string `json:"version,omitempty"`
	Synthetic    *bool  `json:"synthetic"`
	ExperimentID string `json:"experiment_id,omitempty"`
	Unavailable  string `json:"unavailable,omitempty"`
}

// BeliefDiagnostic is a content-free historical bookkeeping diagnostic. It
// does not change execution status or assert that a later repair also failed.
type BeliefDiagnostic struct {
	SchemaVersion int    `json:"schema_version"`
	Code          string `json:"code"`
	SourceEventID string `json:"source_event_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
}

type BeliefRunState struct {
	RunID       string             `json:"run_id"`
	Models      []BeliefModelState `json:"models"`
	Epochs      []BeliefEpochState `json:"epochs"`
	Diagnostics []BeliefDiagnostic `json:"diagnostics"`
}

// ProjectBeliefRun reads only a caller-validated lineage and its locked
// artifacts. Missing artifacts are data availability failures, not zero mass.
func ProjectBeliefRun(ctx context.Context, events []RunEvent, artifacts ArtifactStore, runID string) (BeliefRunState, error) {
	epochs, err := ProjectBeliefEpochs(ctx, events, artifacts, runID)
	if err != nil {
		return BeliefRunState{}, err
	}
	view := BeliefRunState{RunID: runID, Models: []BeliefModelState{}, Epochs: epochs, Diagnostics: []BeliefDiagnostic{}}
	seen := map[string]bool{}
	for _, event := range events {
		if event.RunID != runID {
			continue
		}
		switch event.Type {
		case EventBeliefModelBound:
			var payload beliefEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ModelRef == nil {
				return BeliefRunState{}, fmt.Errorf("invalid belief binding")
			}
			if seen[payload.ModelDigest] {
				continue
			}
			seen[payload.ModelDigest] = true
			model, reason := loadBoundBeliefModel(ctx, artifacts, *payload.ModelRef, payload.ModelDigest, payload.ReducerVersion)
			item := BeliefModelState{Digest: payload.ModelDigest, ExperimentID: payload.ExperimentID, Unavailable: reason}
			if model != nil {
				item.ID, item.Version, item.Synthetic = model.ID, model.Version, new(model.Synthetic)
			}
			view.Models = append(view.Models, item)
		case EventBeliefUnavailable:
			var diagnostic BeliefDiagnostic
			if err := json.Unmarshal(event.Payload, &diagnostic, json.RejectUnknownMembers(true)); err != nil || event.Actor != beliefActor || diagnostic.SchemaVersion != 1 || !slices.Contains([]string{"model_binding_unavailable", "source_binding_unavailable", "bookkeeping_unavailable", "replay_unavailable"}, diagnostic.Code) {
				return BeliefRunState{}, fmt.Errorf("invalid belief diagnostic")
			}
			view.Diagnostics = append(view.Diagnostics, diagnostic)
		}
	}
	return view, nil
}

func cloneBeliefRunState(src BeliefRunState) BeliefRunState {
	dst := src
	dst.Models = slices.Clone(src.Models)
	for i := range dst.Models {
		if src.Models[i].Synthetic != nil {
			dst.Models[i].Synthetic = new(*src.Models[i].Synthetic)
		}
	}
	dst.Diagnostics = slices.Clone(src.Diagnostics)
	dst.Epochs = slices.Clone(src.Epochs)
	for i := range dst.Epochs {
		dst.Epochs[i].Sources = slices.Clone(src.Epochs[i].Sources)
		dst.Epochs[i].Observations = slices.Clone(src.Epochs[i].Observations)
		dst.Epochs[i].Retracted = slices.Clone(src.Epochs[i].Retracted)
		if src.Epochs[i].State != nil {
			state := *src.Epochs[i].State
			state.ObservationIDs = slices.Clone(state.ObservationIDs)
			if state.Distribution != nil {
				state.Distribution = new(slices.Clone(*state.Distribution))
			}
			dst.Epochs[i].State = &state
		}
	}
	return dst
}
