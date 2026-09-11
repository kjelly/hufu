package team

import (
	"fmt"
	"sort"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Stop policy and checkpoint evaluation
// (docs/architecture/decision-runtime.md §29, §32).
//
// A checkpoint is evaluated after a completed tool call and makes zero LLM
// calls. That bound is deliberate: re-asking a model whether its assumptions
// still hold at every checkpoint would turn execution into a second inference
// loop. Assumption status changes only through the three sources in §18.1, and
// the checkpoint reads what the event log already recorded.
//
// The ledger owns the counters; this file only interprets them.

// CheckpointState is the observed state at one checkpoint. Every field is a
// fact the runtime already has: nothing here requires a judgment call.
type CheckpointState struct {
	Attempt             int           `json:"attempt"`
	ToolCalls           int           `json:"tool_calls"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
	NoProgressStreak    int           `json:"no_progress_streak"`
	TokensUsed          int64         `json:"tokens_used"`
	Elapsed             time.Duration `json:"elapsed"`

	// CriticalAssumptionContradicted is set when an assumption marked critical
	// has reached the contradicted state through a recorded transition.
	CriticalAssumptionContradicted bool   `json:"critical_assumption_contradicted"`
	ContradictedAssumptionID       string `json:"contradicted_assumption_id,omitempty"`

	// MaterialEvidenceChanged is set when the governing decision's sealed
	// evidence hash no longer matches the one execution was authorized under.
	MaterialEvidenceChanged bool `json:"material_evidence_changed"`

	// SideEffectState is the reconcile classification when a mutation's
	// outcome is in doubt: complete, partial, not_started or unknown.
	SideEffectState string `json:"side_effect_state,omitempty"`
}

// CheckpointOutcome is the typed result of a deterministic checkpoint. It is
// deliberately richer than the old decision-only value so the scheduler can
// apply the lifecycle consequence without re-evaluating policy.
type CheckpointOutcome struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	// Criterion names the kill criterion that fired, when one did.
	Criterion string          `json:"criterion,omitempty"`
	Detail    string          `json:"detail,omitempty"`
	Request   string          `json:"request,omitempty"`
	State     CheckpointState `json:"state"`

	DecisionID     string `json:"decision_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	BranchID       string `json:"branch_id,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// CheckpointDecision remains an alias for callers from the earlier decision
// stages. There is one runtime type and therefore one lifecycle contract.
type CheckpointDecision = CheckpointOutcome

// Checkpoint actions. They reuse the replan vocabulary so a configured replan
// action can be returned directly.
const (
	CheckpointContinue           = agent.ReplanContinue
	CheckpointStop               = agent.ReplanStop
	CheckpointReplan             = agent.ReplanReplan
	CheckpointRequestInformation = agent.ReplanRequestInformation
	CheckpointNeedsHuman         = agent.ReplanNeedsHuman
	CheckpointEscalate           = agent.ReplanEscalate
)

// ValidateStopPolicyBeforeExecute enforces that stop conditions were declared
// before resources were spent, not invented afterwards (spec §29.3).
func ValidateStopPolicyBeforeExecute(policy StopPolicy) error {
	if policy.RequireKillCriteria && len(policy.KillCriteria) == 0 {
		return &GateResult{
			Reason: ReasonStopPolicyMissingKillCriteria,
			Detail: "profile requires kill criteria and the task declared none",
		}
	}
	return nil
}

// ShouldCheckpoint reports whether a checkpoint is due after toolCalls
// completed calls in this attempt. checkpoint-every: 0 disables checkpoints.
func ShouldCheckpoint(policy StopPolicy, toolCalls int) bool {
	if policy.CheckpointEvery <= 0 || toolCalls <= 0 {
		return false
	}
	return toolCalls%policy.CheckpointEvery == 0
}

// EvaluateCheckpoint decides whether execution may continue. It is pure and
// deterministic: the same observed state always yields the same action.
//
// Order matters. Kill criteria and hard budget bounds come first because they
// were declared in advance; invalidation triggers follow, because a plan whose
// assumptions failed should replan rather than grind on. Resources already
// consumed are never a reason to continue.
func EvaluateCheckpoint(policy StopPolicy, replan ReplanPolicy, state CheckpointState) CheckpointDecision {
	if decision, hit := evaluateKillCriteria(policy, state); hit {
		return decision
	}
	if decision, hit := evaluateStopBounds(policy, state); hit {
		return decision
	}

	if state.SideEffectState == RecoveryStateUnknown {
		return CheckpointDecision{
			Action: CheckpointStop,
			Reason: ReasonReconcileUnknownState,
			Detail: "side-effect state is unknown; reconcile before any retry",
		}
	}
	if state.CriticalAssumptionContradicted {
		return CheckpointDecision{
			Action: replan.ReplanAction("critical_assumption_contradicted"),
			Reason: ReasonAssumptionInvalidated,
			Detail: fmt.Sprintf("critical assumption %s was contradicted", state.ContradictedAssumptionID),
		}
	}
	if state.MaterialEvidenceChanged {
		return CheckpointDecision{
			Action: replan.ReplanAction("material_evidence_changed"),
			Reason: ReasonDecisionStale,
			Detail: "the sealed evidence execution was authorized under has changed materially",
		}
	}
	if policy.MaxAttempts > 0 && state.ConsecutiveFailures > 0 && state.Attempt >= policy.MaxAttempts {
		return CheckpointDecision{
			Action: replan.ReplanAction("repeated_failure"),
			Reason: ReasonKillCriterionReached,
			Detail: fmt.Sprintf("attempt %d reached the configured maximum %d after repeated failure",
				state.Attempt, policy.MaxAttempts),
		}
	}
	return CheckpointDecision{Action: CheckpointContinue}
}

// evaluateStopBounds applies the policy's own hard bounds.
func evaluateStopBounds(policy StopPolicy, state CheckpointState) (CheckpointDecision, bool) {
	if policy.MaxToolCalls > 0 && state.ToolCalls >= policy.MaxToolCalls {
		return CheckpointDecision{
			Action: CheckpointStop, Reason: ReasonKillCriterionReached,
			Detail: fmt.Sprintf("tool calls %d reached the configured maximum %d", state.ToolCalls, policy.MaxToolCalls),
		}, true
	}
	if policy.MaxTokens > 0 && state.TokensUsed >= policy.MaxTokens {
		return CheckpointDecision{
			Action: CheckpointStop, Reason: ReasonKillCriterionReached,
			Detail: fmt.Sprintf("tokens %d reached the configured maximum %d", state.TokensUsed, policy.MaxTokens),
		}, true
	}
	if limit, err := policy.Duration(); err == nil && limit > 0 && state.Elapsed >= limit {
		return CheckpointDecision{
			Action: CheckpointStop, Reason: ReasonKillCriterionReached,
			Detail: fmt.Sprintf("elapsed %s reached the configured maximum %s", state.Elapsed.Round(time.Second), limit),
		}, true
	}
	return CheckpointDecision{}, false
}

// evaluateKillCriteria applies the predeclared criteria in a fixed ID order so
// two criteria firing at once always report the same one.
func evaluateKillCriteria(policy StopPolicy, state CheckpointState) (CheckpointDecision, bool) {
	criteria := append([]KillCriterion(nil), policy.KillCriteria...)
	sort.Slice(criteria, func(i, j int) bool { return criteria[i].ID < criteria[j].ID })

	for _, criterion := range criteria {
		observed, ok := observedFor(criterion.Kind, state)
		if !ok {
			continue
		}
		if criterion.Kind == agent.KillKindAssumptionInvalid {
			if !state.CriticalAssumptionContradicted {
				continue
			}
			return CheckpointDecision{
				Action: CheckpointStop, Reason: ReasonKillCriterionReached, Criterion: criterion.ID,
				Detail: fmt.Sprintf("kill criterion %q: critical assumption %s was contradicted",
					criterion.ID, state.ContradictedAssumptionID),
			}, true
		}
		if criterion.Threshold > 0 && observed >= criterion.Threshold {
			return CheckpointDecision{
				Action: CheckpointStop, Reason: ReasonKillCriterionReached, Criterion: criterion.ID,
				Detail: fmt.Sprintf("kill criterion %q: %s reached %.0f (threshold %.0f)",
					criterion.ID, criterion.Kind, observed, criterion.Threshold),
			}, true
		}
	}
	return CheckpointDecision{}, false
}

// observedFor maps a kill criterion kind to the counter it reads. Every kind is
// computable from CheckpointState; the draft's expected_value kind was removed
// because V1 has no expected-value source (spec §29.2).
func observedFor(kind string, state CheckpointState) (float64, bool) {
	switch kind {
	case agent.KillKindBudgetTokens:
		return float64(state.TokensUsed), true
	case agent.KillKindBudgetDuration:
		return state.Elapsed.Seconds(), true
	case agent.KillKindToolCalls:
		return float64(state.ToolCalls), true
	case agent.KillKindAttempts:
		return float64(state.Attempt), true
	case agent.KillKindRepeatedFailure:
		return float64(state.ConsecutiveFailures), true
	case agent.KillKindNoProgress:
		return float64(state.NoProgressStreak), true
	case agent.KillKindAssumptionInvalid:
		return 0, true
	}
	return 0, false
}
