package agent

import (
	"math"
	"strings"
	"testing"
)

// validPolicy is the smallest policy that passes validation; tests mutate one
// field at a time so a failure names exactly one rule.
func validPolicy() DecisionPolicy {
	return DecisionPolicy{
		IndependentJudgments: 3,
		ContextIsolation:     DecisionIsolationStrict,
		ScoreScale:           DecisionScoreScale,
		Aggregation:          AggregationPolicy{Method: AggregationMeanScore},
		MaxRounds:            2,
	}
}

func TestDecisionPolicyValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*DecisionPolicy)
		wantErr string
	}{
		{name: "baseline", mutate: func(*DecisionPolicy) {}},
		{
			name:    "judges below one",
			mutate:  func(p *DecisionPolicy) { p.IndependentJudgments = 0 },
			wantErr: "independent-judgments must be >= 1",
		},
		{
			name:    "min judges exceeds judges",
			mutate:  func(p *DecisionPolicy) { p.MinIndependentJudgments = 4 },
			wantErr: "must not exceed independent-judgments",
		},
		{
			name:   "min judges defaults to judges",
			mutate: func(p *DecisionPolicy) { p.MinIndependentJudgments = 0 },
		},
		{
			name:    "max rounds above limit",
			mutate:  func(p *DecisionPolicy) { p.MaxRounds = 3 },
			wantErr: "max-rounds must be between 1 and 2",
		},
		{
			name:    "unknown isolation",
			mutate:  func(p *DecisionPolicy) { p.ContextIsolation = "loose" },
			wantErr: "context-isolation must be",
		},
		{
			name:    "unsupported score scale",
			mutate:  func(p *DecisionPolicy) { p.ScoreScale = "0-100" },
			wantErr: "score-scale must be",
		},
		{
			name:    "unknown aggregation",
			mutate:  func(p *DecisionPolicy) { p.Aggregation.Method = "vibes" },
			wantErr: "aggregation.method",
		},
		{
			name:    "challenge count above cap",
			mutate:  func(p *DecisionPolicy) { p.Challenge.Count = 4 },
			wantErr: "challenge.count must be between 0 and 3",
		},
		{
			name:    "negative dispersion trigger",
			mutate:  func(p *DecisionPolicy) { p.Challenge.Trigger = &ChallengeTrigger{DispersionAbove: -1} },
			wantErr: "dispersion-above must be a finite value",
		},
		{
			name:    "NaN dispersion trigger",
			mutate:  func(p *DecisionPolicy) { p.Challenge.Trigger = &ChallengeTrigger{DispersionAbove: math.NaN()} },
			wantErr: "dispersion-above must be a finite value",
		},
		{
			name:    "empty criterion id",
			mutate:  func(p *DecisionPolicy) { p.Criteria = []DecisionCriterion{{Weight: 1}} },
			wantErr: "criteria[0].id must not be empty",
		},
		{
			name: "duplicate criterion id",
			mutate: func(p *DecisionPolicy) {
				p.Criteria = []DecisionCriterion{{ID: "cost", Weight: 1}, {ID: "cost", Weight: 2}}
			},
			wantErr: "is duplicated",
		},
		{
			name:    "zero criterion weight",
			mutate:  func(p *DecisionPolicy) { p.Criteria = []DecisionCriterion{{ID: "cost", Weight: 0}} },
			wantErr: "weight must be a finite value > 0",
		},
		{
			name:    "infinite criterion weight",
			mutate:  func(p *DecisionPolicy) { p.Criteria = []DecisionCriterion{{ID: "cost", Weight: math.Inf(1)}} },
			wantErr: "weight must be a finite value > 0",
		},
		{
			name: "unknown criterion direction",
			mutate: func(p *DecisionPolicy) {
				p.Criteria = []DecisionCriterion{{ID: "cost", Weight: 1, Direction: "sideways"}}
			},
			wantErr: "direction",
		},
		{
			name:    "unknown finalization mode",
			mutate:  func(p *DecisionPolicy) { p.Finalization.Mode = "coin-flip" },
			wantErr: "finalization.mode",
		},
		{
			name:    "judge finalization without judge id",
			mutate:  func(p *DecisionPolicy) { p.Finalization.Mode = FinalizationJudge },
			wantErr: "finalization.judge-id is required",
		},
		{
			name: "judge finalization with canonical judge id",
			mutate: func(p *DecisionPolicy) {
				p.Finalization = FinalizationPolicy{Mode: FinalizationJudge, JudgeID: "judge-1"}
			},
		},
		{
			name: "judge finalization with free-form judge id",
			mutate: func(p *DecisionPolicy) {
				p.Finalization = FinalizationPolicy{Mode: FinalizationJudge, JudgeID: "reviewer"}
			},
			wantErr: "is not one of the configured judges",
		},
		{
			name: "judge finalization with out-of-range judge id",
			mutate: func(p *DecisionPolicy) {
				p.Finalization = FinalizationPolicy{Mode: FinalizationJudge, JudgeID: "judge-4"}
			},
			wantErr: "is not one of the configured judges",
		},
		{
			name:    "unknown budget degradation",
			mutate:  func(p *DecisionPolicy) { p.BudgetDegradation = "silent" },
			wantErr: "budget-degradation must be",
		},
		{
			name:    "negative max tokens",
			mutate:  func(p *DecisionPolicy) { p.MaxTokens = -1 },
			wantErr: "max-tokens must not be negative",
		},
		{
			name: "unknown kill criterion kind",
			mutate: func(p *DecisionPolicy) {
				p.Discipline.Stop.KillCriteria = []KillCriterion{{ID: "ev", Kind: "expected_value"}}
			},
			wantErr: ReasonStopPolicyUnknownKillKind,
		},
		{
			name: "computable kill criterion kind",
			mutate: func(p *DecisionPolicy) {
				p.Discipline.Stop.KillCriteria = []KillCriterion{{ID: "tokens", Kind: KillKindBudgetTokens, Threshold: 1000}}
			},
		},
		{
			name: "duplicate kill criterion id",
			mutate: func(p *DecisionPolicy) {
				p.Discipline.Stop.KillCriteria = []KillCriterion{
					{ID: "a", Kind: KillKindAttempts},
					{ID: "a", Kind: KillKindNoProgress},
				}
			},
			wantErr: "is duplicated",
		},
		{
			name:    "invalid stop duration",
			mutate:  func(p *DecisionPolicy) { p.Discipline.Stop.MaxDuration = "soon" },
			wantErr: "parsing stop.max-duration",
		},
		{
			name:   "valid stop duration",
			mutate: func(p *DecisionPolicy) { p.Discipline.Stop.MaxDuration = "15m" },
		},
		{
			name:    "unknown side effect class",
			mutate:  func(p *DecisionPolicy) { p.Discipline.Commit.RequiredForSideEffects = []string{"nuclear"} },
			wantErr: "is not a known side-effect class",
		},
		{
			name:    "unknown replan action",
			mutate:  func(p *DecisionPolicy) { p.Discipline.Replan.OnRepeatedFailure = "panic" },
			wantErr: "is not supported",
		},
		{
			name:   "known replan action",
			mutate: func(p *DecisionPolicy) { p.Discipline.Replan.OnRepeatedFailure = ReplanStop },
		},
		{
			name:    "negative independent groups",
			mutate:  func(p *DecisionPolicy) { p.Discipline.Evidence.RequiredIndependentGroups = -1 },
			wantErr: "required-independent-groups must not be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPolicy()
			tt.mutate(&p)
			err := p.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecisionConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     DecisionConfig
		wantErr string
	}{
		{name: "empty config is valid"},
		{
			name: "default profile off needs no declaration",
			cfg:  DecisionConfig{DefaultProfile: DecisionProfileOff},
		},
		{
			name:    "undefined default profile",
			cfg:     DecisionConfig{DefaultProfile: "standard"},
			wantErr: ReasonDecisionProfileUnknown,
		},
		{
			name: "reserved profile name declared",
			cfg: DecisionConfig{Profiles: map[string]DecisionPolicy{
				DecisionProfileOff: validPolicy(),
			}},
			wantErr: "reserved profile name",
		},
		{
			name: "invalid nested profile is reported with its name",
			cfg: DecisionConfig{Profiles: map[string]DecisionPolicy{
				"standard": {IndependentJudgments: 0},
			}},
			wantErr: "decision.profiles.standard",
		},
		{
			name: "valid profile",
			cfg: DecisionConfig{
				DefaultProfile: "standard",
				Profiles:       map[string]DecisionPolicy{"standard": validPolicy()},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecisionPolicyEffectiveDefaults(t *testing.T) {
	var p DecisionPolicy
	p.IndependentJudgments = 5
	if got := p.EffectiveMinJudgments(); got != 5 {
		t.Fatalf("EffectiveMinJudgments() = %d, want 5", got)
	}
	if got := p.EffectiveMaxRounds(); got != 1 {
		t.Fatalf("EffectiveMaxRounds() = %d, want 1", got)
	}
	if got := p.EffectiveIsolation(); got != DecisionIsolationStrict {
		t.Fatalf("EffectiveIsolation() = %q, want %q", got, DecisionIsolationStrict)
	}
	if got := p.EffectiveAggregation(); got != AggregationMeanScore {
		t.Fatalf("EffectiveAggregation() = %q, want %q", got, AggregationMeanScore)
	}
	if got := p.EffectiveFinalization(); got != FinalizationAggregate {
		t.Fatalf("EffectiveFinalization() = %q, want %q", got, FinalizationAggregate)
	}
	if got := p.EffectiveBudgetDegradation(); got != BudgetDegradationForbidden {
		t.Fatalf("EffectiveBudgetDegradation() = %q, want %q", got, BudgetDegradationForbidden)
	}
}

// An unconfigured ReplanPolicy must preserve old behavior: continue.
func TestReplanPolicyDefaultsToContinue(t *testing.T) {
	var r ReplanPolicy
	for _, trigger := range []string{"critical_assumption_contradicted", "material_evidence_changed", "repeated_failure", "unrecognized"} {
		if got := r.ReplanAction(trigger); got != ReplanContinue {
			t.Fatalf("ReplanAction(%q) = %q, want %q", trigger, got, ReplanContinue)
		}
	}
	r.OnRepeatedFailure = ReplanStop
	if got := r.ReplanAction("repeated_failure"); got != ReplanStop {
		t.Fatalf("ReplanAction(repeated_failure) = %q, want %q", got, ReplanStop)
	}
}

func TestStopPolicyDuration(t *testing.T) {
	if d, err := (StopPolicy{}).Duration(); err != nil || d != 0 {
		t.Fatalf("Duration() = %v, %v, want 0, nil", d, err)
	}
	d, err := StopPolicy{MaxDuration: "90s"}.Duration()
	if err != nil || d.Seconds() != 90 {
		t.Fatalf("Duration() = %v, %v, want 90s, nil", d, err)
	}
	if _, err := (StopPolicy{MaxDuration: "-5s"}).Duration(); err == nil {
		t.Fatal("Duration() = nil error, want negative-duration rejection")
	}
}

// Reason codes and event names are part of the runtime contract: they are
// asserted so a rename cannot slip through silently (spec §37).
func TestDecisionReasonCodesAreStableAndUnique(t *testing.T) {
	seen := make(map[string]bool, len(DecisionReasonCodes))
	for _, code := range DecisionReasonCodes {
		if code == "" {
			t.Fatal("reason code must not be empty")
		}
		if seen[code] {
			t.Fatalf("duplicate reason code %q", code)
		}
		seen[code] = true
	}
	for _, want := range []string{
		"decision_profile_unknown",
		"decision_no_no_go_option",
		"decision_outside_view_missing",
		"decision_insufficient_valid_opinions",
		"commit_gate_missing_observability",
		"stop_policy_unknown_kill_kind",
	} {
		if !seen[want] {
			t.Fatalf("reason code %q is missing from DecisionReasonCodes", want)
		}
	}
}

func TestDecisionEventTypesAreStableAndUnique(t *testing.T) {
	seen := make(map[string]bool, len(DecisionEventTypes))
	for _, name := range DecisionEventTypes {
		if name == "" {
			t.Fatal("event type must not be empty")
		}
		if seen[name] {
			t.Fatalf("duplicate event type %q", name)
		}
		if strings.ContainsAny(name, ".- ") {
			t.Fatalf("event type %q must use the existing snake_case convention", name)
		}
		seen[name] = true
	}
	if !seen["decision_evidence_sealed"] || !seen["commit_gate_blocked"] {
		t.Fatal("canonical decision event types are missing")
	}
}

// expected_value was removed because V1 has no computable expected-value
// source; the config layer must reject it rather than accept a dead knob.
func TestExpectedValueKillKindRejected(t *testing.T) {
	if knownKillKinds["expected_value"] {
		t.Fatal("expected_value must not be a known kill criterion kind")
	}
}
