package belief

import (
	"cmp"
	"encoding/json/v2"
	"slices"
)

const (
	PolicyAlgorithm           = "max_posterior_v1"
	MaxPolicyBytes            = 64 << 10
	InvalidPolicy   ErrorCode = "invalid_policy"
)

// Policy is a finite, advisory-only mapping. Its candidate IDs carry no
// authority; the runtime must bind them to existing catalog grants separately.
type Policy struct {
	SchemaVersion int            `json:"schema_version"`
	Version       string         `json:"version"`
	Algorithm     string         `json:"algorithm"`
	Mode          string         `json:"mode"`
	Choices       []PolicyChoice `json:"choices"`
}

type PolicyChoice struct {
	HypothesisID string `json:"hypothesis_id"`
	CandidateID  string `json:"candidate_id"`
}

type Advice struct {
	CandidateID  string `json:"candidate_id,omitempty"`
	HypothesisID string `json:"hypothesis_id,omitempty"`
	Unavailable  string `json:"unavailable,omitempty"`
}

func DecodePolicy(data []byte) (Policy, error) {
	if len(data) > MaxPolicyBytes {
		return Policy{}, invalid(ResourceLimit, "policy_bytes")
	}
	var policy Policy
	if err := json.Unmarshal(data, &policy, json.RejectUnknownMembers(true)); err != nil {
		return Policy{}, invalid(InvalidPolicy, "json")
	}
	if err := ValidatePolicy(policy); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

func ValidatePolicy(policy Policy) error {
	if policy.SchemaVersion != 1 || policy.Algorithm != PolicyAlgorithm {
		return invalid(UnsupportedVersion, "policy_version")
	}
	if policy.Mode != "advisory" || !validID(policy.Version) || len(policy.Choices) < 2 || len(policy.Choices) > MaxHypotheses {
		return invalid(InvalidPolicy, "policy")
	}
	seen := map[string]bool{}
	for _, choice := range policy.Choices {
		if !validID(choice.HypothesisID) || !validID(choice.CandidateID) || seen[choice.HypothesisID] {
			return invalid(InvalidPolicy, "choices")
		}
		seen[choice.HypothesisID] = true
	}
	return nil
}

func PolicyDigest(policy Policy) (string, error) {
	if err := ValidatePolicy(policy); err != nil {
		return "", err
	}
	policy.Choices = slices.Clone(policy.Choices)
	slices.SortFunc(policy.Choices, func(a, b PolicyChoice) int { return cmp.Compare(a.HypothesisID, b.HypothesisID) })
	return digest("hufu-belief-policy-v1", policy)
}

// Suggest is pure and consumes the published Q9 distribution, never a model
// call, tool result string, current configuration, clock or previous advice.
func Suggest(policy Policy, state State) (Advice, error) {
	if err := ValidatePolicy(policy); err != nil {
		return Advice{}, err
	}
	if state.Distribution == nil || state.Status != Prior && state.Status != Estimated {
		return Advice{Unavailable: "distribution_unavailable"}, nil
	}
	distribution := *state.Distribution
	if len(distribution) != len(policy.Choices) {
		return Advice{}, invalid(InvalidPolicy, "hypotheses")
	}
	var best Mass
	seen := map[string]bool{}
	var total uint64
	for _, mass := range distribution {
		if mass.Units > Scale || seen[mass.HypothesisID] || !slices.ContainsFunc(policy.Choices, func(choice PolicyChoice) bool { return choice.HypothesisID == mass.HypothesisID }) {
			return Advice{}, invalid(InvalidPolicy, "distribution")
		}
		seen[mass.HypothesisID] = true
		total += mass.Units
		if best.HypothesisID == "" || mass.Units > best.Units || mass.Units == best.Units && mass.HypothesisID < best.HypothesisID {
			best = mass
		}
	}
	if total != Scale {
		return Advice{}, invalid(InvalidPolicy, "distribution_sum")
	}
	index := slices.IndexFunc(policy.Choices, func(choice PolicyChoice) bool { return choice.HypothesisID == best.HypothesisID })
	return Advice{CandidateID: policy.Choices[index].CandidateID, HypothesisID: best.HypothesisID}, nil
}
