package team

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Capability-aware routing (spec1.md §12; plan.md Stage 8).
//
// This is a ranking service, not an authorization service: Resolve only ever
// scores the caller-supplied eligible set, and can never add a candidate to
// it. An agent excluded from eligible — because delegation.allowed-workers,
// tool policy, or any other authorization boundary already rejected it — can
// never be selected here no matter how well its declared capabilities match,
// because it is never in the slice being scored in the first place.
//
// "Capability" here means what a worker is good at, sourced with an explicit
// trust tier, never what it is allowed to do. Granting authorization stays
// the job of delegation.allowed-workers and the tool policy gate.

// CapabilitySource records how a capability claim was established. A
// self-declared claim (extracted from the agent's own system prompt) can
// never be scored as if it were maintainer-authored config, and neither can
// be scored as if it were backed by verified outcomes — verified is reserved
// for Stage 9 (outcome learning), which has not started (plan.md Stage 9,
// 0 of 30 resolved decisions), so no code path in this package can currently
// produce a CapabilitySourceVerified record.
type CapabilitySource string

const (
	CapabilitySourceSelfDeclared       CapabilitySource = "self_declared"
	CapabilitySourceMaintainerDeclared CapabilitySource = "maintainer_declared"
	CapabilitySourceVerified           CapabilitySource = "verified"
)

// capabilityConfidenceCeiling bounds how much trust a source can ever carry,
// regardless of what confidence value it claims for itself. This is what
// makes "self-claimed capability must not auto-raise trusted score" true by
// construction rather than by convention.
var capabilityConfidenceCeiling = map[CapabilitySource]float64{
	CapabilitySourceSelfDeclared:       0.3,
	CapabilitySourceMaintainerDeclared: 0.7,
	CapabilitySourceVerified:           1.0,
}

// CapabilityRecord is one scored claim that a specific agent has a specific
// capability (spec1.md §12.1).
type CapabilityRecord struct {
	AgentID    string
	Capability string
	Confidence float64
	Source     CapabilitySource
	CostClass  string
	Freshness  time.Time
	// EvidenceRefs are opaque, advisory identifiers (e.g. artifact IDs); this
	// package never resolves or trusts their content, only that a maintainer
	// bothered to cite something.
	EvidenceRefs []string
	// Stale is set by the registry, not the caller, when Freshness is older
	// than the declaration's configured stale-after window.
	Stale bool
}

// CapabilityQuery is one routing request (spec1.md §12.1).
type CapabilityQuery struct {
	Required  []string
	Preferred []string
	RiskClass string
}

// CapabilityCandidate is one ranked, explained result (spec1.md §12.1).
type CapabilityCandidate struct {
	AgentID     string
	Score       float64
	Explanation []string
}

// CapabilityResolver ranks eligible candidates for a capability query.
// Implementations must never return a candidate absent from eligible.
type CapabilityResolver interface {
	Resolve(ctx context.Context, query CapabilityQuery, eligible []string) ([]CapabilityCandidate, error)
}

// CapabilityRegistry is the deterministic, provenance-aware CapabilityResolver
// built from a team's configured agents and its maintainer-declared registry.
type CapabilityRegistry struct {
	agents     map[string]*agent.AgentDef
	maintainer map[string][]agent.DeclaredCapability
	weights    agent.ScoringWeights
	now        func() time.Time
}

// NewCapabilityRegistry builds a registry over a team's configured agents and
// its team.yaml `capability-registry` declarations. Its scoring weights start
// at the zero value, which resolves through ScoringWeights' Effective*
// accessors to exactly the formula this package used before weights existed —
// call WithScoringWeights to configure anything else.
func NewCapabilityRegistry(agents map[string]*agent.AgentDef, maintainer map[string][]agent.DeclaredCapability) *CapabilityRegistry {
	return &CapabilityRegistry{agents: agents, maintainer: maintainer, now: time.Now}
}

// WithScoringWeights configures the deterministic weighted-scoring formula
// (spec.md v2 §12). Not calling this at all is equivalent to the zero value,
// which is exactly today's hardcoded formula.
func (r *CapabilityRegistry) WithScoringWeights(weights agent.ScoringWeights) *CapabilityRegistry {
	r.weights = weights
	return r
}

// Resolve implements CapabilityResolver. It is deterministic under fixed
// inputs: candidates are always sorted by score descending, then AgentID
// ascending, so two runs over the same eligible set and the same
// configuration produce byte-identical ordering.
func (r *CapabilityRegistry) Resolve(_ context.Context, query CapabilityQuery, eligible []string) ([]CapabilityCandidate, error) {
	if r == nil {
		return nil, fmt.Errorf("capability registry is nil")
	}
	candidates := make([]CapabilityCandidate, 0, len(eligible))
	for _, agentID := range eligible {
		score, explanation := r.scoreAgent(agentID, query)
		candidates = append(candidates, CapabilityCandidate{
			AgentID:     agentID,
			Score:       score,
			Explanation: explanation,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].AgentID < candidates[j].AgentID
	})
	return candidates, nil
}

