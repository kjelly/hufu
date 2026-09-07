package agent

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Decision configuration surface for the decision-aware runtime. See
// docs/hufu-decision-aware-runtime-spec.md §10-§13.
//
// These types live in the agent package because that is where TeamConfig and
// every other declarative team-configuration type already lives
// (WorkflowConfig, RetryConfig, CapabilityConfig). The runtime-side decision
// types (DecisionRecord, DecisionOpinion, ...) live in internal/team, which
// imports this package; the reverse import would be a cycle.

// DecisionProfileOff is the reserved profile name meaning "do not run
// structured multi-agent decision formation". It never needs to be declared in
// the profiles map, and it never disables runtime correctness or safety
// (spec §8).
const DecisionProfileOff = "off"

// Context isolation levels (spec §16).
const (
	DecisionIsolationStrict = "strict"
	DecisionIsolationSealed = "sealed"
)

// DecisionScoreScale is the only score scale supported in V1 (spec §14.1).
const DecisionScoreScale = "0-10"

// Aggregation methods (spec §21).
const (
	AggregationMeanScore       = "mean-score"
	AggregationMedianScore     = "median-score"
	AggregationMeanProbability = "mean-probability"
	AggregationMajority        = "majority"
)

// Finalization modes (spec §26).
const (
	FinalizationAggregate   = "aggregate"
	FinalizationCoordinator = "coordinator"
	FinalizationJudge       = "judge"
)

// Budget degradation modes (spec §34).
const (
	BudgetDegradationForbidden = "forbidden"
	BudgetDegradationExplicit  = "explicit"
)

// Criterion directions (spec §14.1).
const (
	CriterionHigherIsBetter = "higher-is-better"
	CriterionLowerIsBetter  = "lower-is-better"
)

// Replan actions (spec §31).
const (
	ReplanContinue           = "continue"
	ReplanReplan             = "replan"
	ReplanStop               = "stop"
	ReplanRequestInformation = "request_information"
	ReplanEscalate           = "escalate"
	ReplanNeedsHuman         = "needs_human"
)

// Kill criterion kinds. Only computable kinds are accepted; the draft's
// expected_value kind is deliberately absent (spec §29.2).
const (
	KillKindBudgetTokens      = "budget_tokens"
	KillKindBudgetDuration    = "budget_duration"
	KillKindToolCalls         = "tool_calls"
	KillKindAttempts          = "attempts"
	KillKindRepeatedFailure   = "repeated_failure"
	KillKindNoProgress        = "no_progress"
	KillKindAssumptionInvalid = "assumption_invalid"
)

// maxChallengeCount bounds challenge fan-out per decision (spec §12).
const maxChallengeCount = 3

// maxDecisionRounds is the V1 hard limit: one independent round plus at most
// one independent revision round (spec §25).
const maxDecisionRounds = 2

// DecisionConfig is the team-level decision configuration (spec §10).
type DecisionConfig struct {
	DefaultProfile  string                    `yaml:"default-profile,omitempty"`
	Profiles        map[string]DecisionPolicy `yaml:"profiles,omitempty"`
	RequestContract RequestContractConfig     `yaml:"request-contract,omitempty"`
	// RoutingHints widen a role's preferred-capability list for a specific
	// decision, based on the task's own question text (spec.md v2 §16,
	// §30-31). Team-wide, not per-profile: every profile that opts a role
	// into capability routing sees the same hints. A hint can only ever add
	// to a role's *preferred* list — never to required — so it can widen
	// which already-qualified candidate wins, never grant eligibility to one
	// that failed the required-capability check.
	RoutingHints []RoutingHint `yaml:"routing-hints,omitempty"`
}

// RoutingHint is one goal-substring-triggered capability-routing rule
// (spec.md v2 §16). It reuses the same selector shape TaskGoalInvariants
// already uses, rather than a new model-facing schema field: the task's
// Goal is already free text a coordinator writes for its own reasons, and a
// team-declared, auditable rule set matches spec.md v2's own examples
// (its "Kubernetes migration" / "Security-sensitive task" hints are
// themselves static, scenario-keyed rules, not freeform LLM-authored tags).
type RoutingHint struct {
	WhenGoalContains      string   `yaml:"when-goal-contains"`
	PreferredCapabilities []string `yaml:"preferred-capabilities"`
}

// Validate rejects a hint that could never match anything or never add
// anything — either mistake would silently no-op forever rather than fail
// at load time.
func (h RoutingHint) Validate() error {
	if strings.TrimSpace(h.WhenGoalContains) == "" {
		return fmt.Errorf("routing-hints[].when-goal-contains must not be empty")
	}
	if len(h.PreferredCapabilities) == 0 {
		return fmt.Errorf("routing-hints[].preferred-capabilities must name at least one capability")
	}
	return nil
}

// RequestContractConfig contains explicit request-level intent. It is kept
// separate from DecisionCriterion because scoring weights are not acceptance
// criteria (spec §11).
type RequestContractConfig struct {
	Enabled         bool                        `yaml:"enabled,omitempty"`
	Objective       string                      `yaml:"objective,omitempty"`
	SuccessCriteria []RequestSuccessCriterion   `yaml:"success-criteria,omitempty"`
	Constraints     []RequestConstraint         `yaml:"constraints,omitempty"`
	Assumptions     []RequestContractAssumption `yaml:"assumptions,omitempty"`
}

type RequestSuccessCriterion struct {
	ID        string `yaml:"id"`
	Statement string `yaml:"statement"`
}

type RequestConstraint struct {
	ID        string `yaml:"id"`
	Statement string `yaml:"statement"`
}

