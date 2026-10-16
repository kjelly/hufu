package team

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/kjelly/hufu/internal/belief"
)

const maxBoundBeliefPolicyBytes = 1 << 20

// BoundBeliefPolicy captures proposal eligibility from the frozen catalog.
// Source policy files cannot provide these grants. It is advisory, not an
// ActionProposal and never a substitute for the execution authorization gate.
type BoundBeliefPolicy struct {
	SchemaVersion int                     `json:"schema_version"`
	ModelDigest   string                  `json:"model_digest"`
	CatalogDigest string                  `json:"catalog_digest"`
	Policy        belief.Policy           `json:"policy"`
	Candidates    []BeliefPolicyCandidate `json:"candidates"`
}

type BeliefPolicyCandidate struct {
	ID          string   `json:"id"`
	EntryDigest string   `json:"entry_digest"`
	Proposers   []string `json:"proposers"`
}

func CompileBeliefPolicy(model belief.Model, policy belief.Policy, catalog *ActionCatalogSnapshot) (BoundBeliefPolicy, error) {
	if err := belief.ValidatePolicy(policy); err != nil {
		return BoundBeliefPolicy{}, err
	}
	modelDigest, err := belief.ModelDigest(model)
	if err != nil {
		return BoundBeliefPolicy{}, err
	}
	if catalog == nil || len(policy.Choices) != len(model.Hypotheses) {
		return BoundBeliefPolicy{}, fmt.Errorf("belief policy requires a complete model mapping and an action catalog")
	}
	bound := BoundBeliefPolicy{SchemaVersion: 1, ModelDigest: modelDigest, CatalogDigest: catalog.Hash, Policy: policy, Candidates: []BeliefPolicyCandidate{}}
	bound.Policy.Choices = slices.Clone(policy.Choices)
	slices.SortFunc(bound.Policy.Choices, func(a, b belief.PolicyChoice) int { return cmp.Compare(a.HypothesisID, b.HypothesisID) })
	seen := map[string]bool{}
	for _, choice := range bound.Policy.Choices {
		if !slices.ContainsFunc(model.Hypotheses, func(h belief.Hypothesis) bool { return h.ID == choice.HypothesisID }) {
			return BoundBeliefPolicy{}, fmt.Errorf("belief policy names an unknown hypothesis")
		}
		if seen[choice.CandidateID] {
			continue
		}
		entry, ok := catalog.Lookup(choice.CandidateID)
		if !ok || entry.SideEffect != SideEffectNone {
			return BoundBeliefPolicy{}, fmt.Errorf("belief policy candidates must be existing catalog actions with side-effect none")
		}
		var proposers []string
		for _, actor := range entry.Propose {
			if slices.Contains(entry.Discover, actor) {
				proposers = append(proposers, actor)
			}
		}
		slices.Sort(proposers)
		proposers = slices.Compact(proposers)
		bound.Candidates = append(bound.Candidates, BeliefPolicyCandidate{ID: entry.ID, EntryDigest: entry.Hash, Proposers: proposers})
		seen[choice.CandidateID] = true
	}
	slices.SortFunc(bound.Candidates, func(a, b BeliefPolicyCandidate) int { return cmp.Compare(a.ID, b.ID) })
	if err := validateBoundBeliefPolicy(bound); err != nil {
		return BoundBeliefPolicy{}, err
	}
	return bound, nil
}

