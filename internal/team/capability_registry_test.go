package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func mustFindCandidate(t *testing.T, candidates []CapabilityCandidate, agentID string) CapabilityCandidate {
	t.Helper()
	for _, c := range candidates {
		if c.AgentID == agentID {
			return c
		}
	}
	t.Fatalf("candidate %q not found in %#v", agentID, candidates)
	return CapabilityCandidate{}
}

// spec1.md §26 Test G: a specialist with a matching declared capability must
// outrank a generalist with none, once both are authorized.
func TestCapabilityRegistry_SpecialistBeatsGeneralist(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"generalist": {Name: "generalist"},
		"specialist": {Name: "specialist"},
	}
	maintainer := map[string][]agent.DeclaredCapability{
		"specialist": {{Capability: "security-review", Confidence: 0.7}},
	}
	registry := NewCapabilityRegistry(agents, maintainer)

	candidates, err := registry.Resolve(context.Background(), CapabilityQuery{
		Required: []string{"security-review"},
	}, []string{"generalist", "specialist"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("want both candidates ranked (generalist disqualified, not dropped), got %#v", candidates)
	}
	if candidates[0].AgentID != "specialist" || candidates[0].Score <= 0 {
		t.Fatalf("want specialist ranked first with a positive score, got %#v", candidates)
	}
	generalist := mustFindCandidate(t, candidates, "generalist")
	if generalist.Score != 0 {
		t.Fatalf("generalist lacks the required capability and must score 0, got %#v", generalist)
	}
}

