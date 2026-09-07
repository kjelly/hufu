package team

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

type decisionRoleBinding struct {
	AgentID  string
	Def      *agent.AgentDef
	Model    string
	Provider string
}

// resolveDecisionRoleBindingPlan resolves every ordinal before any ordinal is
// dispatched. Hard diversity floors are properties of the resulting binding
// plan, not of the candidate pool, so a pool with X,X,Y must produce X,Y for
// two dispatches when two distinct models are required.
func resolveDecisionRoleBindingPlan(
	ctx context.Context,
	c *Coordinator,
	roleName string,
	required, preferred []string,
	dispatchCount, minAgents, minModels, minProviders int,
	preferModels, preferProviders bool,
) ([]decisionRoleBinding, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("%s role routing requires an active session", roleName)
	}
	candidates, err := c.ResolveCapabilityCandidates(ctx, CapabilityQuery{Required: required, Preferred: preferred})
	if err != nil {
		return nil, fmt.Errorf("%s role routing: %w", roleName, err)
	}
	qualified := make([]decisionRoleBinding, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score <= 0 {
			continue
		}
		def := c.session.Agents[candidate.AgentID]
		if def == nil {
			return nil, fmt.Errorf("%s role routing: resolved worker %q is not a configured agent", roleName, candidate.AgentID)
		}
		model := strings.TrimSpace(c.resolveAgentModel(def, ""))
		if model == "" {
			return nil, fmt.Errorf("%s role routing: resolved worker %q has no configured model", roleName, candidate.AgentID)
		}
		provider, err := c.canonicalProviderKey(model)
		if err != nil {
			return nil, fmt.Errorf("%s role routing: resolve provider for worker %q: %w", roleName, candidate.AgentID, err)
		}
		qualified = append(qualified, decisionRoleBinding{AgentID: candidate.AgentID, Def: def, Model: model, Provider: provider})
	}
	if len(qualified) < max(1, minAgents) {
		return nil, fmt.Errorf(
			"%s role routing: %d qualified candidate(s) satisfy required capabilities %v, need at least %d (%s-role.min-distinct-agents)",
			roleName, len(qualified), required, max(1, minAgents), roleName)
	}
	if dispatchCount < 1 {
		return nil, fmt.Errorf("%s role routing: dispatch count must be positive", roleName)
	}
	ordered := orderDecisionRoleBindings(qualified, preferModels, preferProviders)
	plan := make([]decisionRoleBinding, 0, dispatchCount)
	var buildPlan func(int) bool
	buildPlan = func(ordinal int) bool {
		if ordinal == dispatchCount {
			diversity := decisionRoleBindingDiversity(plan)
			return diversity.DistinctAgentCount >= max(1, minAgents) &&
				diversity.DistinctModelCount >= minModels &&
				diversity.DistinctProviderCount >= minProviders
		}
		desired := ordinal % len(ordered)
		for offset := range len(ordered) {
			index := (desired + offset) % len(ordered)
			candidate := ordered[index]
			remaining := dispatchCount - ordinal - 1
			if !decisionRoleBindingCanSatisfy(plan, candidate, ordered, remaining, minAgents, minModels, minProviders) {
				continue
			}
			plan = append(plan, candidate)
			if buildPlan(ordinal + 1) {
				return true
			}
			plan = plan[:len(plan)-1]
		}
		return false
	}
	if !buildPlan(0) {
		return nil, fmt.Errorf("%s role routing: no binding plan satisfies the configured diversity floors", roleName)
	}
	if got := decisionRoleBindingDiversity(plan); got.DistinctAgentCount < max(1, minAgents) ||
		got.DistinctModelCount < minModels || got.DistinctProviderCount < minProviders {
		return nil, fmt.Errorf("%s role routing: binding plan achieved insufficient diversity: agents=%d models=%d providers=%d", roleName, got.DistinctAgentCount, got.DistinctModelCount, got.DistinctProviderCount)
	}
	return plan, nil
}

