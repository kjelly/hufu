package team

import (
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// DiversityPolicy extensions beyond MinDistinctAgents (spec.md v2 §13, §32).
//
// Capability-group distinctness is deliberately out of scope: no such
// taxonomy exists anywhere in this codebase, and within one role's candidate
// set every qualified candidate already satisfies the exact same
// required-capability set by construction, so there is no non-arbitrary way
// to define "distinct capability groups" without inventing a new taxonomy
// layer — a real product decision, not a wiring gap.

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
