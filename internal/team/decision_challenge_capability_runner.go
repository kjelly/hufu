package team

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// Real capability routing for CHALLENGE, and REVISE's reuse of JUDGE's
// binding (spec.md v2 §19; spec2.md PR-4).
//
// Per spec2.md's own final scope (§9-§10), PREMORTEM, AGGREGATE, and
// FINALIZE are never capability-routed roles in this design — only
// REFERENCE, JUDGE, and CHALLENGE are, with REVISE defined as reusing
// JUDGE's binding rather than being a fourth routed role.

// challengeRoleZeroTools mirrors judgeRoleZeroTools: a challenger's MVP
// scope also keeps live research off (spec2.md §7), so it gets zero tools
// regardless of what the resolved agent declares for itself.
var challengeRoleZeroTools []fantasy.AgentTool

// runChallengeViaCapabilityRouting implements the capability-routed half of
// coordinatorDecisionRunners.RunChallenge.
func (r *coordinatorDecisionRunners) runChallengeViaCapabilityRouting(ctx context.Context, req ChallengeRequest) (DecisionChallenge, error) {
	c := r.coordinator
	if c == nil || c.session == nil {
		return DecisionChallenge{}, fmt.Errorf("challenge role routing requires an active session")
	}
	role := req.RoutingRole

	chosen, def, pinned, err := resolveChallengeRoleCandidate(ctx, c, role, req.ChallengerID)
	if err != nil {
		return DecisionChallenge{}, err
	}

	response, modelID, err := c.invokeCapabilityRoutedAgent(ctx, def, req.Prompt, challengeRoleZeroTools, judgeRoleMaxSteps, fantasy.StepCountIs(1))
	if err != nil {
		return DecisionChallenge{}, fmt.Errorf("challenge role invocation of %q for %s: %w", chosen, req.ChallengerID, err)
	}
	if pinned {
		c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
			"challenge role %s pinned to %q (model %q): reason=%q", req.ChallengerID, chosen, modelID, role.Pin.Reason)))
	} else {
		c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
			"challenge role bound %s to %q (model %q): required=%v preferred=%v",
			req.ChallengerID, chosen, modelID, role.RequiredCapabilities, role.PreferredCapabilities)))
	}

	challenge, err := decodeChallengeResponse(response, req.ChallengerID)
	if err != nil {
		return DecisionChallenge{}, err
	}
	challenge.AgentID = chosen
	challenge.Model = modelID
	challenge.Provider = strings.TrimSpace(def.ProviderURL)
	if pinned {
		challenge.Pinned = true
		challenge.BindingReason = role.Pin.Reason
	}
	return challenge, nil
}

// resolveChallengeRoleCandidate picks the concrete agent for one challenger
// ordinal under role: a pin (role.Pin), if set, forces every ordinal to the
// same named agent (still subject to authorization/required-capability
// checks); otherwise it round-robins over the ranked qualified pool exactly
// as resolveJudgeRoleCandidate does for JUDGE.
func resolveChallengeRoleCandidate(ctx context.Context, c *Coordinator, role *agent.ChallengeRolePolicy, challengerID string) (chosen string, def *agent.AgentDef, pinned bool, err error) {
	if pinnedAgent, pinnedDef, ok, err := resolvePinnedCandidate(ctx, c, "challenge", role.RequiredCapabilities, role.PreferredCapabilities, role.Pin); err != nil {
		return "", nil, false, err
	} else if ok {
		return pinnedAgent, pinnedDef, true, nil
	}

	candidates, err := c.ResolveCapabilityCandidates(ctx, CapabilityQuery{
		Required:  role.RequiredCapabilities,
		Preferred: role.PreferredCapabilities,
	})
	if err != nil {
		return "", nil, false, fmt.Errorf("challenge role routing: %w", err)
	}
	qualified := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score > 0 {
			qualified = append(qualified, candidate.AgentID)
		}
	}
	minDistinct := role.EffectiveMinDistinctAgents()
	if len(qualified) < minDistinct {
		return "", nil, false, fmt.Errorf(
			"challenge role routing: %d qualified candidate(s) satisfy required capabilities %v, need at least %d (challenge-role.min-distinct-agents)",
			len(qualified), role.RequiredCapabilities, minDistinct)
	}
	if role.MinDistinctModels > 0 {
		if got := distinctModelCount(qualified, c.session.Agents); got < role.MinDistinctModels {
			return "", nil, false, fmt.Errorf(
				"challenge role routing: qualified pool spans %d distinct model(s), need at least %d (challenge-role.min-distinct-models)",
				got, role.MinDistinctModels)
		}
	}
	if role.MinDistinctProviders > 0 {
		if got := distinctProviderCount(qualified, c.session.Agents); got < role.MinDistinctProviders {
			return "", nil, false, fmt.Errorf(
				"challenge role routing: qualified pool spans %d distinct provider(s), need at least %d (challenge-role.min-distinct-providers)",
				got, role.MinDistinctProviders)
		}
	}
	ordered := resolveRoleCandidateOrder(qualified, c.session.Agents, role.PreferDistinctModels, role.PreferDistinctProviders)

	ordinal, err := challengerOrdinal(challengerID)
	if err != nil {
		return "", nil, false, fmt.Errorf("challenge role routing: %w", err)
	}
	chosen = ordered[(ordinal-1)%len(ordered)]
	def = c.session.Agents[chosen]
	if def == nil {
		return "", nil, false, fmt.Errorf("challenge role routing: resolved worker %q is not a configured agent", chosen)
	}
	return chosen, def, false, nil
}