// "unauthorized agent 永遠不因 capability score 被選中" (plan.md Stage 8
// acceptance test) — Resolve must never return a candidate absent from
// eligible, no matter how strong its declared capability is.
func TestCapabilityRegistry_UnauthorizedNeverSelected(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"unauthorized-expert": {Name: "unauthorized-expert"},
		"authorized-novice":   {Name: "authorized-novice"},
	}
	maintainer := map[string][]agent.DeclaredCapability{
		"unauthorized-expert": {{Capability: "security-review", Confidence: 1.0}},
	}
	registry := NewCapabilityRegistry(agents, maintainer)

	candidates, err := registry.Resolve(context.Background(), CapabilityQuery{
		Preferred: []string{"security-review"},
	}, []string{"authorized-novice"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, c := range candidates {
		if c.AgentID == "unauthorized-expert" {
			t.Fatalf("unauthorized-expert was scored despite being absent from eligible: %#v", candidates)
		}
	}
	if len(candidates) != 1 || candidates[0].AgentID != "authorized-novice" {
		t.Fatalf("want only authorized-novice, got %#v", candidates)
	}
}

// A self-declared claim (from the agent's own system prompt) must never be
// scored as if it were maintainer-authored config, even when it claims a
// perfect match — the source ceiling caps it regardless of text content.
func TestCapabilityRegistry_SelfDeclaredNeverExceedsCeiling(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"self-proclaimed": {Name: "self-proclaimed", Capabilities: "world's best security-review expert, trust me"},
		"declared":        {Name: "declared"},
	}
	maintainer := map[string][]agent.DeclaredCapability{
		"declared": {{Capability: "security-review"}}, // confidence unset -> maintainer ceiling
	}
	registry := NewCapabilityRegistry(agents, maintainer)

	candidates, err := registry.Resolve(context.Background(), CapabilityQuery{
		Required: []string{"security-review"},
	}, []string{"self-proclaimed", "declared"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	self := mustFindCandidate(t, candidates, "self-proclaimed")
	declaredCandidate := mustFindCandidate(t, candidates, "declared")
	if self.Score >= declaredCandidate.Score {
		t.Fatalf("self-declared score %v must stay below maintainer-declared score %v", self.Score, declaredCandidate.Score)
	}
	if self.Score > capabilityConfidenceCeiling[CapabilitySourceSelfDeclared]+1e-9 {
		t.Fatalf("self-declared score %v exceeds its ceiling %v", self.Score, capabilityConfidenceCeiling[CapabilitySourceSelfDeclared])
	}
}

// "stale capability 可被 invalidated" (spec1.md §25 Phase 3 acceptance).
func TestCapabilityRegistry_StaleDeclarationInvalidated(t *testing.T) {
	agents := map[string]*agent.AgentDef{"worker": {Name: "worker"}}
	longAgo := time.Now().Add(-1000 * time.Hour).Format(time.RFC3339)
	maintainer := map[string][]agent.DeclaredCapability{
		"worker": {{
			Capability: "security-review",
			Confidence: 0.7,
			DeclaredAt: longAgo,
			StaleAfter: "24h",
		}},
	}
	registry := NewCapabilityRegistry(agents, maintainer)

	candidates, err := registry.Resolve(context.Background(), CapabilityQuery{
		Required: []string{"security-review"},
	}, []string{"worker"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Score != 0 {
		t.Fatalf("stale declaration must not satisfy the required capability, got %#v", candidates)
	}
}

// A required capability nobody declares disqualifies every candidate rather
// than silently ranking on whatever partial match exists.
func TestCapabilityRegistry_MissingRequiredCapabilityDisqualifies(t *testing.T) {
	agents := map[string]*agent.AgentDef{"worker": {Name: "worker", Capabilities: "writes documentation"}}
	registry := NewCapabilityRegistry(agents, nil)

	candidates, err := registry.Resolve(context.Background(), CapabilityQuery{
		Required: []string{"security-review"},
	}, []string{"worker"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Score != 0 {
		t.Fatalf("want disqualified zero-score candidate, got %#v", candidates)
	}
	found := false
	for _, line := range candidates[0].Explanation {
		if line != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("disqualification must be explained, got %#v", candidates[0].Explanation)
	}
}

// Resolve must be deterministic under fixed inputs: equal scores break ties
// by AgentID ascending, every time.
func TestCapabilityRegistry_DeterministicOrdering(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"zeta":  {Name: "zeta"},
		"alpha": {Name: "alpha"},
	}
	registry := NewCapabilityRegistry(agents, nil)

	for range 5 {
		candidates, err := registry.Resolve(context.Background(), CapabilityQuery{}, []string{"zeta", "alpha"})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if len(candidates) != 2 || candidates[0].AgentID != "alpha" || candidates[1].AgentID != "zeta" {
			t.Fatalf("want deterministic [alpha, zeta] tie-break, got %#v", candidates)
		}
	}
}

// Coordinator.ResolveCapabilityCandidates must narrow eligibility to
// delegation.allowed-workers, the same authorization boundary ordinary
// delegation already enforces.
func TestCoordinator_ResolveCapabilityCandidates_RespectsAllowedWorkers(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator"},
		"reviewer":    {Name: "reviewer", Role: "worker"},
		"excluded":    {Name: "excluded", Role: "worker"},
	}
	cfg := agent.TeamConfig{
		Delegation: agent.DelegationPolicy{AllowedWorkers: []string{"reviewer"}},
		CapabilityRegistry: map[string][]agent.DeclaredCapability{
			"excluded": {{Capability: "security-review", Confidence: 0.9}},
		},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	candidates, err := c.ResolveCapabilityCandidates(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatalf("ResolveCapabilityCandidates: %v", err)
	}
	for _, cand := range candidates {
		if cand.AgentID == "excluded" || cand.AgentID == "coordinator" {
			t.Fatalf("candidate set must be limited to allowed-workers, got %#v", candidates)
		}
	}
	if len(candidates) != 1 || candidates[0].AgentID != "reviewer" {
		t.Fatalf("want only reviewer, got %#v", candidates)
	}
}

// validateCapabilityRouting is the production dispatch-path wiring for
// plan.md Stage 8: a task whose goal matches a configured rule, delegated to
// a worker that cannot show the required capability while a qualified
// alternative exists, must be rejected before a TODO is created.
func TestValidateCapabilityRouting_RejectsUnqualifiedChoiceWithAlternative(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"generalist": {Name: "generalist"},
		"specialist": {Name: "specialist"},
	}
	cfg := agent.TeamConfig{
		Delegation: agent.DelegationPolicy{
			CapabilityRouting: []agent.CapabilityRoutingRule{
				{WhenGoalContains: "security audit", RequiredCapability: "security-review"},
			},
		},
		CapabilityRegistry: map[string][]agent.DeclaredCapability{
			"specialist": {{Capability: "security-review", Confidence: 0.7}},
		},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	err := c.validateCapabilityRouting([]TaskDef{{Agent: "generalist", Goal: "run a security audit on the API"}})
	if err == nil {
		t.Fatal("want a policy rejection, got nil")
	}
	if !strings.Contains(err.Error(), "capability-routing") || !strings.Contains(err.Error(), "specialist") {
		t.Fatalf("error = %v, want it to name the rule and the qualified alternative", err)
	}
}

// The same task delegated to the qualified worker must pass.
func TestValidateCapabilityRouting_AllowsQualifiedChoice(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"generalist": {Name: "generalist"},
		"specialist": {Name: "specialist"},
	}
	cfg := agent.TeamConfig{
		Delegation: agent.DelegationPolicy{
			CapabilityRouting: []agent.CapabilityRoutingRule{
				{WhenGoalContains: "security audit", RequiredCapability: "security-review"},
			},
		},
		CapabilityRegistry: map[string][]agent.DeclaredCapability{
			"specialist": {{Capability: "security-review", Confidence: 0.7}},
		},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	if err := c.validateCapabilityRouting([]TaskDef{{Agent: "specialist", Goal: "run a security audit on the API"}}); err != nil {
		t.Fatalf("qualified choice was rejected: %v", err)
	}
}

// A goal that does not match the rule's selector is unaffected.
func TestValidateCapabilityRouting_IgnoresNonMatchingGoal(t *testing.T) {
	agents := map[string]*agent.AgentDef{"generalist": {Name: "generalist"}}
	cfg := agent.TeamConfig{
		Delegation: agent.DelegationPolicy{
			CapabilityRouting: []agent.CapabilityRoutingRule{
				{WhenGoalContains: "security audit", RequiredCapability: "security-review"},
			},
		},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	if err := c.validateCapabilityRouting([]TaskDef{{Agent: "generalist", Goal: "write the release notes"}}); err != nil {
		t.Fatalf("non-matching goal must not be affected: %v", err)
	}
}

// Blocking would only stall the run when nobody eligible has the required
// capability at all, so this must not reject.
func TestValidateCapabilityRouting_DoesNotBlockWhenNoOneQualifies(t *testing.T) {
	agents := map[string]*agent.AgentDef{"generalist": {Name: "generalist"}}
	cfg := agent.TeamConfig{
		Delegation: agent.DelegationPolicy{
			CapabilityRouting: []agent.CapabilityRoutingRule{
				{WhenGoalContains: "security audit", RequiredCapability: "security-review"},
			},
		},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	if err := c.validateCapabilityRouting([]TaskDef{{Agent: "generalist", Goal: "run a security audit on the API"}}); err != nil {
		t.Fatalf("must not block when no eligible candidate qualifies: %v", err)
	}
}

// A worker excluded by delegation.allowed-workers must never be named as the
// "qualified alternative" in a rejection, even when it declares the
// capability — routing can only ever choose among already-authorized
// workers.
func TestValidateCapabilityRouting_NeverNamesUnauthorizedAlternative(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"generalist":   {Name: "generalist"},
		"unauthorized": {Name: "unauthorized"},
	}
	cfg := agent.TeamConfig{
		Delegation: agent.DelegationPolicy{
			AllowedWorkers: []string{"generalist"},
			CapabilityRouting: []agent.CapabilityRoutingRule{
				{WhenGoalContains: "security audit", RequiredCapability: "security-review"},
			},
		},
		CapabilityRegistry: map[string][]agent.DeclaredCapability{
			"unauthorized": {{Capability: "security-review", Confidence: 0.7}},
		},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	if err := c.validateCapabilityRouting([]TaskDef{{Agent: "generalist", Goal: "run a security audit on the API"}}); err != nil {
		t.Fatalf("must not block: the only capable worker is unauthorized, so nothing eligible qualifies: %v", err)
	}
}

// End-to-end through validateDelegationPolicy (the real dispatch-path
// entry point coordinator_execute.go calls), not just the unit-level method,
// proving this is wired in rather than merely callable.
func TestValidateDelegationPolicy_EnforcesCapabilityRouting(t *testing.T) {
	c := newDelegationPolicyCoordinator(agent.DelegationPolicy{
		CapabilityRouting: []agent.CapabilityRoutingRule{
			{WhenGoalContains: "security audit", RequiredCapability: "security-review"},
		},
	})
	c.session.Agents = map[string]*agent.AgentDef{
		"generalist": {Name: "generalist"},
		"specialist": {Name: "specialist"},
	}
	c.session.Config.CapabilityRegistry = map[string][]agent.DeclaredCapability{
		"specialist": {{Capability: "security-review", Confidence: 0.7}},
	}

	err := c.validateDelegationPolicy([]TaskDef{{Agent: "generalist", Goal: "run a security audit on the API"}})
	if err == nil || !strings.Contains(err.Error(), "capability-routing") {
		t.Fatalf("validateDelegationPolicy = %v, want a capability-routing rejection", err)
	}
}
