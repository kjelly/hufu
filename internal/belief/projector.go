package belief

import (
	"cmp"
	"math/big"
	"slices"
)

type Scope struct {
	ProjectID string `json:"project_id,omitempty"`
	TeamID    string `json:"team_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id"`
	BranchID  string `json:"branch_id"`
	TaskID    string `json:"task_id"`
	Attempt   int    `json:"attempt"`
}
type Epoch struct {
	ID                  string `json:"id"`
	ModelDigest         string `json:"model_digest"`
	Scope               Scope  `json:"scope"`
	SubjectRevision     string `json:"subject_revision"`
	EnvironmentRevision string `json:"environment_revision"`
}
type Observation struct {
	ID           string `json:"id"`
	ExperimentID string `json:"experiment_id"`
	OutcomeID    string `json:"outcome_id"`
	SourceID     string `json:"source_id"`
}
type Mass struct {
	HypothesisID string `json:"hypothesis_id"`
	Units        uint64 `json:"units"`
}
type Status string

const (
	Prior                Status = "prior"
	Estimated            Status = "estimated"
	ModelInconsistent    Status = "model_inconsistent"
	DependenceUnresolved Status = "dependence_unresolved"
	ResolutionLimited    Status = "resolution_limited"
)

type State struct {
	SchemaVersion  int      `json:"schema_version"`
	ReducerVersion string   `json:"reducer_version"`
	ModelDigest    string   `json:"model_digest"`
	Synthetic      bool     `json:"synthetic"`
	Epoch          Epoch    `json:"epoch"`
	Status         Status   `json:"status"`
	ObservationIDs []string `json:"observation_ids"`
	Distribution   *[]Mass  `json:"distribution"`
	InputDigest    string   `json:"input_digest"`
	Digest         string   `json:"digest,omitempty"`
}

// Project recomputes from the original prior and unique valid observations.
// Multiple occurrences in a correlation group remain unresolved, never independent by default.
func Project(model Model, epoch Epoch, observations []Observation) (State, error) {
	state, _, err := projectExact(model, epoch, observations)
	return state, err
}