// scoreAgent scores one candidate already known to be eligible. Required
// capabilities the candidate cannot show any record for produce a
// disqualifying zero score with a stated reason; this is what lets a caller
// distinguish "no match" from "not eligible" purely from the explanation.
func (r *CapabilityRegistry) scoreAgent(agentID string, query CapabilityQuery) (float64, []string) {
	records := r.recordsFor(agentID)

	best := make(map[string]CapabilityRecord, len(records))
	for _, rec := range records {
		key := strings.ToLower(strings.TrimSpace(rec.Capability))
		if key == "" || rec.Stale {
			continue
		}
		if existing, ok := best[key]; !ok || rec.Confidence > existing.Confidence {
			best[key] = rec
		}
	}

	var explanation []string
	var missing []string
	for _, req := range query.Required {
		key := strings.ToLower(strings.TrimSpace(req))
		if rec, ok := matchCapability(best, key); ok {
			explanation = append(explanation, fmt.Sprintf(
				"required %q matched by %q (%s, confidence %.2f)", req, rec.Capability, rec.Source, rec.Confidence))
		} else {
			missing = append(missing, req)
		}
	}
	if len(missing) > 0 {
		return 0, append(explanation, fmt.Sprintf("missing required capability: %s", strings.Join(missing, ", ")))
	}

	var score float64
	for _, req := range query.Required {
		if rec, ok := matchCapability(best, strings.ToLower(strings.TrimSpace(req))); ok {
			score += r.weights.EffectiveRequiredMatch() * rec.Confidence
		}
	}
	for _, pref := range query.Preferred {
		key := strings.ToLower(strings.TrimSpace(pref))
		if rec, ok := matchCapability(best, key); ok {
			score += r.weights.EffectivePreferredMatch() * rec.Confidence
			explanation = append(explanation, fmt.Sprintf(
				"preferred %q matched by %q (%s, confidence %.2f)", pref, rec.Capability, rec.Source, rec.Confidence))
		}
	}
	if len(query.Required) == 0 && len(query.Preferred) == 0 {
		explanation = append(explanation, "no capability requirement declared; every eligible candidate ties")
	}
	// Cost only ever affects ranking when a team explicitly configures a
	// non-zero weight (spec.md v2 §12) — it never applies to a candidate
	// already disqualified above.
	if costWeight := r.weights.EffectiveCost(); costWeight != 0 {
		if class := costClassFor(records); class != "" {
			contribution := costWeight * costClassScore(class)
			score += contribution
			explanation = append(explanation, fmt.Sprintf(
				"cost class %q contributes %.2f (weight %.2f)", class, contribution, costWeight))
		}
	}
	return score, explanation
}

// costClassFor returns the first non-empty declared cost class among
// records, in their declared order — deterministic, since records is built
// from a fixed AgentDef.Capabilities line order followed by the
// capability-registry's own declared (slice, not map) order.
func costClassFor(records []CapabilityRecord) string {
	for _, rec := range records {
		if class := strings.ToLower(strings.TrimSpace(rec.CostClass)); class != "" {
			return class
		}
	}
	return ""
}

// costClassScores maps a declared cost class to a 0-1 desirability score
// (higher is cheaper, so it can be added the same way a capability match
// is). An unrecognized or empty class scores neutrally.
var costClassScores = map[string]float64{
	"low": 1.0, "medium": 0.6, "high": 0.2,
}

func costClassScore(class string) float64 {
	if score, ok := costClassScores[class]; ok {
		return score
	}
	return 0.5
}

// matchCapability finds the highest-confidence record whose capability text
// contains the query key (or vice versa), so a maintainer-declared tag like
// "security-review" matches a self-declared prose line that mentions it.
func matchCapability(best map[string]CapabilityRecord, key string) (CapabilityRecord, bool) {
	if key == "" {
		return CapabilityRecord{}, false
	}
	if rec, ok := best[key]; ok {
		return rec, true
	}
	var found CapabilityRecord
	ok := false
	for capText, rec := range best {
		if strings.Contains(capText, key) || strings.Contains(key, capText) {
			if !ok || rec.Confidence > found.Confidence {
				found, ok = rec, true
			}
		}
	}
	return found, ok
}