type RequestContractAssumption struct {
	ID        string `yaml:"id"`
	Statement string `yaml:"statement"`
	Critical  bool   `yaml:"critical,omitempty"`
}

// DecisionPolicy is one named rigor profile (spec §12).
type DecisionPolicy struct {
	IndependentJudgments    int    `yaml:"independent-judgments,omitempty"`
	MinIndependentJudgments int    `yaml:"min-independent-judgments,omitempty"`
	ContextIsolation        string `yaml:"context-isolation,omitempty"`
	ScoreScale              string `yaml:"score-scale,omitempty"`

	OutsideView OutsideViewPolicy   `yaml:"outside-view,omitempty"`
	Criteria    []DecisionCriterion `yaml:"criteria,omitempty"`

	// JudgeRole opts the JUDGE stage into real capability-routed execution
	// (spec.md v2 §17; spec2.md PR-3). A nil JudgeRole preserves the
	// sidecar-only behavior exactly as before this field existed.
	JudgeRole *JudgeRolePolicy `yaml:"judge-role,omitempty"`

	Aggregation AggregationPolicy `yaml:"aggregation,omitempty"`
	Challenge   ChallengePolicy   `yaml:"challenge,omitempty"`
	// ChallengeRole opts the CHALLENGE stage into real capability-routed
	// execution (spec.md v2 §19; spec2.md PR-4). There is no separate
	// revision-role: REVISE reuses JudgeRole directly.
	ChallengeRole *ChallengeRolePolicy `yaml:"challenge-role,omitempty"`
	Revision      RevisionPolicy       `yaml:"revision,omitempty"`
	Premortem     PremortemPolicy      `yaml:"premortem,omitempty"`
	Forecast      ForecastPolicy       `yaml:"forecast,omitempty"`
	Finalization  FinalizationPolicy   `yaml:"finalization,omitempty"`

	OptionProposal OptionProposalPolicy `yaml:"option-proposal,omitempty"`

	Discipline DisciplinePolicy `yaml:"discipline,omitempty"`

	MaxRounds         int    `yaml:"max-rounds,omitempty"`
	MaxTokens         int64  `yaml:"max-tokens,omitempty"`
	BudgetDegradation string `yaml:"budget-degradation,omitempty"`
}

// DecisionCriterion is one weighted scoring dimension (spec §12).
type DecisionCriterion struct {
	ID        string  `yaml:"id" json:"id"`
	Statement string  `yaml:"statement,omitempty" json:"statement,omitempty"`
	Weight    float64 `yaml:"weight" json:"weight"`
	Direction string  `yaml:"direction,omitempty" json:"direction,omitempty"`
}

// OutsideViewPolicy gates JUDGE on base-rate evidence (spec §17).
type OutsideViewPolicy struct {
	Required          bool `yaml:"required,omitempty"`
	ReferenceEvidence bool `yaml:"reference-evidence,omitempty"`
	// Role opts the reference stage into real capability-routed execution
	// (spec.md v2 §15; plan.md Stage 8 follow-up): instead of the team's
	// judge-model sidecar, the runtime resolves an authorized,
	// capability-matched concrete worker and invokes it directly, with its
	// tools narrowed to read-only. A nil Role preserves the sidecar-only
	// behavior exactly as before this field existed.
	Role *ReferenceRolePolicy `yaml:"role,omitempty"`
}

// RoutingPin forces a role to a single named agent instead of ranking
// candidates (spec.md v2 §34). It never bypasses authorization or required-
// capability checks — it only replaces *which* already-qualified candidate
// is chosen. A profile must separately opt in via
// DisciplinePolicy.Routing.AllowPinnedBinding before a pin is honored.
type RoutingPin struct {
	Agent  string `yaml:"agent"`
	Reason string `yaml:"reason"`
}

// Validate requires both fields: an empty agent could never resolve to
// anything, and a pin without a stated reason is exactly the silent,
// unaccountable override spec.md v2 §34 requires runtimes to reject.
func (p RoutingPin) Validate() error {
	if strings.TrimSpace(p.Agent) == "" {
		return fmt.Errorf("pin.agent must not be empty")
	}
	if strings.TrimSpace(p.Reason) == "" {
		return fmt.Errorf("pin.reason must not be empty")
	}
	return nil
}

// ReferenceRolePolicy names the capabilities the reference role's resolved
// worker must (and should) show. It is the minimal slice of spec.md v2's
// RoleSpec this runtime implements — full diversity and provenance tiers
// beyond declared/maintainer-declared remain future work.
type ReferenceRolePolicy struct {
	RequiredCapabilities  []string    `yaml:"required-capabilities,omitempty"`
	PreferredCapabilities []string    `yaml:"preferred-capabilities,omitempty"`
	Pin                   *RoutingPin `yaml:"pin,omitempty"`
}

// Validate rejects a role declared with nothing to route on: without at
// least one required capability, routing could never disqualify any
// candidate, which is indistinguishable from not configuring it at all,
// except silently.
func (p ReferenceRolePolicy) Validate() error {
	if len(p.RequiredCapabilities) == 0 {
		return fmt.Errorf("outside-view.role.required-capabilities must name at least one capability")
	}
	if p.Pin != nil {
		if err := p.Pin.Validate(); err != nil {
			return fmt.Errorf("pin: %w", err)
		}
	}
	return nil
}