func (c *Coordinator) canonicalProviderKey(modelID string) (string, error) {
	if c == nil || c.providerManager == nil {
		return "", fmt.Errorf("provider manager unavailable")
	}
	policy, err := c.providerManager.ResolveProviderExecutionPolicy(modelID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(policy.ProviderKey) == "" {
		return "", fmt.Errorf("provider identity unavailable for model %q", modelID)
	}
	return strings.TrimSpace(policy.ProviderKey), nil
}

func orderDecisionRoleBindings(pool []decisionRoleBinding, preferModels, preferProviders bool) []decisionRoleBinding {
	if !preferModels && !preferProviders {
		return pool
	}
	seenModels := map[string]bool{}
	seenProviders := map[string]bool{}
	fresh := make([]decisionRoleBinding, 0, len(pool))
	repeat := make([]decisionRoleBinding, 0, len(pool))
	for _, binding := range pool {
		isFresh := (preferModels && !seenModels[binding.Model]) || (preferProviders && !seenProviders[binding.Provider])
		if isFresh {
			fresh = append(fresh, binding)
		} else {
			repeat = append(repeat, binding)
		}
		seenModels[binding.Model] = true
		seenProviders[binding.Provider] = true
	}
	return append(fresh, repeat...)
}

func decisionRoleBindingCanSatisfy(selected []decisionRoleBinding, candidate decisionRoleBinding, pool []decisionRoleBinding, remaining, minAgents, minModels, minProviders int) bool {
	selected = append(selected, candidate)
	if !decisionRoleFloorReachable(selected, pool, remaining, minAgents, func(binding decisionRoleBinding) string { return binding.AgentID }) {
		return false
	}
	if !decisionRoleFloorReachable(selected, pool, remaining, minModels, func(binding decisionRoleBinding) string { return binding.Model }) {
		return false
	}
	return decisionRoleFloorReachable(selected, pool, remaining, minProviders, func(binding decisionRoleBinding) string { return binding.Provider })
}

func decisionRoleFloorReachable(selected, pool []decisionRoleBinding, remaining, floor int, key func(decisionRoleBinding) string) bool {
	if floor <= 0 {
		return true
	}
	seen := make(map[string]bool, len(selected))
	for _, binding := range selected {
		seen[key(binding)] = true
	}
	if len(seen) >= floor {
		return true
	}
	possible := make(map[string]bool, len(seen)+len(pool))
	for value := range seen {
		possible[value] = true
	}
	for _, binding := range pool {
		possible[key(binding)] = true
	}
	return len(possible) >= floor && len(seen)+remaining >= floor
}

func decisionRoleBindingDiversity(bindings []decisionRoleBinding) BindingDiversitySummary {
	identities := make([]bindingIdentity, 0, len(bindings))
	for _, binding := range bindings {
		identities = append(identities, bindingIdentity{AgentID: binding.AgentID, Model: binding.Model, Provider: binding.Provider})
	}
	return computeBindingDiversitySummary(identities)
}

// DiversityPolicy extensions beyond MinDistinctAgents (spec.md v2 §13, §32).
//
// Capability-group distinctness is deliberately out of scope: no such
// taxonomy exists anywhere in this codebase, and within one role's candidate
// set every qualified candidate already satisfies the exact same
// required-capability set by construction, so there is no non-arbitrary way
// to define "distinct capability groups" without inventing a new taxonomy
// layer — a real product decision, not a wiring gap.

// The following helpers are retained for the pure legacy ranking tests and
// callers that operate only on declarative AgentDef data. Capability-routed
// decision execution must use resolveDecisionRoleBindingPlan above, because
// only that path has ProviderManager's canonical effective provider identity.
//
// resolveRoleCandidateOrder returns pool reordered so that, when at least
// one of preferModels/preferProviders is set, a candidate introducing a
// model or provider value not yet seen (per the enabled dimensions) sorts
// before a pure repeat — stable within each bucket, so rank order is
// preserved inside "fresh" and inside "repeat". With neither flag set, it
// returns pool completely unchanged (byte-identical to today's plain
// round-robin over the ranked pool). This is deterministic and stateless: it
// depends only on (pool, agents, flags), never on which ordinals have
// already been dispatched, which is what keeps a role's candidate
// resolution a pure function of (role, ordinal) — the same property that
// lets REVISE reuse JUDGE's binding and lets independent judges resolve in
// parallel with no shared state.
func resolveRoleCandidateOrder(pool []string, agents map[string]*agent.AgentDef, preferModels, preferProviders bool) []string {
	if !preferModels && !preferProviders {
		return pool
	}
	seenModels := map[string]bool{}
	seenProviders := map[string]bool{}
	fresh := make([]string, 0, len(pool))
	repeat := make([]string, 0, len(pool))
	for _, agentID := range pool {
		def := agents[agentID]
		model, provider := agentModelKey(def), agentProviderKey(def)

		isFresh := (preferModels && !seenModels[model]) || (preferProviders && !seenProviders[provider])
		if isFresh {
			fresh = append(fresh, agentID)
		} else {
			repeat = append(repeat, agentID)
		}
		seenModels[model] = true
		seenProviders[provider] = true
	}
	return append(fresh, repeat...)
}

// distinctModelCount/distinctProviderCount count how many distinct
// model/provider values a candidate pool could achieve, for
// MinDistinctModels/MinDistinctProviders fail-closed checks — the same
// style as the existing MinDistinctAgents floor, just on a different
// dimension of the pool.
func distinctModelCount(pool []string, agents map[string]*agent.AgentDef) int {
	seen := map[string]bool{}
	for _, agentID := range pool {
		seen[agentModelKey(agents[agentID])] = true
	}
	return len(seen)
}

func distinctProviderCount(pool []string, agents map[string]*agent.AgentDef) int {
	seen := map[string]bool{}
	for _, agentID := range pool {
		seen[agentProviderKey(agents[agentID])] = true
	}
	return len(seen)
}

func agentModelKey(def *agent.AgentDef) string {
	if def == nil {
		return ""
	}
	return strings.TrimSpace(def.Generation.Model)
}

func agentProviderKey(def *agent.AgentDef) string {
	if def == nil {
		return ""
	}
	return strings.TrimSpace(def.ProviderURL)
}

// BindingDiversitySummary reports the diversity actually achieved across one
// role's set of durable bindings (spec.md v2 §32's BindingDiversitySummary),
// computed after the fact from AgentID/Model/Provider already recorded on
// each opinion/challenge — no live agent lookup needed at record-build time.
type BindingDiversitySummary struct {
	Count                 int      `json:"count"`
	DistinctAgentCount    int      `json:"distinct_agent_count"`
	DistinctModelCount    int      `json:"distinct_model_count"`
	DistinctProviderCount int      `json:"distinct_provider_count"`
	RepeatedAgentBindings []string `json:"repeated_agent_bindings,omitempty"`
}

// bindingIdentity is the minimal shape computeBindingDiversitySummary needs
// from a durable per-stage record.
type bindingIdentity struct {
	AgentID  string
	Model    string
	Provider string
}

// computeBindingDiversitySummary is pure: the same bindings always produce
// the same summary. Entries with an empty AgentID (the legacy sidecar path)
// are excluded entirely, not counted as a distinct "unrouted" agent.
func computeBindingDiversitySummary(bindings []bindingIdentity) BindingDiversitySummary {
	agentCounts := map[string]int{}
	models := map[string]bool{}
	providers := map[string]bool{}
	var routed int
	for _, b := range bindings {
		if b.AgentID == "" {
			continue
		}
		routed++
		agentCounts[b.AgentID]++
		if b.Model != "" {
			models[b.Model] = true
		}
		if b.Provider != "" {
			providers[b.Provider] = true
		}
	}
	var repeated []string
	for agentID, count := range agentCounts {
		if count > 1 {
			repeated = append(repeated, agentID)
		}
	}
	sort.Strings(repeated)
	return BindingDiversitySummary{
		Count:                 routed,
		DistinctAgentCount:    len(agentCounts),
		DistinctModelCount:    len(models),
		DistinctProviderCount: len(providers),
		RepeatedAgentBindings: repeated,
	}
}

// judgeDiversitySummary projects opinions into a BindingDiversitySummary, or
// nil when no opinion was capability-routed at all (never emits a
// misleading all-zero summary for an unrouted team).
func judgeDiversitySummary(opinions []DecisionOpinion) *BindingDiversitySummary {
	bindings := make([]bindingIdentity, 0, len(opinions))
	for _, o := range opinions {
		bindings = append(bindings, bindingIdentity{AgentID: o.AgentID, Model: o.Model, Provider: o.Provider})
	}
	summary := computeBindingDiversitySummary(bindings)
	if summary.Count == 0 {
		return nil
	}
	return &summary
}

// challengeDiversitySummary mirrors judgeDiversitySummary for challenges.
func challengeDiversitySummary(challenges []DecisionChallenge) *BindingDiversitySummary {
	bindings := make([]bindingIdentity, 0, len(challenges))
	for _, c := range challenges {
		bindings = append(bindings, bindingIdentity{AgentID: c.AgentID, Model: c.Model, Provider: c.Provider})
	}
	summary := computeBindingDiversitySummary(bindings)
	if summary.Count == 0 {
		return nil
	}
	return &summary
}
