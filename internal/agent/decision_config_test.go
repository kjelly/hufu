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

// DeclaredCapability (plan.md Stage 8; spec1.md §12.1) must fail closed on a
// malformed declaration at team-load time, before any routing decision ever
// consults it.
func TestDeclaredCapabilityValidate(t *testing.T) {
	cases := []struct {
		name    string
		decl    DeclaredCapability
		wantErr bool
	}{
		{name: "valid minimal", decl: DeclaredCapability{Capability: "security-review"}},
		{name: "valid with confidence", decl: DeclaredCapability{Capability: "security-review", Confidence: 0.5}},
		{name: "empty capability", decl: DeclaredCapability{}, wantErr: true},
		{name: "confidence too high", decl: DeclaredCapability{Capability: "x", Confidence: 1.5}, wantErr: true},
		{name: "confidence negative", decl: DeclaredCapability{Capability: "x", Confidence: -0.1}, wantErr: true},
		{
			name:    "stale-after without declared-at",
			decl:    DeclaredCapability{Capability: "x", StaleAfter: "24h"},
			wantErr: true,
		},
		{
			name:    "unparseable stale-after",
			decl:    DeclaredCapability{Capability: "x", DeclaredAt: "2026-01-01T00:00:00Z", StaleAfter: "not-a-duration"},
			wantErr: true,
		},
		{
			name:    "unparseable declared-at",
			decl:    DeclaredCapability{Capability: "x", DeclaredAt: "not-a-date"},
			wantErr: true,
		},
		{
			name: "valid with declared-at and stale-after",
			decl: DeclaredCapability{Capability: "x", DeclaredAt: "2026-01-01T00:00:00Z", StaleAfter: "720h"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.decl.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// outside-view.role (plan.md Stage 8 follow-up; spec.md v2 §15) must fail
// closed at load time when declared with nothing to route on, and must not
// affect an otherwise-valid policy when left unset.
func TestDecisionPolicyOutsideViewRoleValidate(t *testing.T) {
	policy := validPolicy()
	if err := policy.Validate(); err != nil {
		t.Fatalf("policy with no role configured should validate: %v", err)
	}

	policy.OutsideView.Role = &ReferenceRolePolicy{}
	if err := policy.Validate(); err == nil {
		t.Fatal("role with no required capabilities must fail validation")
	}

	policy.OutsideView.Role = &ReferenceRolePolicy{RequiredCapabilities: []string{"evidence-research"}}
	if err := policy.Validate(); err != nil {
		t.Fatalf("role with a required capability should validate: %v", err)
	}
}

// judge-role (spec2.md PR-3) must fail closed on the same "nothing to route
// on" mistake, and on a diversity floor the configured judge count could
// never satisfy.
func TestDecisionPolicyJudgeRoleValidate(t *testing.T) {
	policy := validPolicy()
	policy.IndependentJudgments = 3

	policy.JudgeRole = &JudgeRolePolicy{}
	if err := policy.Validate(); err == nil {
		t.Fatal("judge-role with no required capabilities must fail validation")
	}

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctAgents: 4}
	if err := policy.Validate(); err == nil {
		t.Fatal("min-distinct-agents exceeding independent-judgments must fail validation")
	}

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctAgents: 2}
	if err := policy.Validate(); err != nil {
		t.Fatalf("valid judge-role should validate: %v", err)
	}
}

// challenge-role (spec2.md PR-4) mirrors judge-role's validation rules,
// bounded against challenge.count instead of independent-judgments.
func TestDecisionPolicyChallengeRoleValidate(t *testing.T) {
	policy := validPolicy()
	policy.Challenge = ChallengePolicy{Enabled: true, Count: 2}

	policy.ChallengeRole = &ChallengeRolePolicy{}
	if err := policy.Validate(); err == nil {
		t.Fatal("challenge-role with no required capabilities must fail validation")
	}

	policy.ChallengeRole = &ChallengeRolePolicy{RequiredCapabilities: []string{"adversarial-analysis"}, MinDistinctAgents: 3}
	if err := policy.Validate(); err == nil {
		t.Fatal("min-distinct-agents exceeding challenge.count must fail validation")
	}

	policy.ChallengeRole = &ChallengeRolePolicy{RequiredCapabilities: []string{"adversarial-analysis"}, MinDistinctAgents: 2}
	if err := policy.Validate(); err != nil {
		t.Fatalf("valid challenge-role should validate: %v", err)
	}
}

// DiversityPolicy extensions (spec.md v2 §13): min-distinct-models/
// min-distinct-providers must fail closed the same way min-distinct-agents
// does, and allow-repeated-agent-definition must not coexist with a
// diversity floor above 1.
func TestDecisionPolicyJudgeRoleDiversityExtensionsValidate(t *testing.T) {
	policy := validPolicy()
	policy.IndependentJudgments = 3

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctModels: -1}
	if err := policy.Validate(); err == nil {
		t.Fatal("negative min-distinct-models must fail validation")
	}

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctModels: 4}
	if err := policy.Validate(); err == nil {
		t.Fatal("min-distinct-models exceeding independent-judgments must fail validation")
	}

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctProviders: 4}
	if err := policy.Validate(); err == nil {
		t.Fatal("min-distinct-providers exceeding independent-judgments must fail validation")
	}

	policy.JudgeRole = &JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"}, MinDistinctAgents: 2, AllowRepeatedAgentDefinition: true,
	}
	if err := policy.Validate(); err == nil {
		t.Fatal("allow-repeated-agent-definition conflicting with min-distinct-agents > 1 must fail validation")
	}

	policy.JudgeRole = &JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"}, MinDistinctModels: 2, PreferDistinctModels: true,
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("valid diversity extensions should validate: %v", err)
	}
}

