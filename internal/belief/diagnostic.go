package belief

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math/big"
	"slices"
)

const (
	DiagnosticGiniAlgorithm           = "expected_gini_gain_per_cost_v1"
	DiagnosticFixedOrder              = "fixed_order_v1"
	MaxDiagnosticCost                 = 1_000_000
	InvalidDiagnostic       ErrorCode = "invalid_diagnostic"
)

// DiagnosticPolicy is a pure ranking contract, never an execution grant.
// Candidate order is meaningful only for the fixed-order baseline.
type DiagnosticPolicy struct {
	SchemaVersion int                   `json:"schema_version"`
	Version       string                `json:"version"`
	Algorithm     string                `json:"algorithm"`
	Threshold     uint64                `json:"threshold_units"`
	Limits        DiagnosticLimits      `json:"limits"`
	Candidates    []DiagnosticCandidate `json:"candidates"`
}

// DiagnosticLimits are pinned declarative caps. Ranking does not reserve or
// spend resources; a future executor must enforce these against the run ledger.
type DiagnosticLimits struct {
	MaxRounds             int    `json:"max_rounds"`
	MaxAttempts           int    `json:"max_attempts"`
	MaxCostUnits          uint64 `json:"max_cost_units"`
	ActionTimeoutSeconds  int    `json:"action_timeout_seconds"`
	EpisodeTimeoutSeconds int    `json:"episode_timeout_seconds"`
	MaxEvents             int    `json:"max_events"`
	MaxObservationBytes   int    `json:"max_observation_bytes"`
	MaxFenceRetries       int    `json:"max_fence_retries"`
}

func (l DiagnosticLimits) Validate() error {
	if l.MaxRounds < 1 || l.MaxRounds > MaxExperiments || l.MaxAttempts < 1 || l.MaxAttempts > MaxObservations || l.MaxCostUnits < 1 || l.MaxCostUnits > MaxDiagnosticCost || l.ActionTimeoutSeconds < 1 || l.ActionTimeoutSeconds > 3600 || l.EpisodeTimeoutSeconds < 1 || l.EpisodeTimeoutSeconds > 86400 || l.MaxEvents < 8 || l.MaxEvents > 4096 || l.MaxObservationBytes < 1 || l.MaxObservationBytes > MaxDiagnosticEvidenceBytes || l.MaxFenceRetries < 1 || l.MaxFenceRetries > 3 {
		return invalid(InvalidDiagnostic, "limits")
	}
	return nil
}

type DiagnosticCandidate struct {
	ID           string `json:"id"`
	ExperimentID string `json:"experiment_id"`
	CostUnits    uint64 `json:"cost_units"`
}

type DiagnosticScore struct {
	CandidateID string `json:"candidate_id"`
	Numerator   string `json:"numerator"`
	Denominator string `json:"denominator"`
	CostUnits   uint64 `json:"cost_units"`
	Excluded    string `json:"excluded,omitempty"`
}

type DiagnosticChoice struct {
	CandidateID string            `json:"candidate_id,omitempty"`
	StopReason  string            `json:"stop_reason,omitempty"`
	Scores      []DiagnosticScore `json:"scores"`
}

func DecodeDiagnosticPolicy(data []byte) (DiagnosticPolicy, error) {
	if len(data) > MaxPolicyBytes {
		return DiagnosticPolicy{}, invalid(ResourceLimit, "diagnostic_policy_bytes")
	}
	var policy DiagnosticPolicy
	if err := json.Unmarshal(data, &policy, json.RejectUnknownMembers(true)); err != nil {
		return DiagnosticPolicy{}, invalid(InvalidDiagnostic, "policy_json")
	}
	root, err := wireObject(data, "schema_version", "version", "algorithm", "threshold_units", "limits", "candidates")
	if err != nil {
		return DiagnosticPolicy{}, err
	}
	if _, err := wireObject(root["limits"], "max_rounds", "max_attempts", "max_cost_units", "action_timeout_seconds", "episode_timeout_seconds", "max_events", "max_observation_bytes", "max_fence_retries"); err != nil {
		return DiagnosticPolicy{}, err
	}
	var candidates []jsontext.Value
	if err := json.Unmarshal(root["candidates"], &candidates); err != nil {
		return DiagnosticPolicy{}, invalid(InvalidDiagnostic, "candidates")
	}
	for _, raw := range candidates {
		if _, err := wireObject(raw, "id", "experiment_id", "cost_units"); err != nil {
			return DiagnosticPolicy{}, err
		}
	}
	if err := validateDiagnosticPolicy(policy); err != nil {
		return DiagnosticPolicy{}, err
	}
	return policy, nil
}

func validateDiagnosticPolicy(policy DiagnosticPolicy) error {
	if policy.SchemaVersion != SchemaVersion {
		return invalid(UnsupportedVersion, "diagnostic_policy")
	}
	if !validID(policy.Version) || !slices.Contains([]string{DiagnosticGiniAlgorithm, DiagnosticFixedOrder}, policy.Algorithm) || policy.Threshold == 0 || policy.Threshold > Scale || len(policy.Candidates) == 0 || len(policy.Candidates) > MaxExperiments {
		return invalid(InvalidDiagnostic, "policy")
	}
	if err := policy.Limits.Validate(); err != nil {
		return err
	}
	ids, experiments := map[string]bool{}, map[string]bool{}
	for _, candidate := range policy.Candidates {
		if !validID(candidate.ID) || !validID(candidate.ExperimentID) || ids[candidate.ID] || experiments[candidate.ExperimentID] || candidate.CostUnits == 0 || candidate.CostUnits > MaxDiagnosticCost {
			return invalid(InvalidDiagnostic, "candidates")
		}
		ids[candidate.ID], experiments[candidate.ExperimentID] = true, true
	}
	return nil
}