// recordsFor merges self-declared (from the agent's own system prompt) and
// maintainer-declared (from team.yaml capability-registry) records for one
// agent, applying each source's confidence ceiling and staleness.
func (r *CapabilityRegistry) recordsFor(agentID string) []CapabilityRecord {
	var records []CapabilityRecord
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}

	if def := r.agents[agentID]; def != nil {
		for _, line := range strings.Split(def.Capabilities, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			records = append(records, CapabilityRecord{
				AgentID:    agentID,
				Capability: strings.ToLower(line),
				Confidence: capabilityConfidenceCeiling[CapabilitySourceSelfDeclared],
				Source:     CapabilitySourceSelfDeclared,
				Freshness:  now,
			})
		}
	}

	for _, decl := range r.maintainer[agentID] {
		ceiling := capabilityConfidenceCeiling[CapabilitySourceMaintainerDeclared]
		confidence := ceiling
		if decl.Confidence > 0 && decl.Confidence < ceiling {
			confidence = decl.Confidence
		}
		rec := CapabilityRecord{
			AgentID:    agentID,
			Capability: strings.ToLower(strings.TrimSpace(decl.Capability)),
			Confidence: confidence,
			Source:     CapabilitySourceMaintainerDeclared,
			CostClass:  decl.CostClass,
			Freshness:  now,
		}
		if strings.TrimSpace(decl.DeclaredAt) != "" {
			if declaredAt, err := time.Parse(time.RFC3339, strings.TrimSpace(decl.DeclaredAt)); err == nil {
				rec.Freshness = declaredAt
				if strings.TrimSpace(decl.StaleAfter) != "" {
					if staleAfter, err := time.ParseDuration(strings.TrimSpace(decl.StaleAfter)); err == nil {
						if now.Sub(declaredAt) > staleAfter {
							rec.Stale = true
							rec.Confidence = 0
						}
					}
				}
			}
		}
		records = append(records, rec)
	}
	return records
}

// eligibleWorkerIDs returns the authorized worker set a routing query may
// rank over: every configured agent, narrowed by delegation.allowed-workers
// when that allowlist is non-empty (mirroring the same "empty = allow all"
// rule the delegation policy itself uses). Routing can only ever narrow this
// set further by score; it can never add a name absent from it.
func (c *Coordinator) eligibleWorkerIDs() []string {
	if c == nil || c.session == nil {
		return nil
	}
	allowed := c.session.Config.Delegation.AllowedWorkers
	names := make([]string, 0, len(c.session.Agents))
	for name := range c.session.Agents {
		if len(allowed) > 0 && !slices.Contains(allowed, name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveCapabilityCandidates ranks this team's currently-authorized workers
// for query, using the team's configured capability-registry declarations
// and each candidate's self-declared capabilities.
func (c *Coordinator) ResolveCapabilityCandidates(ctx context.Context, query CapabilityQuery) ([]CapabilityCandidate, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("capability routing requires an active session")
	}
	registry := NewCapabilityRegistry(c.session.Agents, c.session.Config.CapabilityRegistry).
		WithScoringWeights(c.session.Config.RoutingPolicy.Scoring.Weights)
	return registry.Resolve(ctx, query, c.eligibleWorkerIDs())
}

// validateCapabilityRouting is the production dispatch-path wiring for
// plan.md Stage 8: it rejects a delegated task before a TODO is created when
// a configured delegation.capability-routing rule applies to the task's goal
// and the chosen worker cannot show the required capability while another
// already-authorized worker can. It never expands eligibility — a rejection
// only ever redirects the coordinator toward a worker already inside
// delegation.allowed-workers — and it never blocks when no eligible worker
// anywhere has the capability, since blocking would only stall the run for
// no benefit.
func (c *Coordinator) validateCapabilityRouting(tasks []TaskDef) error {
	if c == nil || c.session == nil || len(c.session.Config.Delegation.CapabilityRouting) == 0 {
		return nil
	}
	eligible := c.eligibleWorkerIDs()
	if len(eligible) == 0 {
		return nil
	}
	registry := NewCapabilityRegistry(c.session.Agents, c.session.Config.CapabilityRegistry).
		WithScoringWeights(c.session.Config.RoutingPolicy.Scoring.Weights)
	for taskIndex, task := range tasks {
		chosen := strings.TrimSpace(task.Agent)
		for ruleIndex, rule := range c.session.Config.Delegation.CapabilityRouting {
			if !strings.Contains(task.Goal, rule.WhenGoalContains) {
				continue
			}
			candidates, err := registry.Resolve(context.Background(), CapabilityQuery{
				Required: []string{rule.RequiredCapability},
			}, eligible)
			if err != nil {
				return fmt.Errorf("capability routing for tasks[%d]: %w", taskIndex, err)
			}
			var chosenScore float64
			var qualified []string
			for _, candidate := range candidates {
				if strings.EqualFold(candidate.AgentID, chosen) {
					chosenScore = candidate.Score
				}
				if candidate.Score > 0 {
					qualified = append(qualified, candidate.AgentID)
				}
			}
			c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
				"tasks[%d] / delegation.capability-routing[%d] required %q: chosen=%q score=%.2f qualified=%s",
				taskIndex, ruleIndex, rule.RequiredCapability, chosen, chosenScore, formatAgentNames(qualified))))
			if chosenScore <= 0 && len(qualified) > 0 {
				return c.rejectDelegationPolicy(fmt.Sprintf(
					"tasks[%d] violates delegation.capability-routing[%d]: %q requires capability %q, which %q does not show, but %s do(es)",
					taskIndex, ruleIndex, rule.WhenGoalContains, rule.RequiredCapability, chosen, formatAgentNames(qualified)))
			}
		}
	}
	return nil
}