// RoutingPin.Validate (spec.md v2 §34) requires both fields non-empty:
// an unaccountable pin (no reason) is exactly what the spec forbids.
func TestRoutingPinValidate(t *testing.T) {
	cases := []struct {
		name    string
		pin     RoutingPin
		wantErr bool
	}{
		{name: "valid", pin: RoutingPin{Agent: "security-reviewer", Reason: "required by security acceptance policy"}},
		{name: "empty agent", pin: RoutingPin{Reason: "required by policy"}, wantErr: true},
		{name: "empty reason", pin: RoutingPin{Agent: "security-reviewer"}, wantErr: true},
		{name: "both empty", pin: RoutingPin{}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.pin.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// A pin must fail team load unless its profile explicitly opts in via
// discipline.routing.allow-pinned-binding (spec.md v2 §34 "profile permits
// pin"), and must not coexist with a diversity floor above 1 (a pin forces
// every ordinal to the same agent).
func TestDecisionPolicyPinRequiresProfileGateAndNoDistinctFloor(t *testing.T) {
	policy := validPolicy()
	policy.IndependentJudgments = 3
	validPin := &RoutingPin{Agent: "security-reviewer", Reason: "required by security acceptance policy"}

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, Pin: validPin}
	if err := policy.Validate(); err == nil {
		t.Fatal("judge-role.pin without discipline.routing.allow-pinned-binding must fail validation")
	}

	policy.Discipline.Routing.AllowPinnedBinding = true
	if err := policy.Validate(); err != nil {
		t.Fatalf("judge-role.pin with the profile gate enabled should validate: %v", err)
	}

	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctAgents: 2, Pin: validPin}
	if err := policy.Validate(); err == nil {
		t.Fatal("judge-role.pin conflicting with min-distinct-agents > 1 must fail validation")
	}
	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctModels: 2, Pin: validPin}
	if err := policy.Validate(); err == nil {
		t.Fatal("judge-role.pin conflicting with min-distinct-models > 1 must fail validation")
	}
	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctProviders: 2, Pin: validPin}
	if err := policy.Validate(); err == nil {
		t.Fatal("judge-role.pin conflicting with min-distinct-providers > 1 must fail validation")
	}

	policy.Challenge = ChallengePolicy{Enabled: true, Count: 2}
	policy.ChallengeRole = &ChallengeRolePolicy{RequiredCapabilities: []string{"adversarial-analysis"}, MinDistinctModels: 2, Pin: validPin}
	if err := policy.Validate(); err == nil {
		t.Fatal("challenge-role.pin conflicting with min-distinct-models > 1 must fail validation")
	}
	policy.ChallengeRole = &ChallengeRolePolicy{RequiredCapabilities: []string{"adversarial-analysis"}, MinDistinctProviders: 2, Pin: validPin}
	if err := policy.Validate(); err == nil {
		t.Fatal("challenge-role.pin conflicting with min-distinct-providers > 1 must fail validation")
	}

	// An invalid pin (no reason) must fail even with the gate enabled.
	policy.JudgeRole = &JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, Pin: &RoutingPin{Agent: "security-reviewer"}}
	if err := policy.Validate(); err == nil {
		t.Fatal("judge-role.pin with no reason must fail validation even with the profile gate enabled")
	}
}

