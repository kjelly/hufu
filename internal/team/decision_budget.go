package team

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kjelly/hufu/internal/agent"
)

// Decision budget admission and explicit degradation
// (docs/architecture/decision-runtime.md §34).
//
// The default is fail-closed: a profile that cannot be run at its configured
// rigor does not quietly run at less. A team may opt into degradation, but the
// ladder is fixed, quality gates are never on it, and every step is recorded so
// the reduction can never be silent.

// defaultJudgeTokenEstimate is a floor, not a cost model: it answers "can this
// run afford to dispatch these judges at all", which is the question a
// fail-closed admission check has to answer before any of them start. A profile
// that wants a real allowance sets max-tokens.
const defaultJudgeTokenEstimate = 2000

// degradationLadder names the fixed order in which rigor is reduced.
const (
	degradeChallengeCount = "challenge.count"
	degradeRevision       = "revision"
	degradeJudgeCount     = "independent-judgments"
)

// requiredDecisionTokens is the allowance a policy needs for one decision.
func requiredDecisionTokens(policy DecisionPolicy) int64 {
	if policy.MaxTokens > 0 {
		return policy.MaxTokens
	}
	judges := int64(policy.IndependentJudgments)
	if judges < 1 {
		judges = 1
	}
	rounds := int64(policy.EffectiveMaxRounds())
	challengers := int64(policy.Challenge.Count)
	dispatches := judges*rounds + challengers
	return dispatches * defaultJudgeTokenEstimate
}

// remainingDecisionTokens returns the run's unspent allowance, or -1 when the
// run has no token ceiling at all.
func remainingDecisionTokens(budget BudgetManager) int64 {
	if budget == nil {
		return -1
	}
	limits := budget.Limits()
	if limits.MaxTokens <= 0 {
		return -1
	}
	remaining := limits.MaxTokens - budget.TokensUsed() - budget.Reserved()
	if remaining < 0 {
		return 0
	}
	return remaining
}

// admitBudget checks whether the run can support the configured rigor, applying
// the explicit degradation ladder when the team opted into it. It returns the
// policy actually in force plus every degradation applied.
func (e *decisionEngine) admitBudget(ctx context.Context, req DecisionRequest, state decisionState) (DecisionPolicy, []DecisionDegradation, error) {
	policy := req.Policy
	remaining := remainingDecisionTokens(e.services.Budget)
	if remaining < 0 || remaining >= requiredDecisionTokens(policy) {
		return policy, nil, nil
	}

	if policy.EffectiveBudgetDegradation() != agent.BudgetDegradationExplicit {
		return DecisionPolicy{}, nil, fmt.Errorf(
			"%s: profile %q needs %d tokens but only %d remain, and budget-degradation is %q",
			ReasonDecisionBudgetInsufficient, req.Profile, requiredDecisionTokens(policy), remaining,
			policy.EffectiveBudgetDegradation())
	}

	var applied []DecisionDegradation
	for _, step := range []func(*DecisionPolicy) (DecisionDegradation, bool){
		degradeChallenge,
		degradeRevisionRound,
		degradeJudges,
	} {
		if remaining >= requiredDecisionTokens(policy) {
			break
		}
		degradation, ok := step(&policy)
		if !ok {
			continue
		}
		degradation.Reason = fmt.Sprintf("insufficient budget: %d tokens remain", remaining)
		degradation.Timestamp = e.now()
		applied = append(applied, degradation)
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionBudgetDegraded, decisionEvent{
			DecisionID: req.DecisionID, Profile: req.Profile, Degradation: &degradation,
		}); err != nil {
			return DecisionPolicy{}, nil, err
		}
	}

	if remaining < requiredDecisionTokens(policy) {
		return DecisionPolicy{}, nil, fmt.Errorf(
			"%s: profile %q still needs %d tokens after degradation but only %d remain",
			ReasonDecisionBudgetInsufficient, req.Profile, requiredDecisionTokens(policy), remaining)
	}
	_ = state
	return policy, applied, nil
}

// Step 1: reduce challenge fan-out to a single challenger.
func degradeChallenge(policy *DecisionPolicy) (DecisionDegradation, bool) {
	if policy.Challenge.Count <= 1 {
		return DecisionDegradation{}, false
	}
	from := policy.Challenge.Count
	policy.Challenge.Count = 1
	return DecisionDegradation{
		Step: degradeChallengeCount,
		From: strconv.Itoa(from),
		To:   "1",
	}, true
}

// Step 2: drop the independent revision round.
func degradeRevisionRound(policy *DecisionPolicy) (DecisionDegradation, bool) {
	if policy.EffectiveMaxRounds() <= 1 && !policy.Revision.Enabled {
		return DecisionDegradation{}, false
	}
	from := strconv.Itoa(policy.EffectiveMaxRounds())
	policy.MaxRounds = 1
	policy.Revision.Enabled = false
	return DecisionDegradation{
		Step: degradeRevision,
		From: from,
		To:   "1",
	}, true
}

// Step 3: reduce judges to the profile's declared floor, never below it.
func degradeJudges(policy *DecisionPolicy) (DecisionDegradation, bool) {
	floor := policy.EffectiveMinJudgments()
	if policy.IndependentJudgments <= floor {
		return DecisionDegradation{}, false
	}
	from := policy.IndependentJudgments
	policy.IndependentJudgments = floor
	return DecisionDegradation{
		Step: degradeJudgeCount,
		From: strconv.Itoa(from),
		To:   strconv.Itoa(floor),
	}, true
}