// JudgeRolePolicy names the capabilities the JUDGE stage's resolved workers
// must (and should) show, and the minimal diversity floor across them
// (spec.md v2 §17-§18; spec2.md PR-3). MinDistinctAgents is deliberately not
// the full DiversityPolicy spec.md v2 describes — only the floor needed to
// prove "candidates too few must fail closed, not silently repeat one agent".
type JudgeRolePolicy struct {
	RequiredCapabilities  []string    `yaml:"required-capabilities,omitempty"`
	PreferredCapabilities []string    `yaml:"preferred-capabilities,omitempty"`
	MinDistinctAgents     int         `yaml:"min-distinct-agents,omitempty"`
	Pin                   *RoutingPin `yaml:"pin,omitempty"`
	// MinDistinctModels/MinDistinctProviders extend MinDistinctAgents with
	// two more diversity floors from spec.md v2 §13's DiversityPolicy
	// (capability-group distinctness remains out of scope — no such
	// taxonomy exists in this codebase). PreferDistinctModels/
	// PreferDistinctProviders are soft preferences: when set, ranking
	// prefers introducing a new model/provider value across a role's
	// ordinals over repeating one already used, without ever excluding a
	// qualified candidate outright. AllowRepeatedAgentDefinition opts a
	// profile into the light-profile fallback spec.md v2 §33.1 describes
	// (same concrete agent × N sealed invocations) — it is contradictory
	// with, and rejected alongside, MinDistinctAgents > 1.
	MinDistinctModels            int  `yaml:"min-distinct-models,omitempty"`
	MinDistinctProviders         int  `yaml:"min-distinct-providers,omitempty"`
	PreferDistinctModels         bool `yaml:"prefer-distinct-models,omitempty"`
	PreferDistinctProviders      bool `yaml:"prefer-distinct-providers,omitempty"`
	AllowRepeatedAgentDefinition bool `yaml:"allow-repeated-agent-definition,omitempty"`
}

// Validate enforces the same "must have something to route on" rule
// ReferenceRolePolicy does, plus a config-time sanity bound on
// MinDistinctAgents: independentJudgments is the caller's
// DecisionPolicy.IndependentJudgments, since requiring more distinct agents
// than judges dispatched could never be satisfied. A pin forces every judge
// ordinal to the same agent, so it is definitionally incompatible with a
// diversity floor above 1.
func (p JudgeRolePolicy) Validate(independentJudgments int) error {
	if len(p.RequiredCapabilities) == 0 {
		return fmt.Errorf("judge-role.required-capabilities must name at least one capability")
	}
	if p.MinDistinctAgents < 0 {
		return fmt.Errorf("judge-role.min-distinct-agents must not be negative, got %d", p.MinDistinctAgents)
	}
	if p.MinDistinctAgents > independentJudgments {
		return fmt.Errorf("judge-role.min-distinct-agents (%d) must not exceed independent-judgments (%d)", p.MinDistinctAgents, independentJudgments)
	}
	if err := validateDiversityFloors("judge-role", p.MinDistinctModels, p.MinDistinctProviders, independentJudgments); err != nil {
		return err
	}
	if p.AllowRepeatedAgentDefinition && p.EffectiveMinDistinctAgents() > 1 {
		return fmt.Errorf("judge-role.allow-repeated-agent-definition conflicts with judge-role.min-distinct-agents > 1")
	}
	if p.Pin != nil {
		if err := p.Pin.Validate(); err != nil {
			return fmt.Errorf("pin: %w", err)
		}
		if p.EffectiveMinDistinctAgents() > 1 {
			return fmt.Errorf("judge-role.pin conflicts with judge-role.min-distinct-agents > 1: a pin forces every judge to the same agent")
		}
		if p.MinDistinctModels > 1 {
			return fmt.Errorf("judge-role.pin conflicts with judge-role.min-distinct-models > 1: a pin forces every judge to the same model")
		}
		if p.MinDistinctProviders > 1 {
			return fmt.Errorf("judge-role.pin conflicts with judge-role.min-distinct-providers > 1: a pin forces every judge to the same provider")
		}
	}
	return nil
}

// EffectiveMinDistinctAgents defaults to 1 (no diversity requirement beyond
// "at least one authorized candidate exists").
func (p JudgeRolePolicy) EffectiveMinDistinctAgents() int {
	if p.MinDistinctAgents <= 0 {
		return 1
	}
	return p.MinDistinctAgents
}

// validateDiversityFloors bounds a role's MinDistinctModels/
// MinDistinctProviders the same way MinDistinctAgents is bounded: neither
// may be negative or exceed the role's dispatch count, since requiring more
// distinct values than dispatches could never be satisfied.
func validateDiversityFloors(rolePrefix string, minDistinctModels, minDistinctProviders, count int) error {
	if minDistinctModels < 0 {
		return fmt.Errorf("%s.min-distinct-models must not be negative, got %d", rolePrefix, minDistinctModels)
	}
	if minDistinctModels > count {
		return fmt.Errorf("%s.min-distinct-models (%d) must not exceed the configured count (%d)", rolePrefix, minDistinctModels, count)
	}
	if minDistinctProviders < 0 {
		return fmt.Errorf("%s.min-distinct-providers must not be negative, got %d", rolePrefix, minDistinctProviders)
	}
	if minDistinctProviders > count {
		return fmt.Errorf("%s.min-distinct-providers (%d) must not exceed the configured count (%d)", rolePrefix, minDistinctProviders, count)
	}
	return nil
}

