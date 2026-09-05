package team

import (
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestValidateStopPolicyBeforeExecute(t *testing.T) {
	if err := ValidateStopPolicyBeforeExecute(StopPolicy{}); err != nil {
		t.Fatalf("unconfigured policy rejected: %v", err)
	}
	err := ValidateStopPolicyBeforeExecute(StopPolicy{RequireKillCriteria: true})
	if err == nil || !strings.Contains(err.Error(), ReasonStopPolicyMissingKillCriteria) {
		t.Fatalf("missing kill criteria = %v, want %s", err, ReasonStopPolicyMissingKillCriteria)
	}
	ok := StopPolicy{RequireKillCriteria: true, KillCriteria: []KillCriterion{{ID: "a", Kind: agent.KillKindAttempts, Threshold: 3}}}
	if err := ValidateStopPolicyBeforeExecute(ok); err != nil {
		t.Fatalf("declared kill criteria rejected: %v", err)
	}
}

// checkpoint-every: N means every N completed tool calls in this attempt.
func TestShouldCheckpoint(t *testing.T) {
	if ShouldCheckpoint(StopPolicy{}, 5) {
		t.Fatal("checkpoint-every 0 should disable checkpoints")
	}
	every2 := StopPolicy{CheckpointEvery: 2}
	for calls, want := range map[int]bool{0: false, 1: false, 2: true, 3: false, 4: true, 6: true} {
		if got := ShouldCheckpoint(every2, calls); got != want {
			t.Errorf("ShouldCheckpoint(every=2, calls=%d) = %v, want %v", calls, got, want)
		}
	}
	every1 := StopPolicy{CheckpointEvery: 1}
	for calls := 1; calls <= 3; calls++ {
		if !ShouldCheckpoint(every1, calls) {
			t.Errorf("ShouldCheckpoint(every=1, calls=%d) = false", calls)
		}
	}
}

func TestEvaluateCheckpointKillCriteria(t *testing.T) {
	tests := []struct {
		name      string
		criterion KillCriterion
		state     CheckpointState
		wantStop  bool
	}{
		{
			name:      "token budget reached",
			criterion: KillCriterion{ID: "tokens", Kind: agent.KillKindBudgetTokens, Threshold: 1000},
			state:     CheckpointState{TokensUsed: 1000},
			wantStop:  true,
		},
		{
			name:      "token budget not reached",
			criterion: KillCriterion{ID: "tokens", Kind: agent.KillKindBudgetTokens, Threshold: 1000},
			state:     CheckpointState{TokensUsed: 999},
		},
		{
			name:      "duration reached",
			criterion: KillCriterion{ID: "clock", Kind: agent.KillKindBudgetDuration, Threshold: 60},
			state:     CheckpointState{Elapsed: 61 * time.Second},
			wantStop:  true,
		},
		{
			name:      "tool calls reached",
			criterion: KillCriterion{ID: "calls", Kind: agent.KillKindToolCalls, Threshold: 10},
			state:     CheckpointState{ToolCalls: 10},
			wantStop:  true,
		},
		{
			name:      "attempts reached",
			criterion: KillCriterion{ID: "attempts", Kind: agent.KillKindAttempts, Threshold: 3},
			state:     CheckpointState{Attempt: 3},
			wantStop:  true,
		},
		{
			name:      "repeated failure reached",
			criterion: KillCriterion{ID: "failures", Kind: agent.KillKindRepeatedFailure, Threshold: 2},
			state:     CheckpointState{ConsecutiveFailures: 2},
			wantStop:  true,
		},
		{
			name:      "no progress reached",
			criterion: KillCriterion{ID: "stalled", Kind: agent.KillKindNoProgress, Threshold: 3},
			state:     CheckpointState{NoProgressStreak: 3},
			wantStop:  true,
		},
		{
			name:      "assumption invalid fires only on a contradiction",
			criterion: KillCriterion{ID: "assumption", Kind: agent.KillKindAssumptionInvalid},
			state:     CheckpointState{},
		},
		{
			name:      "assumption invalid fires on a contradiction",
			criterion: KillCriterion{ID: "assumption", Kind: agent.KillKindAssumptionInvalid},
			state:     CheckpointState{CriticalAssumptionContradicted: true, ContradictedAssumptionID: "A1"},
			wantStop:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := StopPolicy{CheckpointEvery: 1, KillCriteria: []KillCriterion{tt.criterion}}
			decision := EvaluateCheckpoint(policy, ReplanPolicy{}, tt.state)
			if tt.wantStop {
				if decision.Action != CheckpointStop || decision.Reason != ReasonKillCriterionReached {
					t.Fatalf("decision = %#v, want a stop on %s", decision, ReasonKillCriterionReached)
				}
				if decision.Criterion != tt.criterion.ID {
					t.Fatalf("decision named criterion %q, want %q", decision.Criterion, tt.criterion.ID)
				}
				return
			}
			if decision.Action != CheckpointContinue {
				t.Fatalf("decision = %#v, want continue", decision)
			}
		})
	}
}

// Resources already spent are never a reason to keep going.
func TestCheckpointDoesNotTreatConsumptionAsProgress(t *testing.T) {
	policy := StopPolicy{
		CheckpointEvery: 1,
		KillCriteria:    []KillCriterion{{ID: "tokens", Kind: agent.KillKindBudgetTokens, Threshold: 100}},
	}
	decision := EvaluateCheckpoint(policy, ReplanPolicy{}, CheckpointState{TokensUsed: 5000, ToolCalls: 40})
	if decision.Action != CheckpointStop {
		t.Fatalf("decision = %#v, want stop", decision)
	}
}