// runRevisionViaCapabilityRouting implements the capability-routed half of
// coordinatorDecisionRunners.RunRevision. It reuses the original opinion's
// durable AgentID directly (spec2.md §8's "reuse the original binding")
// rather than blindly re-resolving and hoping the result matches — see
// revisionCandidate.
func (r *coordinatorDecisionRunners) runRevisionViaCapabilityRouting(ctx context.Context, req RevisionRequest) (DecisionRevision, error) {
	c := r.coordinator
	role := req.RoutingRole

	chosen, def, pinned, reason, err := revisionCandidate(ctx, c, role, req)
	if err != nil {
		return DecisionRevision{}, err
	}

	response, modelID, err := c.invokeCapabilityRoutedAgent(ctx, def, req.Prompt, judgeRoleZeroTools, judgeRoleMaxSteps, fantasy.StepCountIs(1))
	if err != nil {
		return DecisionRevision{}, fmt.Errorf("revision role invocation of %q for %s: %w", chosen, req.JudgeID, err)
	}
	c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
		"revision role reused %s's original binding %q (model %q)",
		req.JudgeID, chosen, modelID)))

	revision, err := decodeRevisionResult(response, req.JudgeID)
	if err != nil {
		return DecisionRevision{}, err
	}
	revision.AgentID = chosen
	if pinned {
		revision.Pinned = true
		revision.BindingReason = reason
	}
	return revision, nil
}

// revisionCandidate resolves REVISE's binding for req (spec.md v2 §35-37,
// mid-run capability invalidation). When the original opinion durably
// recorded which agent produced it (req.Original.AgentID), REVISE reuses
// that binding directly instead of re-resolving through the ranked pool —
// but only after confirming it is still authorized and still satisfies the
// role's required capabilities, failing closed otherwise rather than
// silently landing on whatever a fresh re-rank happens to produce. This is
// strictly more robust than the previous "recompute and hope it matches"
// approach: a capability that went stale between JUDGE round 1 and REVISE
// (CHALLENGE and aggregation both run in between, so real wall-clock time
// passes) is naturally excluded by CapabilityRegistry's own staleness
// filtering, so the failure here is deliberate, not a silent switch to a
// different candidate.
//
// Falls back to resolveJudgeRoleCandidate (the original recompute) only when
// the original opinion carries no AgentID at all — e.g. it was formed by the
// legacy sidecar but this profile has judge-role configured anyway, a
// misconfiguration edge case, not the normal path.
func revisionCandidate(ctx context.Context, c *Coordinator, role *agent.JudgeRolePolicy, req RevisionRequest) (chosen string, def *agent.AgentDef, pinned bool, reason string, err error) {
	original := strings.TrimSpace(req.Original.AgentID)
	if original == "" {
		chosen, def, pinned, err = resolveJudgeRoleCandidate(ctx, c, role, req.JudgeID)
		if pinned {
			reason = role.Pin.Reason
		}
		return chosen, def, pinned, reason, err
	}
	if c == nil || c.session == nil {
		return "", nil, false, "", fmt.Errorf("revision role routing requires an active session")
	}
	if !slices.Contains(c.eligibleWorkerIDs(), original) {
		return "", nil, false, "", fmt.Errorf("revision role routing: original binding %q is no longer authorized", original)
	}
	candidates, err := c.ResolveCapabilityCandidates(ctx, CapabilityQuery{
		Required:  role.RequiredCapabilities,
		Preferred: role.PreferredCapabilities,
	})
	if err != nil {
		return "", nil, false, "", fmt.Errorf("revision role routing: %w", err)
	}
	stillQualifies := false
	for _, candidate := range candidates {
		if candidate.AgentID == original && candidate.Score > 0 {
			stillQualifies = true
			break
		}
	}
	if !stillQualifies {
		return "", nil, false, "", fmt.Errorf("revision role routing: original binding %q no longer satisfies required capabilities %v", original, role.RequiredCapabilities)
	}
	originalDef := c.session.Agents[original]
	if originalDef == nil {
		return "", nil, false, "", fmt.Errorf("revision role routing: resolved worker %q is not a configured agent", original)
	}
	return original, originalDef, req.Original.Pinned, req.Original.BindingReason, nil
}

// challengerOrdinal parses the 1-indexed position encoded in a challenger ID
// (e.g. "challenger-2" -> 2), the same format decision_engine_stages.go's
// runChallenges produces.
func challengerOrdinal(challengerID string) (int, error) {
	const prefix = "challenger-"
	if !strings.HasPrefix(challengerID, prefix) {
		return 0, fmt.Errorf("challenger ID %q does not have the expected %q prefix", challengerID, prefix)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(challengerID, prefix))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("challenger ID %q does not encode a positive ordinal", challengerID)
	}
	return n, nil
}