// ChallengeRolePolicy names the capabilities the CHALLENGE stage's resolved
// workers must (and should) show (spec.md v2 §19; spec2.md PR-4). There is
// no separate revision-role policy: REVISE reuses JudgeRolePolicy directly
// ("REVISE = reuse JUDGE bindings").
type ChallengeRolePolicy struct {
	RequiredCapabilities  []string    `yaml:"required-capabilities,omitempty"`
	PreferredCapabilities []string    `yaml:"preferred-capabilities,omitempty"`
	MinDistinctAgents     int         `yaml:"min-distinct-agents,omitempty"`
	Pin                   *RoutingPin `yaml:"pin,omitempty"`
	// MinDistinctModels/MinDistinctProviders/PreferDistinctModels/
	// PreferDistinctProviders/AllowRepeatedAgentDefinition mirror
	// JudgeRolePolicy's DiversityPolicy extension (spec.md v2 §13),
	// bounded against the configured challenge count instead of judge
	// count.
	MinDistinctModels            int  `yaml:"min-distinct-models,omitempty"`
	MinDistinctProviders         int  `yaml:"min-distinct-providers,omitempty"`
	PreferDistinctModels         bool `yaml:"prefer-distinct-models,omitempty"`
	PreferDistinctProviders      bool `yaml:"prefer-distinct-providers,omitempty"`
	AllowRepeatedAgentDefinition bool `yaml:"allow-repeated-agent-definition,omitempty"`
	// AdaptiveCapabilities maps a configured decision criterion ID to extra
	// preferred capabilities for CHALLENGE routing (spec.md v2 §39-40),
	// applied when that criterion shows the most disagreement across JUDGE
	// round 1's opinions. Deterministic and policy-defined: the runtime
	// never infers "who should oppose the winner", only which capability
	// domain the aggregate's own per-criterion dispersion points at.
	AdaptiveCapabilities map[string][]string `yaml:"adaptive-capabilities,omitempty"`
}

// Validate mirrors JudgeRolePolicy.Validate, bounding MinDistinctAgents
// against the configured challenge count instead of judge count. criteria is
// the decision's own DecisionPolicy.Criteria, used to fail closed on an
// AdaptiveCapabilities key that names no configured criterion (a typo would
// otherwise silently never fire).
func (p ChallengeRolePolicy) Validate(challengeCount int, criteria []DecisionCriterion) error {
	if len(p.RequiredCapabilities) == 0 {
		return fmt.Errorf("challenge-role.required-capabilities must name at least one capability")
	}
	if p.MinDistinctAgents < 0 {
		return fmt.Errorf("challenge-role.min-distinct-agents must not be negative, got %d", p.MinDistinctAgents)
	}
	if p.MinDistinctAgents > challengeCount {
		return fmt.Errorf("challenge-role.min-distinct-agents (%d) must not exceed challenge.count (%d)", p.MinDistinctAgents, challengeCount)
	}
	if err := validateDiversityFloors("challenge-role", p.MinDistinctModels, p.MinDistinctProviders, challengeCount); err != nil {
		return err
	}
	if p.AllowRepeatedAgentDefinition && p.EffectiveMinDistinctAgents() > 1 {
		return fmt.Errorf("challenge-role.allow-repeated-agent-definition conflicts with challenge-role.min-distinct-agents > 1")
	}
	if p.Pin != nil {
		if err := p.Pin.Validate(); err != nil {
			return fmt.Errorf("pin: %w", err)
		}
		if p.EffectiveMinDistinctAgents() > 1 {
			return fmt.Errorf("challenge-role.pin conflicts with challenge-role.min-distinct-agents > 1: a pin forces every challenger to the same agent")
		}
		if p.MinDistinctModels > 1 {
			return fmt.Errorf("challenge-role.pin conflicts with challenge-role.min-distinct-models > 1: a pin forces every challenger to the same model")
		}
		if p.MinDistinctProviders > 1 {
			return fmt.Errorf("challenge-role.pin conflicts with challenge-role.min-distinct-providers > 1: a pin forces every challenger to the same provider")
		}
	}
	if len(p.AdaptiveCapabilities) > 0 {
		known := make(map[string]bool, len(criteria))
		for _, c := range criteria {
			known[strings.TrimSpace(c.ID)] = true
		}
		ids := make([]string, 0, len(p.AdaptiveCapabilities))
		for id := range p.AdaptiveCapabilities {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if !known[id] {
				return fmt.Errorf("challenge-role.adaptive-capabilities references unknown criterion %q", id)
			}
			if len(p.AdaptiveCapabilities[id]) == 0 {
				return fmt.Errorf("challenge-role.adaptive-capabilities[%q] must name at least one capability", id)
			}
		}
	}
	return nil
}

// EffectiveMinDistinctAgents defaults to 1, mirroring JudgeRolePolicy.
func (p ChallengeRolePolicy) EffectiveMinDistinctAgents() int {
	if p.MinDistinctAgents <= 0 {
		return 1
	}
	return p.MinDistinctAgents
}

// AggregationPolicy selects the deterministic aggregator (spec §21).
type AggregationPolicy struct {
	Method string `yaml:"method,omitempty"`
}

// ChallengePolicy configures the post-aggregate challenge stage (spec §23).
type ChallengePolicy struct {
	Enabled bool              `yaml:"enabled,omitempty"`
	Count   int               `yaml:"count,omitempty"`
	Trigger *ChallengeTrigger `yaml:"trigger,omitempty"`
}

// ChallengeTrigger makes challenge conditional on score dispersion. It is a
// pointer so "no trigger configured" (challenge always runs) is distinguishable
// from "dispersion-above: 0" (challenge always triggers) (spec §12.1).
type ChallengeTrigger struct {
	// DispersionAbove is expressed in score points on the 0-10 scale (§14.5).
	DispersionAbove float64 `yaml:"dispersion-above,omitempty"`
}

// RevisionPolicy enables the single bounded revision round (spec §25).
type RevisionPolicy struct {
	Enabled bool `yaml:"enabled,omitempty"`
}

// PremortemPolicy configures risk discovery (spec §24).
type PremortemPolicy struct {
	Enabled              bool `yaml:"enabled,omitempty"`
	RequiredBeforeCommit bool `yaml:"required-before-commit,omitempty"`
}