func ValidateDiagnosticPolicy(model Model, policy DiagnosticPolicy) error {
	if err := ValidateModel(model); err != nil {
		return err
	}
	if err := validateDiagnosticPolicy(policy); err != nil {
		return err
	}
	for _, candidate := range policy.Candidates {
		if !slices.ContainsFunc(model.Experiments, func(e Experiment) bool { return e.ID == candidate.ExperimentID }) {
			return invalid(InvalidDiagnostic, "experiment_binding")
		}
	}
	return nil
}

func DiagnosticPolicyDigest(policy DiagnosticPolicy) (string, error) {
	if err := validateDiagnosticPolicy(policy); err != nil {
		return "", err
	}
	return digest("hufu-diagnostic-policy-v1", policy)
}

// RankDiagnostic recomputes exact probabilities from the original prior.
// It does not consult clocks, authorization, budgets, providers or journals.
// Callers must apply those gates separately before any future execution.
func RankDiagnostic(model Model, epoch Epoch, observations []Observation, policy DiagnosticPolicy) (DiagnosticChoice, error) {
	if err := ValidateDiagnosticPolicy(model, policy); err != nil {
		return DiagnosticChoice{}, err
	}
	state, weights, err := projectExact(model, epoch, observations)
	if err != nil {
		return DiagnosticChoice{}, err
	}
	choice := DiagnosticChoice{Scores: []DiagnosticScore{}}
	if state.Distribution == nil || state.Status != Prior && state.Status != Estimated {
		choice.StopReason = "distribution_unavailable"
		return choice, nil
	}
	if len(state.ObservationIDs) > 0 && diagnosticThresholdReached(weights, policy.Threshold) {
		choice.StopReason = "threshold_reached"
		return choice, nil
	}
	if len(state.ObservationIDs) >= policy.Limits.MaxRounds {
		choice.StopReason = "round_limit"
		return choice, nil
	}
	used := map[string]bool{}
	groups := map[string]bool{}
	experiments := map[string]Experiment{}
	for _, experiment := range model.Experiments {
		experiments[experiment.ID] = experiment
	}
	for _, observation := range observations {
		used[observation.ExperimentID] = true
		groups[experiments[observation.ExperimentID].CorrelationGroup] = true
	}
	candidates := slices.Clone(policy.Candidates)
	if policy.Algorithm == DiagnosticGiniAlgorithm {
		slices.SortFunc(candidates, func(a, b DiagnosticCandidate) int { return cmp.Compare(a.ID, b.ID) })
	}
	var best *big.Rat
	hasInformation := false
	for _, candidate := range candidates {
		score := DiagnosticScore{CandidateID: candidate.ID, CostUnits: candidate.CostUnits, Numerator: "0", Denominator: "1"}
		if used[candidate.ExperimentID] {
			score.Excluded = "already_observed"
			choice.Scores = append(choice.Scores, score)
			continue
		}
		experiment := experiments[candidate.ExperimentID]
		if groups[experiment.CorrelationGroup] {
			score.Excluded = "correlation_group_observed"
			choice.Scores = append(choice.Scores, score)
			continue
		}
		gain := diagnosticGain(weights, experiment)
		gain.Quo(gain, new(big.Rat).SetInt(new(big.Int).SetUint64(candidate.CostUnits)))
		hasInformation = hasInformation || gain.Sign() > 0
		score.Numerator, score.Denominator = gain.Num().String(), gain.Denom().String()
		choice.Scores = append(choice.Scores, score)
		if best == nil || policy.Algorithm == DiagnosticGiniAlgorithm && gain.Cmp(best) > 0 {
			choice.CandidateID, best = candidate.ID, gain
		}
	}
	if best == nil {
		choice.StopReason = "no_candidate"
	} else if !hasInformation {
		choice.CandidateID, choice.StopReason = "", "no_information_gain"
	}
	return choice, nil
}

func diagnosticThresholdReached(weights []big.Int, threshold uint64) bool {
	probabilities, _ := normalizeDiagnostic(weights)
	minimum := new(big.Rat).SetFrac(new(big.Int).SetUint64(threshold), new(big.Int).SetUint64(Scale))
	return slices.ContainsFunc(probabilities, func(p *big.Rat) bool { return p.Cmp(minimum) >= 0 })
}

func diagnosticGain(weights []big.Int, experiment Experiment) *big.Rat {
	probabilities, total := normalizeDiagnostic(weights)
	gain := diagnosticGini(probabilities)
	for _, outcome := range experiment.Outcomes {
		next := make([]big.Int, len(weights))
		for i := range weights {
			next[i].Mul(&weights[i], new(big.Int).SetUint64(outcome.LikelihoodUnits[i]))
		}
		posterior, mass := normalizeDiagnostic(next)
		if mass.Sign() == 0 {
			continue
		}
		denominator := new(big.Int).Mul(total, new(big.Int).SetUint64(Scale))
		probability := new(big.Rat).SetFrac(mass, denominator)
		gain.Sub(gain, probability.Mul(probability, diagnosticGini(posterior)))
	}
	return gain
}

func normalizeDiagnostic(weights []big.Int) ([]*big.Rat, *big.Int) {
	total := new(big.Int)
	for i := range weights {
		total.Add(total, &weights[i])
	}
	probabilities := make([]*big.Rat, len(weights))
	if total.Sign() > 0 {
		for i := range weights {
			probabilities[i] = new(big.Rat).SetFrac(&weights[i], total)
		}
	}
	return probabilities, total
}

func diagnosticGini(probabilities []*big.Rat) *big.Rat {
	gini := big.NewRat(1, 1)
	for _, probability := range probabilities {
		gini.Sub(gini, new(big.Rat).Mul(probability, probability))
	}
	return gini
}