func validateBoundBeliefPolicy(bound BoundBeliefPolicy) error {
	if bound.SchemaVersion != 1 || !decisionDigestPattern.MatchString(bound.ModelDigest) || !runInputHashPattern.MatchString(bound.CatalogDigest) {
		return fmt.Errorf("invalid belief policy binding")
	}
	if err := belief.ValidatePolicy(bound.Policy); err != nil {
		return err
	}
	if len(bound.Candidates) < 1 || len(bound.Candidates) > belief.MaxHypotheses {
		return fmt.Errorf("invalid belief candidate count")
	}
	seen := map[string]bool{}
	for _, candidate := range bound.Candidates {
		if !actionCatalogIDPattern.MatchString(candidate.ID) || seen[candidate.ID] || !runInputHashPattern.MatchString(candidate.EntryDigest) || len(candidate.Proposers) < 1 || len(candidate.Proposers) > 128 {
			return fmt.Errorf("invalid belief candidate eligibility")
		}
		seen[candidate.ID] = true
		for i, actor := range candidate.Proposers {
			if actor == "" || len(actor) > 128 || actor != strings.ToLower(strings.TrimSpace(actor)) || strings.ContainsFunc(actor, unicode.IsControl) || i > 0 && actor <= candidate.Proposers[i-1] {
				return fmt.Errorf("invalid belief proposer identity")
			}
		}
	}
	for _, choice := range bound.Policy.Choices {
		if !seen[choice.CandidateID] {
			return fmt.Errorf("belief policy candidate has no frozen eligibility")
		}
	}
	for id := range seen {
		if !slices.ContainsFunc(bound.Policy.Choices, func(c belief.PolicyChoice) bool { return c.CandidateID == id }) {
			return fmt.Errorf("belief policy has an unused grant")
		}
	}
	return nil
}

func boundBeliefPolicyDigest(bound BoundBeliefPolicy) (string, error) {
	if err := validateBoundBeliefPolicy(bound); err != nil {
		return "", err
	}
	data, err := json.Marshal(bound, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	if len(data) > maxBoundBeliefPolicyBytes {
		return "", fmt.Errorf("bound belief policy exceeds size limit")
	}
	return beliefIdentity("bound_policy", bound), nil
}

func loadBoundBeliefPolicy(ctx context.Context, store ArtifactStore, ref ArtifactRef, expected string) (*BoundBeliefPolicy, string) {
	if store == nil || ref.Kind != "belief_policy" || ref.ID == "" || ref.SHA256 == "" {
		return nil, "policy_unavailable"
	}
	canonical, err := store.Resolve(ctx, ref)
	if err != nil || canonical.Kind != "belief_policy" {
		return nil, "policy_unavailable"
	}
	reader, err := store.Open(ctx, canonical.ID)
	if err != nil {
		return nil, "policy_unavailable"
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, maxBoundBeliefPolicyBytes+1))
	if err != nil || len(data) > maxBoundBeliefPolicyBytes {
		return nil, "policy_unavailable"
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != canonical.SHA256 {
		return nil, "policy_digest_mismatch"
	}
	var bound BoundBeliefPolicy
	if err := json.Unmarshal(data, &bound, json.RejectUnknownMembers(true)); err != nil {
		return nil, "policy_invalid"
	}
	digest, err := boundBeliefPolicyDigest(bound)
	if err != nil {
		return nil, "policy_invalid"
	}
	if digest != expected {
		return nil, "policy_digest_mismatch"
	}
	return &bound, ""
}

func boundBeliefAdvice(bound *BoundBeliefPolicy, state *belief.State, actor string, unavailable string) (belief.Advice, error) {
	if unavailable != "" {
		return belief.Advice{Unavailable: unavailable}, nil
	}
	if bound == nil || state == nil {
		return belief.Advice{Unavailable: "belief_unavailable"}, nil
	}
	if bound.ModelDigest != state.ModelDigest {
		return belief.Advice{}, fmt.Errorf("belief policy model mismatch")
	}
	advice, err := belief.Suggest(bound.Policy, *state)
	if err != nil || advice.Unavailable != "" {
		return advice, err
	}
	index := slices.IndexFunc(bound.Candidates, func(c BeliefPolicyCandidate) bool { return c.ID == advice.CandidateID })
	if index < 0 || !slices.Contains(bound.Candidates[index].Proposers, strings.ToLower(strings.TrimSpace(actor))) {
		return belief.Advice{Unavailable: "candidate_not_authorized"}, nil
	}
	return advice, nil
}