// ForecastPolicy requires a resolvable probability on the final record (§40).
type ForecastPolicy struct {
	Required bool `yaml:"required,omitempty"`
}

// OptionProposalPolicy lets the runtime propose alternatives when a task
// declares none, so a decision task does not have to hand-write its options
// (spec §19.1). The gate it must not weaken is the no-go requirement; that is
// preserved by runtime injection plus per-option provenance, not by trusting
// the proposer.
type OptionProposalPolicy struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// MaxOptions caps the option set including any the runtime injects.
	// Zero uses defaultMaxProposedOptions.
	MaxOptions int `yaml:"max-options,omitempty"`
}

// defaultMaxProposedOptions bounds a proposed option set. More options means
// every judge scores every one of them, so the cost is multiplicative.
const defaultMaxProposedOptions = 5

// EffectiveMaxOptions returns the option cap, defaulted.
func (p OptionProposalPolicy) EffectiveMaxOptions() int {
	if p.MaxOptions <= 0 {
		return defaultMaxProposedOptions
	}
	return p.MaxOptions
}

// FinalizationPolicy selects who picks the final option (spec §26).
type FinalizationPolicy struct {
	Mode    string `yaml:"mode,omitempty"`
	JudgeID string `yaml:"judge-id,omitempty"`
}

// DisciplinePolicy groups the commit/stop/replan boundaries so the runtime does
// not scatter boolean flags across unrelated components (spec §13).
type DisciplinePolicy struct {
	Alternatives AlternativesPolicy         `yaml:"alternatives,omitempty"`
	Stop         StopPolicy                 `yaml:"stop,omitempty"`
	Commit       CommitGatePolicy           `yaml:"commit,omitempty"`
	Replan       ReplanPolicy               `yaml:"replan,omitempty"`
	Evidence     EvidenceIndependencePolicy `yaml:"evidence,omitempty"`
	Routing      RoutingPolicy              `yaml:"routing,omitempty"`
}

// RoutingPolicy opts a decision profile into capability-aware worker routing
// (plan.md Stage 8; spec1.md §12). It only ever narrows an already-authorized
// candidate set by declared capability match — it can never grant
// authorization to a worker delegation.allowed-workers would otherwise
// reject (internal/team's CapabilityRegistry.Resolve enforces this
// structurally: it only ever scores the caller-supplied eligible set).
type RoutingPolicy struct {
	CapabilityAware bool `yaml:"capability-aware,omitempty"`
	// AllowPinnedBinding must be true before any role's `pin` config is
	// honored for this profile (spec.md v2 §34's "profile permits pin").
	// Left false, a role's pin fails team load rather than silently
	// resolving through it or silently ignoring it.
	AllowPinnedBinding bool `yaml:"allow-pinned-binding,omitempty"`
}

// AlternativesPolicy enforces that no-go and information options exist before
// judgment starts (spec §19).
type AlternativesPolicy struct {
	RequireNoActionOption bool `yaml:"require-no-action-option,omitempty"`
	RequireInfoOption     bool `yaml:"require-information-option,omitempty"`
	MinOptions            int  `yaml:"min-options,omitempty"`
}

// StopPolicy interprets whether execution may continue. It never owns
// counters; BudgetManager does (spec §29).
type StopPolicy struct {
	MaxAttempts  int    `yaml:"max-attempts,omitempty" json:"max_attempts,omitempty"`
	MaxToolCalls int    `yaml:"max-tool-calls,omitempty" json:"max_tool_calls,omitempty"`
	MaxTokens    int64  `yaml:"max-tokens,omitempty" json:"max_tokens,omitempty"`
	MaxDuration  string `yaml:"max-duration,omitempty" json:"max_duration,omitempty"`

	CheckpointEvery     int  `yaml:"checkpoint-every,omitempty" json:"checkpoint_every,omitempty"`
	RequireKillCriteria bool `yaml:"require-kill-criteria,omitempty" json:"require_kill_criteria,omitempty"`

	KillCriteria []KillCriterion `yaml:"kill-criteria,omitempty" json:"kill_criteria,omitempty"`
}

// Duration parses MaxDuration. An empty value means "no duration bound".
func (s StopPolicy) Duration() (time.Duration, error) {
	if strings.TrimSpace(s.MaxDuration) == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(s.MaxDuration))
	if err != nil {
		return 0, fmt.Errorf("parsing stop.max-duration %q: %w", s.MaxDuration, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("stop.max-duration must not be negative: %q", s.MaxDuration)
	}
	return d, nil
}