// adaptive-capabilities (spec.md v2 §39-40) must fail closed on a criterion
// ID that names no configured decision criterion, or an empty capability
// list — either mistake would otherwise silently never fire.
func TestDecisionPolicyChallengeRoleAdaptiveCapabilitiesValidate(t *testing.T) {
	policy := validPolicy()
	policy.Challenge = ChallengePolicy{Enabled: true, Count: 1}
	policy.Criteria = []DecisionCriterion{{ID: "operability", Weight: 1}}

	policy.ChallengeRole = &ChallengeRolePolicy{
		RequiredCapabilities: []string{"adversarial-analysis"},
		AdaptiveCapabilities: map[string][]string{"data-integrity": {"database"}},
	}
	if err := policy.Validate(); err == nil {
		t.Fatal("adaptive-capabilities referencing an unconfigured criterion must fail validation")
	}

	policy.ChallengeRole = &ChallengeRolePolicy{
		RequiredCapabilities: []string{"adversarial-analysis"},
		AdaptiveCapabilities: map[string][]string{"operability": {}},
	}
	if err := policy.Validate(); err == nil {
		t.Fatal("adaptive-capabilities with an empty capability list must fail validation")
	}

	policy.ChallengeRole = &ChallengeRolePolicy{
		RequiredCapabilities: []string{"adversarial-analysis"},
		AdaptiveCapabilities: map[string][]string{"operability": {"operations", "reliability"}},
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("valid adaptive-capabilities should validate: %v", err)
	}
}

// routing-hints (spec.md v2 §16) must fail closed on a hint that could never
// match anything or never add anything.
func TestRoutingHintValidate(t *testing.T) {
	cases := []struct {
		name    string
		hint    RoutingHint
		wantErr bool
	}{
		{name: "valid", hint: RoutingHint{WhenGoalContains: "kubernetes", PreferredCapabilities: []string{"kubernetes"}}},
		{name: "empty selector", hint: RoutingHint{PreferredCapabilities: []string{"kubernetes"}}, wantErr: true},
		{name: "no preferred capabilities", hint: RoutingHint{WhenGoalContains: "kubernetes"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.hint.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("want error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("want no error, got %v", err)
			}
		})
	}
}

// A malformed routing hint must fail team-level DecisionConfig.Validate,
// not silently no-op forever at dispatch time.
func TestDecisionConfigRejectsInvalidRoutingHint(t *testing.T) {
	cfg := DecisionConfig{RoutingHints: []RoutingHint{{WhenGoalContains: "kubernetes"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("want error for a routing hint with no preferred capabilities")
	}
}