// projectExact retains the unrounded weights for pure experiment selection.
// Both public projections and selectors use the same validation and deduplication.
func projectExact(model Model, epoch Epoch, observations []Observation) (State, []big.Int, error) {
	modelHash, err := ModelDigest(model)
	if err != nil {
		return State{}, nil, err
	}
	if !validID(epoch.ID) || epoch.ModelDigest != modelHash || !validID(epoch.Scope.RunID) || !validID(epoch.Scope.BranchID) || !validID(epoch.Scope.TaskID) || epoch.Scope.Attempt < 1 || !validID(epoch.SubjectRevision) || !validID(epoch.EnvironmentRevision) {
		return State{}, nil, invalid(InvalidEpoch, "binding")
	}
	if len(observations) > 4096 {
		return State{}, nil, invalid(ResourceLimit, "observation_inputs")
	}
	unique, err := uniqueObservations(observations)
	if err != nil {
		return State{}, nil, err
	}
	if len(unique) > MaxObservations {
		return State{}, nil, invalid(ResourceLimit, "observations")
	}
	state := State{SchemaVersion: SchemaVersion, ReducerVersion: ReducerVersion, ModelDigest: modelHash, Synthetic: model.Synthetic, Epoch: epoch, Status: Prior, ObservationIDs: make([]string, 0, len(unique))}
	weights := make([]big.Int, len(model.Hypotheses))
	for i, h := range model.Hypotheses {
		weights[i].SetUint64(h.PriorUnits)
	}
	groups := map[string]bool{}
	sources := map[string]bool{}
	unresolved := false
	for _, observation := range unique {
		experiment, outcome, err := resolveObservation(model, observation)
		if err != nil {
			return State{}, nil, err
		}
		state.ObservationIDs = append(state.ObservationIDs, observation.ID)
		// One source cannot masquerade as independent data under another experiment.
		if groups[experiment.CorrelationGroup] || sources[observation.SourceID] {
			unresolved = true
		}
		groups[experiment.CorrelationGroup], sources[observation.SourceID] = true, true
		if unresolved {
			// Keep validating the remaining observations and their identities,
			// but no distribution can be computed for correlated evidence.
			continue
		}
		for i, mass := range outcome.LikelihoodUnits {
			weights[i].Mul(&weights[i], new(big.Int).SetUint64(mass))
			if weights[i].BitLen() > MaxIntermediateBits {
				return State{}, nil, invalid(ResourceLimit, "intermediate_bits")
			}
		}
	}
	switch {
	case unresolved:
		state.Status = DependenceUnresolved
	default:
		distribution, status := quantize(model.Hypotheses, weights)
		state.Status = status
		if len(unique) == 0 && status == Estimated {
			state.Status = Prior
		}
		if distribution != nil {
			state.Distribution = new(distribution)
		}
	}
	state.InputDigest, err = digest("hufu-belief-input-v1", unique)
	if err != nil {
		return State{}, nil, err
	}
	hash, err := digest("hufu-belief-state-v1", state)
	if err != nil {
		return State{}, nil, err
	}
	state.Digest = hash
	return state, weights, nil
}
func uniqueObservations(observations []Observation) ([]Observation, error) {
	byID := map[string]Observation{}
	for _, observation := range observations {
		if !validID(observation.ID) || !validID(observation.SourceID) {
			return nil, invalid(InvalidObservation, "identity")
		}
		if previous, exists := byID[observation.ID]; exists && previous != observation {
			return nil, invalid(IdentityConflict, "observation")
		}
		byID[observation.ID] = observation
	}
	unique := make([]Observation, 0, len(byID))
	for _, value := range byID {
		unique = append(unique, value)
	}
	slices.SortFunc(unique, func(a, b Observation) int { return cmp.Compare(a.ID, b.ID) })
	return unique, nil
}
func resolveObservation(model Model, observation Observation) (Experiment, Outcome, error) {
	for _, experiment := range model.Experiments {
		if experiment.ID != observation.ExperimentID {
			continue
		}
		for _, outcome := range experiment.Outcomes {
			if outcome.ID == observation.OutcomeID {
				return experiment, outcome, nil
			}
		}
	}
	return Experiment{}, Outcome{}, invalid(InvalidObservation, "experiment_outcome")
}
func quantize(hypotheses []Hypothesis, weights []big.Int) ([]Mass, Status) {
	total := new(big.Int)
	for i := range weights {
		total.Add(total, &weights[i])
	}
	if total.Sign() == 0 {
		return nil, ModelInconsistent
	}
	type remainder struct {
		index int
		value *big.Int
	}
	remainders := make([]remainder, len(weights))
	out := make([]Mass, len(weights))
	var assigned uint64
	for i := range weights {
		scaled := new(big.Int).Mul(&weights[i], new(big.Int).SetUint64(Scale))
		quotient, remaining := new(big.Int), new(big.Int)
		quotient.QuoRem(scaled, total, remaining)
		units := quotient.Uint64()
		assigned += units
		out[i] = Mass{HypothesisID: hypotheses[i].ID, Units: units}
		remainders[i] = remainder{index: i, value: remaining}
	}
	slices.SortFunc(remainders, func(a, b remainder) int {
		return cmp.Or(-a.value.Cmp(b.value), cmp.Compare(hypotheses[a.index].ID, hypotheses[b.index].ID))
	})
	for i := uint64(0); i < Scale-assigned; i++ {
		out[remainders[i].index].Units++
	}
	status := Estimated
	for i := range weights {
		if weights[i].Sign() > 0 && out[i].Units == 0 {
			status = ResolutionLimited
		}
	}
	slices.SortFunc(out, func(a, b Mass) int { return cmp.Compare(a.HypothesisID, b.HypothesisID) })
	return out, status
}