// Two criteria firing at once always report the same one.
func TestCheckpointCriteriaOrderIsDeterministic(t *testing.T) {
	policy := StopPolicy{
		CheckpointEvery: 1,
		KillCriteria: []KillCriterion{
			{ID: "z-tokens", Kind: agent.KillKindBudgetTokens, Threshold: 10},
			{ID: "a-calls", Kind: agent.KillKindToolCalls, Threshold: 1},
		},
	}
	state := CheckpointState{TokensUsed: 100, ToolCalls: 5}
	first := EvaluateCheckpoint(policy, ReplanPolicy{}, state)
	for i := 0; i < 20; i++ {
		again := EvaluateCheckpoint(policy, ReplanPolicy{}, state)
		if again.Criterion != first.Criterion {
			t.Fatalf("criterion %q then %q", first.Criterion, again.Criterion)
		}
	}
	if first.Criterion != "a-calls" {
		t.Fatalf("criterion = %q, want the ascending-ID first match", first.Criterion)
	}
}

// A contradicted critical assumption takes the configured replan action.
func TestCheckpointAssumptionContradictionUsesReplanPolicy(t *testing.T) {
	state := CheckpointState{CriticalAssumptionContradicted: true, ContradictedAssumptionID: "A1"}

	decision := EvaluateCheckpoint(StopPolicy{CheckpointEvery: 1}, ReplanPolicy{}, state)
	if decision.Action != CheckpointContinue {
		t.Fatalf("unconfigured replan policy = %#v, want continue to preserve old behavior", decision)
	}

	replan := ReplanPolicy{OnCriticalAssumptionContradicted: agent.ReplanReplan}
	decision = EvaluateCheckpoint(StopPolicy{CheckpointEvery: 1}, replan, state)
	if decision.Action != CheckpointReplan || decision.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("decision = %#v, want a replan on %s", decision, ReasonAssumptionInvalidated)
	}
	if !strings.Contains(decision.Detail, "A1") {
		t.Fatalf("detail = %q, want the assumption named", decision.Detail)
	}
}

func TestCheckpointMaterialEvidenceChange(t *testing.T) {
	replan := ReplanPolicy{OnMaterialEvidenceChanged: agent.ReplanReplan}
	decision := EvaluateCheckpoint(StopPolicy{CheckpointEvery: 1}, replan, CheckpointState{MaterialEvidenceChanged: true})
	if decision.Action != CheckpointReplan || decision.Reason != ReasonDecisionStale {
		t.Fatalf("decision = %#v, want a replan on %s", decision, ReasonDecisionStale)
	}
}

// An unknown side-effect state never continues and never silently retries.
func TestCheckpointUnknownSideEffectStateStops(t *testing.T) {
	for _, state := range []string{RecoveryStateComplete, RecoveryStatePartial, RecoveryStateNotStarted} {
		decision := EvaluateCheckpoint(StopPolicy{CheckpointEvery: 1}, ReplanPolicy{}, CheckpointState{SideEffectState: state})
		if decision.Action != CheckpointContinue {
			t.Fatalf("state %q = %#v, want continue", state, decision)
		}
	}
	decision := EvaluateCheckpoint(StopPolicy{CheckpointEvery: 1}, ReplanPolicy{}, CheckpointState{SideEffectState: RecoveryStateUnknown})
	if decision.Action != CheckpointStop || decision.Reason != ReasonReconcileUnknownState {
		t.Fatalf("decision = %#v, want a stop on %s", decision, ReasonReconcileUnknownState)
	}
}

func TestCheckpointStopBounds(t *testing.T) {
	tests := []struct {
		name   string
		policy StopPolicy
		state  CheckpointState
	}{
		{"tool calls", StopPolicy{CheckpointEvery: 1, MaxToolCalls: 5}, CheckpointState{ToolCalls: 5}},
		{"tokens", StopPolicy{CheckpointEvery: 1, MaxTokens: 500}, CheckpointState{TokensUsed: 500}},
		{"duration", StopPolicy{CheckpointEvery: 1, MaxDuration: "30s"}, CheckpointState{Elapsed: 31 * time.Second}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision := EvaluateCheckpoint(tt.policy, ReplanPolicy{}, tt.state)
			if decision.Action != CheckpointStop {
				t.Fatalf("decision = %#v, want stop", decision)
			}
		})
	}
}

// The whole evaluation is pure: the same state always gives the same answer.
func TestEvaluateCheckpointIsDeterministic(t *testing.T) {
	policy := StopPolicy{
		CheckpointEvery: 1, MaxToolCalls: 20,
		KillCriteria: []KillCriterion{
			{ID: "tokens", Kind: agent.KillKindBudgetTokens, Threshold: 1000},
			{ID: "failures", Kind: agent.KillKindRepeatedFailure, Threshold: 3},
		},
	}
	state := CheckpointState{TokensUsed: 900, ToolCalls: 4, ConsecutiveFailures: 1, Attempt: 1}
	first := EvaluateCheckpoint(policy, ReplanPolicy{OnRepeatedFailure: agent.ReplanStop}, state)
	for i := 0; i < 50; i++ {
		again := EvaluateCheckpoint(policy, ReplanPolicy{OnRepeatedFailure: agent.ReplanStop}, state)
		if again != first {
			t.Fatalf("iteration %d gave %#v, want %#v", i, again, first)
		}
	}
}
