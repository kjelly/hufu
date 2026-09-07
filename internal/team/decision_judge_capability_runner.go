package team

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
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

// resolveJudgeRoleCandidate picks the concrete agent judgeID is bound to
// under role: the ranked, qualified candidate list is a pure function of
// role alone, and judgeID's ordinal selects a stable position in it. Calling
// this again with the same (role, judgeID) — which is exactly what a
// revision dispatch does — always yields the same candidate, with no
// persisted binding required (spec2.md §8 "REVISE = reuse JUDGE bindings").
//
// A pin (role.Pin), if set, forces every judge ordinal to the same named
// agent instead — still subject to authorization/required-capability checks
// (spec.md v2 §34) — short-circuiting the ranking below entirely.
func resolveJudgeRoleCandidate(ctx context.Context, c *Coordinator, role *agent.JudgeRolePolicy, judgeID string, requestedDispatchCount int) (chosen string, def *agent.AgentDef, pinned bool, err error) {
	ordinal, err := judgeOrdinal(judgeID)
	if err != nil {
		return "", nil, false, fmt.Errorf("judge role routing: %w", err)
	}
	if c == nil || c.session == nil {
		return "", nil, false, fmt.Errorf("judge role routing requires an active session")
	}
	if pinnedAgent, pinnedDef, ok, err := resolvePinnedCandidate(ctx, c, "judge", role.RequiredCapabilities, role.PreferredCapabilities, role.Pin); err != nil {
		return "", nil, false, err
	} else if ok {
		return pinnedAgent, pinnedDef, true, nil
	}

	dispatchCount := roleDispatchCount(ordinal, role.EffectiveMinDistinctAgents(), role.MinDistinctModels, role.MinDistinctProviders, requestedDispatchCount)
	plan, err := resolveDecisionRoleBindingPlan(ctx, c, "judge", role.RequiredCapabilities, role.PreferredCapabilities,
		dispatchCount, role.EffectiveMinDistinctAgents(), role.MinDistinctModels, role.MinDistinctProviders,
		role.PreferDistinctModels, role.PreferDistinctProviders)
	if err != nil {
		return "", nil, false, err
	}
	binding := plan[ordinal-1]
	return binding.AgentID, binding.Def, false, nil
}

func roleDispatchCount(ordinal, minAgents, minModels, minProviders, requested int) int {
	return max(ordinal, minAgents, minModels, minProviders, requested)
}

// runJudgeViaCapabilityRouting implements the capability-routed half of
// coordinatorDecisionRunners.RunJudge.
func (r *coordinatorDecisionRunners) runJudgeViaCapabilityRouting(ctx context.Context, req JudgeRequest) (DecisionOpinion, error) {
	c := r.coordinator
	role := req.RoutingRole

	chosen, def, pinned, err := resolveJudgeRoleCandidate(ctx, c, role, req.JudgeID, req.DispatchCount)
	if err != nil {
		return DecisionOpinion{}, err
	}

	response, modelID, err := c.invokeCapabilityRoutedAgent(ctx, def, req.Context.Prompt, judgeRoleZeroTools, judgeRoleMaxSteps, fantasy.StepCountIs(1))
	if err != nil {
		return DecisionOpinion{}, fmt.Errorf("judge role invocation of %q for %s: %w", chosen, req.JudgeID, err)
	}
	if pinned {
		c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
			"judge role %s pinned to %q (model %q): reason=%q", req.JudgeID, chosen, modelID, role.Pin.Reason)))
	} else {
		c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
			"judge role bound %s to %q (model %q): required=%v preferred=%v",
			req.JudgeID, chosen, modelID, role.RequiredCapabilities, role.PreferredCapabilities)))
	}

	opinion, err := decodeJudgeOpinion(response, req.JudgeID)
	if err != nil {
		return DecisionOpinion{}, err
	}
	opinion.AgentID = chosen
	opinion.Model = modelID
	opinion.Provider, err = c.canonicalProviderKey(modelID)
	if err != nil {
		return DecisionOpinion{}, fmt.Errorf("resolve judge provider identity: %w", err)
	}
	if pinned {
		opinion.Pinned = true
		opinion.BindingReason = role.Pin.Reason
	}
	return opinion, nil
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