// KillCriterion is one predeclared stop condition (spec §29.2).
type KillCriterion struct {
	ID          string  `yaml:"id" json:"id"`
	Kind        string  `yaml:"kind" json:"kind"`
	Threshold   float64 `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	Description string  `yaml:"description,omitempty" json:"description,omitempty"`
}

// CommitGatePolicy states the prerequisites a side-effecting task must satisfy
// before any tool process may start (spec §30).
type CommitGatePolicy struct {
	// RequiredForSideEffects holds side-effect class names. It is []string
	// rather than a typed enum because the canonical SideEffectClass type
	// lives in internal/team, which imports this package.
	RequiredForSideEffects []string `yaml:"required-for-side-effects,omitempty"`

	RequireRollback      bool `yaml:"require-rollback,omitempty"`
	RequireReconcile     bool `yaml:"require-reconcile,omitempty"`
	RequireObservability bool `yaml:"require-observability,omitempty"`
	RequireVerification  bool `yaml:"require-verification,omitempty"`
	RequireEvidence      bool `yaml:"require-evidence,omitempty"`
}

// ReplanPolicy maps invalidation events to configured actions (spec §31).
type ReplanPolicy struct {
	OnCriticalAssumptionContradicted string `yaml:"on-critical-assumption-contradicted,omitempty"`
	OnMaterialEvidenceChanged        string `yaml:"on-material-evidence-changed,omitempty"`
	OnRepeatedFailure                string `yaml:"on-repeated-failure,omitempty"`
}

// EvidenceIndependencePolicy configures source-independence handling. In V1 it
// is advisory: grouping only uses runtime-derivable signals (spec §28).
type EvidenceIndependencePolicy struct {
	RequiredIndependentGroups int  `yaml:"required-independent-groups,omitempty"`
	RejectCircularCitation    bool `yaml:"reject-circular-citation,omitempty"`
	WarnSharedOrigin          bool `yaml:"warn-shared-origin,omitempty"`
}

// knownSideEffectClasses mirrors internal/team's SideEffectClass values. It is
// duplicated here (as strings) only to validate configuration; the runtime
// keeps using the typed constants.
var knownSideEffectClasses = map[string]bool{
	"none":                true,
	"workspace_write":     true,
	"external_write":      true,
	"infra_mutation":      true,
	"credential_mutation": true,
	"unknown":             true,
}

var knownReplanActions = map[string]bool{
	ReplanContinue:           true,
	ReplanReplan:             true,
	ReplanStop:               true,
	ReplanRequestInformation: true,
	ReplanEscalate:           true,
	ReplanNeedsHuman:         true,
}

var knownKillKinds = map[string]bool{
	KillKindBudgetTokens:      true,
	KillKindBudgetDuration:    true,
	KillKindToolCalls:         true,
	KillKindAttempts:          true,
	KillKindRepeatedFailure:   true,
	KillKindNoProgress:        true,
	KillKindAssumptionInvalid: true,
}

// Validate checks the whole decision configuration. Every rule here is a config
// load-time failure, never a silent default (spec §12).
func (c DecisionConfig) Validate() error {
	if err := c.RequestContract.Validate(); err != nil {
		return fmt.Errorf("decision.request-contract: %w", err)
	}
	for i, hint := range c.RoutingHints {
		if err := hint.Validate(); err != nil {
			return fmt.Errorf("decision.routing-hints[%d]: %w", i, err)
		}
	}
	if c.DefaultProfile != "" && c.DefaultProfile != DecisionProfileOff {
		if _, ok := c.Profiles[c.DefaultProfile]; !ok {
			return fmt.Errorf("%s: decision.default-profile %q is not defined", ReasonDecisionProfileUnknown, c.DefaultProfile)
		}
	}
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == DecisionProfileOff {
			return fmt.Errorf("decision.profiles: %q is a reserved profile name and must not be declared", DecisionProfileOff)
		}
		if err := c.Profiles[name].Validate(); err != nil {
			return fmt.Errorf("decision.profiles.%s: %w", name, err)
		}
	}
	return nil
}

func (c RequestContractConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Objective) == "" {
		return fmt.Errorf("enabled request contract requires an objective")
	}
	if len(c.SuccessCriteria) == 0 {
		return fmt.Errorf("enabled request contract requires success criteria")
	}
	seen := make(map[string]struct{}, len(c.SuccessCriteria))
	for _, criterion := range c.SuccessCriteria {
		if strings.TrimSpace(criterion.ID) == "" || strings.TrimSpace(criterion.Statement) == "" {
			return fmt.Errorf("success criteria require non-empty id and statement")
		}
		if _, exists := seen[criterion.ID]; exists {
			return fmt.Errorf("success criterion %q is duplicated", criterion.ID)
		}
		seen[criterion.ID] = struct{}{}
	}
	seen = make(map[string]struct{}, len(c.Constraints))
	for _, constraint := range c.Constraints {
		if strings.TrimSpace(constraint.ID) == "" || strings.TrimSpace(constraint.Statement) == "" {
			return fmt.Errorf("constraints require non-empty id and statement")
		}
		if _, exists := seen[constraint.ID]; exists {
			return fmt.Errorf("constraint %q is duplicated", constraint.ID)
		}
		seen[constraint.ID] = struct{}{}
	}
	seen = make(map[string]struct{}, len(c.Assumptions))
	for _, assumption := range c.Assumptions {
		if strings.TrimSpace(assumption.ID) == "" || strings.TrimSpace(assumption.Statement) == "" {
			return fmt.Errorf("assumptions require non-empty id and statement")
		}
		if _, exists := seen[assumption.ID]; exists {
			return fmt.Errorf("assumption %q is duplicated", assumption.ID)
		}
		seen[assumption.ID] = struct{}{}
	}
	return nil
}

// HasProfile reports whether name resolves to a usable profile. The reserved
// "off" name always resolves.
func (c DecisionConfig) HasProfile(name string) bool {
	if name == "" || name == DecisionProfileOff {
		return true
	}
	_, ok := c.Profiles[name]
	return ok
}

// Validate enforces the per-profile rules from spec §12.
func (p DecisionPolicy) Validate() error {
	if p.IndependentJudgments < 1 {
		return fmt.Errorf("independent-judgments must be >= 1, got %d", p.IndependentJudgments)
	}
	minJudges := p.EffectiveMinJudgments()
	if minJudges < 1 {
		return fmt.Errorf("min-independent-judgments must be >= 1, got %d", p.MinIndependentJudgments)
	}
	if minJudges > p.IndependentJudgments {
		return fmt.Errorf("min-independent-judgments (%d) must not exceed independent-judgments (%d)", minJudges, p.IndependentJudgments)
	}
	if p.MaxRounds != 0 && (p.MaxRounds < 1 || p.MaxRounds > maxDecisionRounds) {
		return fmt.Errorf("max-rounds must be between 1 and %d, got %d", maxDecisionRounds, p.MaxRounds)
	}
	if p.OutsideView.Role != nil {
		if err := p.OutsideView.Role.Validate(); err != nil {
			return fmt.Errorf("outside-view.role: %w", err)
		}
		if err := requirePinPermitted(p.OutsideView.Role.Pin, p.Discipline.Routing, "outside-view.role"); err != nil {
			return err
		}
	}
	if p.JudgeRole != nil {
		if err := p.JudgeRole.Validate(p.IndependentJudgments); err != nil {
			return fmt.Errorf("judge-role: %w", err)
		}
		if err := requirePinPermitted(p.JudgeRole.Pin, p.Discipline.Routing, "judge-role"); err != nil {
			return err
		}
	}
	if p.ChallengeRole != nil {
		if err := p.ChallengeRole.Validate(p.Challenge.Count, p.Criteria); err != nil {
			return fmt.Errorf("challenge-role: %w", err)
		}
		if err := requirePinPermitted(p.ChallengeRole.Pin, p.Discipline.Routing, "challenge-role"); err != nil {
			return err
		}
	}
	switch p.ContextIsolation {
	case "", DecisionIsolationStrict, DecisionIsolationSealed:
	default:
		return fmt.Errorf("context-isolation must be %q or %q, got %q", DecisionIsolationStrict, DecisionIsolationSealed, p.ContextIsolation)
	}
	if p.ScoreScale != "" && p.ScoreScale != DecisionScoreScale {
		return fmt.Errorf("score-scale must be %q in V1, got %q", DecisionScoreScale, p.ScoreScale)
	}
	switch p.Aggregation.Method {
	case "", AggregationMeanScore, AggregationMedianScore, AggregationMeanProbability, AggregationMajority:
	default:
		return fmt.Errorf("aggregation.method %q is not supported", p.Aggregation.Method)
	}
	if p.Challenge.Count < 0 || p.Challenge.Count > maxChallengeCount {
		return fmt.Errorf("challenge.count must be between 0 and %d, got %d", maxChallengeCount, p.Challenge.Count)
	}
	if p.Challenge.Trigger != nil {
		d := p.Challenge.Trigger.DispersionAbove
		if math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
			return fmt.Errorf("challenge.trigger.dispersion-above must be a finite value >= 0, got %v", d)
		}
	}
	if err := validateCriteria(p.Criteria); err != nil {
		return err
	}
	if p.OptionProposal.MaxOptions < 0 {
		return fmt.Errorf("option-proposal.max-options must not be negative, got %d", p.OptionProposal.MaxOptions)
	}
	// A cap below the alternatives floor could never produce a usable option
	// set: the runtime would have to inject past its own limit.
	if p.OptionProposal.Enabled {
		if cap := p.OptionProposal.EffectiveMaxOptions(); cap < p.Discipline.Alternatives.MinOptions {
			return fmt.Errorf("option-proposal.max-options (%d) is below discipline.alternatives.min-options (%d)",
				cap, p.Discipline.Alternatives.MinOptions)
		}
	}
	switch p.Finalization.Mode {
	case "", FinalizationAggregate, FinalizationCoordinator:
	case FinalizationJudge:
		judgeID := strings.TrimSpace(p.Finalization.JudgeID)
		if judgeID == "" {
			return fmt.Errorf("finalization.judge-id is required when finalization.mode is %q", FinalizationJudge)
		}
		member := false
		for i := 1; i <= p.IndependentJudgments; i++ {
			if judgeID == fmt.Sprintf("judge-%d", i) {
				member = true
				break
			}
		}
		if !member {
			return fmt.Errorf("finalization.judge-id %q is not one of the configured judges", judgeID)
		}
	default:
		return fmt.Errorf("finalization.mode %q is not supported", p.Finalization.Mode)
	}
	switch p.BudgetDegradation {
	case "", BudgetDegradationForbidden, BudgetDegradationExplicit:
	default:
		return fmt.Errorf("budget-degradation must be %q or %q, got %q", BudgetDegradationForbidden, BudgetDegradationExplicit, p.BudgetDegradation)
	}
	if p.MaxTokens < 0 {
		return fmt.Errorf("max-tokens must not be negative, got %d", p.MaxTokens)
	}
	return p.Discipline.Validate()
}

// requirePinPermitted rejects a role's pin at team-load time (not a runtime
// surprise) unless its profile has explicitly opted in via
// discipline.routing.allow-pinned-binding — spec.md v2 §34's "profile
// permits pin" check.
func requirePinPermitted(pin *RoutingPin, routing RoutingPolicy, roleName string) error {
	if pin == nil || routing.AllowPinnedBinding {
		return nil
	}
	return fmt.Errorf("%s.pin is set but discipline.routing.allow-pinned-binding is not true for this profile", roleName)
}

func validateCriteria(criteria []DecisionCriterion) error {
	seen := make(map[string]bool, len(criteria))
	var total float64
	for i, c := range criteria {
		id := strings.TrimSpace(c.ID)
		if id == "" {
			return fmt.Errorf("criteria[%d].id must not be empty", i)
		}
		if seen[id] {
			return fmt.Errorf("criteria[%d].id %q is duplicated", i, id)
		}
		seen[id] = true
		if math.IsNaN(c.Weight) || math.IsInf(c.Weight, 0) || c.Weight <= 0 {
			return fmt.Errorf("criteria[%d].weight must be a finite value > 0, got %v", i, c.Weight)
		}
		total += c.Weight
		switch c.Direction {
		case "", CriterionHigherIsBetter, CriterionLowerIsBetter:
		default:
			return fmt.Errorf("criteria[%d].direction %q is not supported", i, c.Direction)
		}
	}
	if len(criteria) > 0 && total <= 0 {
		return fmt.Errorf("sum of criteria weights must be > 0, got %v", total)
	}
	return nil
}

// Validate enforces the discipline sub-policy rules (spec §29-§31).
func (d DisciplinePolicy) Validate() error {
	if d.Alternatives.MinOptions < 0 {
		return fmt.Errorf("discipline.alternatives.min-options must not be negative, got %d", d.Alternatives.MinOptions)
	}
	if err := d.Stop.Validate(); err != nil {
		return fmt.Errorf("discipline.stop: %w", err)
	}
	if err := d.Commit.Validate(); err != nil {
		return fmt.Errorf("discipline.commit: %w", err)
	}
	if err := d.Replan.Validate(); err != nil {
		return fmt.Errorf("discipline.replan: %w", err)
	}
	if d.Evidence.RequiredIndependentGroups < 0 {
		return fmt.Errorf("discipline.evidence.required-independent-groups must not be negative, got %d", d.Evidence.RequiredIndependentGroups)
	}
	return nil
}

// Validate enforces the StopPolicy rules (spec §29).
func (s StopPolicy) Validate() error {
	if s.MaxAttempts < 0 || s.MaxToolCalls < 0 || s.MaxTokens < 0 {
		return fmt.Errorf("max-attempts, max-tool-calls and max-tokens must not be negative")
	}
	if s.CheckpointEvery < 0 {
		return fmt.Errorf("checkpoint-every must not be negative, got %d", s.CheckpointEvery)
	}
	if _, err := s.Duration(); err != nil {
		return err
	}
	seen := make(map[string]bool, len(s.KillCriteria))
	for i, k := range s.KillCriteria {
		id := strings.TrimSpace(k.ID)
		if id == "" {
			return fmt.Errorf("kill-criteria[%d].id must not be empty", i)
		}
		if seen[id] {
			return fmt.Errorf("kill-criteria[%d].id %q is duplicated", i, id)
		}
		seen[id] = true
		if !knownKillKinds[k.Kind] {
			return fmt.Errorf("%s: kill-criteria[%d].kind %q is not a computable kill criterion", ReasonStopPolicyUnknownKillKind, i, k.Kind)
		}
		if math.IsNaN(k.Threshold) || math.IsInf(k.Threshold, 0) || k.Threshold < 0 {
			return fmt.Errorf("kill-criteria[%d].threshold must be a finite value >= 0, got %v", i, k.Threshold)
		}
	}
	return nil
}

// Validate enforces the CommitGatePolicy rules (spec §30).
func (c CommitGatePolicy) Validate() error {
	for i, class := range c.RequiredForSideEffects {
		if !knownSideEffectClasses[class] {
			return fmt.Errorf("required-for-side-effects[%d] %q is not a known side-effect class", i, class)
		}
	}
	return nil
}

// Validate enforces the ReplanPolicy rules (spec §31).
func (r ReplanPolicy) Validate() error {
	for key, action := range map[string]string{
		"on-critical-assumption-contradicted": r.OnCriticalAssumptionContradicted,
		"on-material-evidence-changed":        r.OnMaterialEvidenceChanged,
		"on-repeated-failure":                 r.OnRepeatedFailure,
	} {
		if action == "" {
			continue
		}
		if !knownReplanActions[action] {
			return fmt.Errorf("%s action %q is not supported", key, action)
		}
	}
	return nil
}

// EffectiveMinJudgments returns min-independent-judgments, defaulting to
// independent-judgments when unset (spec §12).
func (p DecisionPolicy) EffectiveMinJudgments() int {
	if p.MinIndependentJudgments == 0 {
		return p.IndependentJudgments
	}
	return p.MinIndependentJudgments
}

// EffectiveMaxRounds returns the bounded round count, defaulting to 1.
func (p DecisionPolicy) EffectiveMaxRounds() int {
	if p.MaxRounds == 0 {
		return 1
	}
	return p.MaxRounds
}

// EffectiveIsolation returns the configured isolation level, defaulting to
// strict (spec §16).
func (p DecisionPolicy) EffectiveIsolation() string {
	if p.ContextIsolation == "" {
		return DecisionIsolationStrict
	}
	return p.ContextIsolation
}

// EffectiveAggregation returns the configured aggregator, defaulting to
// mean-score (spec §21).
func (p DecisionPolicy) EffectiveAggregation() string {
	if p.Aggregation.Method == "" {
		return AggregationMeanScore
	}
	return p.Aggregation.Method
}

// EffectiveFinalization returns the configured finalization mode, defaulting to
// aggregate (spec §26).
func (p DecisionPolicy) EffectiveFinalization() string {
	if p.Finalization.Mode == "" {
		return FinalizationAggregate
	}
	return p.Finalization.Mode
}

// EffectiveBudgetDegradation returns the configured degradation mode,
// defaulting to forbidden (spec §34).
func (p DecisionPolicy) EffectiveBudgetDegradation() string {
	if p.BudgetDegradation == "" {
		return BudgetDegradationForbidden
	}
	return p.BudgetDegradation
}

// ReplanAction returns the configured action for a trigger, defaulting to
// continue so an unconfigured policy preserves old behavior (spec §31).
func (r ReplanPolicy) ReplanAction(trigger string) string {
	var action string
	switch trigger {
	case "critical_assumption_contradicted":
		action = r.OnCriticalAssumptionContradicted
	case "material_evidence_changed":
		action = r.OnMaterialEvidenceChanged
	case "repeated_failure":
		action = r.OnRepeatedFailure
	}
	if action == "" {
		return ReplanContinue
	}
	return action
}
