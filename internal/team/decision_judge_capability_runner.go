package team

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

// Real capability routing for the JUDGE stage (spec.md v2 §17-§18; spec2.md
// PR-3; plan.md Stage 8 follow-up).
//
// Every other decision stage still calls the team's judge-model sidecar
// unconditionally — that is unchanged by this file. Only when a profile sets
// judge-role does this runner resolve N authorized, capability-matched
// concrete workers (one per judge-K, round-robin over the ranked qualified
// pool) and invoke each directly, with zero tools, instead of the sidecar.
//
// Isolation is unaffected by any of this: DecisionEngine already builds one
// sealed JudgeContext per judge with no peer opinions, no aggregate, and no
// coordinator preference (decision_engine.go's BuildJudgeContext), and this
// runner never shares state across judge-K invocations — each call
// independently re-resolves the identical, deterministic candidate list and
// only reads its own rank from it.

// judgeRoleZeroTools is the fixed intersection ceiling for the JUDGE role:
// none (spec2.md §6 — a judge must reason only from the sealed evidence
// packet; live research would break the "same evidence" guarantee that makes
// independent judgments comparable). This is parity with the legacy
// judge-model sidecar, which has no tool surface at all, not a new
// restriction.
var judgeRoleZeroTools []fantasy.AgentTool

// runJudgeViaCapabilityRouting implements the capability-routed half of
// coordinatorDecisionRunners.RunJudge.
func (r *coordinatorDecisionRunners) runJudgeViaCapabilityRouting(ctx context.Context, req JudgeRequest) (DecisionOpinion, error) {
	c := r.coordinator
	if c == nil || c.session == nil {
		return DecisionOpinion{}, fmt.Errorf("judge role routing requires an active session")
	}
	role := req.RoutingRole

	candidates, err := c.ResolveCapabilityCandidates(ctx, CapabilityQuery{
		Required:  role.RequiredCapabilities,
		Preferred: role.PreferredCapabilities,
	})
	if err != nil {
		return DecisionOpinion{}, fmt.Errorf("judge role routing: %w", err)
	}
	qualified := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Score > 0 {
			qualified = append(qualified, candidate.AgentID)
		}
	}
	minDistinct := role.EffectiveMinDistinctAgents()
	if len(qualified) < minDistinct {
		return DecisionOpinion{}, fmt.Errorf(
			"judge role routing: %d qualified candidate(s) satisfy required capabilities %v, need at least %d (judge-role.min-distinct-agents)",
			len(qualified), role.RequiredCapabilities, minDistinct)
	}

	ordinal, err := judgeOrdinal(req.JudgeID)
	if err != nil {
		return DecisionOpinion{}, fmt.Errorf("judge role routing: %w", err)
	}
	// Round-robin over the ranked, qualified pool: judge-1 gets the top
	// candidate, judge-2 the second, wrapping only if the pool is smaller
	// than the judge count (which EffectiveMinDistinctAgents may forbid).
	chosen := qualified[(ordinal-1)%len(qualified)]
	def := c.session.Agents[chosen]
	if def == nil {
		return DecisionOpinion{}, fmt.Errorf("judge role routing: resolved worker %q is not a configured agent", chosen)
	}

	response, modelID, err := c.invokeCapabilityRoutedAgent(ctx, def, req.Context.Prompt, judgeRoleZeroTools, judgeRoleMaxSteps, fantasy.StepCountIs(1))
	if err != nil {
		return DecisionOpinion{}, fmt.Errorf("judge role invocation of %q for %s: %w", chosen, req.JudgeID, err)
	}
	c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
		"judge role bound %s to %q (model %q): required=%v preferred=%v",
		req.JudgeID, chosen, modelID, role.RequiredCapabilities, role.PreferredCapabilities)))

	return decodeJudgeOpinion(response, req.JudgeID)
}

// judgeRoleMaxSteps bounds a judge-role invocation. A zero-tool judge has
// nothing to do but answer once; this is a small ceiling purely as a safety
// bound, not an expected retry budget (fantasy.StepCountIs(1) below is what
// actually forces the single turn).
const judgeRoleMaxSteps = 2

// judgeOrdinal parses the 1-indexed position encoded in a judge ID (e.g.
// "judge-3" -> 3). This is the same format decision_engine.go's judgeIDs
// produces and FinalizationJudge's judge-id validates against.
func judgeOrdinal(judgeID string) (int, error) {
	const prefix = "judge-"
	if !strings.HasPrefix(judgeID, prefix) {
		return 0, fmt.Errorf("judge ID %q does not have the expected %q prefix", judgeID, prefix)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(judgeID, prefix))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("judge ID %q does not encode a positive ordinal", judgeID)
	}
	return n, nil
}
