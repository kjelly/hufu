package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// NormalizeDecisionPolicy materializes the semantic defaults used by V1.
func NormalizeDecisionPolicy(policy DecisionPolicy) (DecisionPolicy, error) {
	if err := policy.Validate(); err != nil {
		return DecisionPolicy{}, err
	}
	normalized, err := CloneDecisionPolicy(policy)
	if err != nil {
		return DecisionPolicy{}, err
	}
	normalized.MinIndependentJudgments = normalized.EffectiveMinJudgments()
	normalized.MaxRounds = normalized.EffectiveMaxRounds()
	normalized.ContextIsolation = normalized.EffectiveIsolation()
	normalized.Aggregation.Method = normalized.EffectiveAggregation()
	normalized.Finalization.Mode = normalized.EffectiveFinalization()
	normalized.BudgetDegradation = normalized.EffectiveBudgetDegradation()
	normalized.OptionProposal.MaxOptions = normalized.OptionProposal.EffectiveMaxOptions()
	if err := normalized.Validate(); err != nil {
		return DecisionPolicy{}, fmt.Errorf("validate normalized decision policy: %w", err)
	}
	return normalized, nil
}

// CanonicalDecisionPolicy returns the stable V1 JSON representation.
func CanonicalDecisionPolicy(policy DecisionPolicy) ([]byte, error) {
	normalized, err := NormalizeDecisionPolicy(policy)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonicalDecisionPolicyV1From(normalized))
}

// DecisionPolicyDigest hashes the canonical V1 policy bytes.
func DecisionPolicyDigest(policy DecisionPolicy) (string, error) {
	b, err := CanonicalDecisionPolicy(policy)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// CloneDecisionPolicy returns a deep copy suitable for immutable snapshots.
func CloneDecisionPolicy(policy DecisionPolicy) (DecisionPolicy, error) {
	b, err := json.Marshal(policy)
	if err != nil {
		return DecisionPolicy{}, err
	}
	var clone DecisionPolicy
	if err := json.Unmarshal(b, &clone); err != nil {
		return DecisionPolicy{}, err
	}
	return clone, nil
}

// canonicalDecisionPolicyV1 freezes the field order and JSON names used for
// schema-v2 policy digests. Nested policy values remain typed, while every
// top-level field is explicitly enumerated so a future DecisionPolicy field
// cannot enter historical digests accidentally.
type canonicalDecisionPolicyV1 struct {
	IndependentJudgments    int
	MinIndependentJudgments int
	ContextIsolation        string
	ScoreScale              string
	OutsideView             canonicalOutsideViewPolicyV1
	Criteria                []canonicalDecisionCriterionV1
	JudgeRole               *canonicalJudgeRolePolicyV1
	Aggregation             canonicalAggregationPolicyV1
	Challenge               canonicalChallengePolicyV1
	ChallengeRole           *canonicalChallengeRolePolicyV1
	Revision                canonicalRevisionPolicyV1
	Premortem               canonicalPremortemPolicyV1
	Forecast                canonicalForecastPolicyV1
	Finalization            canonicalFinalizationPolicyV1
	OptionProposal          canonicalOptionProposalPolicyV1
	Discipline              canonicalDisciplinePolicyV1
	MaxRounds               int
	MaxTokens               int64 `json:"max_tokens,omitempty"`
	BudgetDegradation       string
}

func canonicalDecisionPolicyV1From(policy DecisionPolicy) canonicalDecisionPolicyV1 {
	return canonicalDecisionPolicyV1{
		IndependentJudgments: policy.IndependentJudgments, MinIndependentJudgments: policy.MinIndependentJudgments,
		ContextIsolation: policy.ContextIsolation, ScoreScale: policy.ScoreScale,
		OutsideView:   canonicalOutsideViewPolicyV1From(policy.OutsideView),
		Criteria:      canonicalDecisionCriteriaV1From(policy.Criteria),
		JudgeRole:     canonicalJudgeRolePolicyV1From(policy.JudgeRole),
		Aggregation:   canonicalAggregationPolicyV1{Method: policy.Aggregation.Method},
		Challenge:     canonicalChallengePolicyV1From(policy.Challenge),
		ChallengeRole: canonicalChallengeRolePolicyV1From(policy.ChallengeRole),
		Revision:      canonicalRevisionPolicyV1{Enabled: policy.Revision.Enabled},
		Premortem: canonicalPremortemPolicyV1{
			Enabled: policy.Premortem.Enabled, RequiredBeforeCommit: policy.Premortem.RequiredBeforeCommit,
		},
		Forecast:       canonicalForecastPolicyV1{Required: policy.Forecast.Required},
		Finalization:   canonicalFinalizationPolicyV1{Mode: policy.Finalization.Mode, JudgeID: policy.Finalization.JudgeID},
		OptionProposal: canonicalOptionProposalPolicyV1{Enabled: policy.OptionProposal.Enabled, MaxOptions: policy.OptionProposal.MaxOptions},
		Discipline:     canonicalDisciplinePolicyV1From(policy.Discipline),
		MaxRounds:      policy.MaxRounds, MaxTokens: policy.MaxTokens,
		BudgetDegradation: policy.BudgetDegradation,
	}
}

type canonicalDecisionCriterionV1 struct {
	ID        string  `json:"id"`
	Statement string  `json:"statement,omitempty"`
	Weight    float64 `json:"weight"`
	Direction string  `json:"direction,omitempty"`
}

type canonicalRoutingPinV1 struct {
	Agent  string
	Reason string
}

type canonicalReferenceRolePolicyV1 struct {
	RequiredCapabilities  []string
	PreferredCapabilities []string
	Pin                   *canonicalRoutingPinV1
}

type canonicalOutsideViewPolicyV1 struct {
	Required          bool
	ReferenceEvidence bool
	Role              *canonicalReferenceRolePolicyV1
}

type canonicalJudgeRolePolicyV1 struct {
	RequiredCapabilities         []string
	PreferredCapabilities        []string
	MinDistinctAgents            int
	Pin                          *canonicalRoutingPinV1
	MinDistinctModels            int
	MinDistinctProviders         int
	PreferDistinctModels         bool
	PreferDistinctProviders      bool
	AllowRepeatedAgentDefinition bool
}

type canonicalChallengeRolePolicyV1 struct {
	RequiredCapabilities         []string
	PreferredCapabilities        []string
	MinDistinctAgents            int
	Pin                          *canonicalRoutingPinV1
	MinDistinctModels            int
	MinDistinctProviders         int
	PreferDistinctModels         bool
	PreferDistinctProviders      bool
	AllowRepeatedAgentDefinition bool
	AdaptiveCapabilities         map[string][]string
}

type canonicalAggregationPolicyV1 struct {
	Method string
}

type canonicalChallengeTriggerV1 struct {
	DispersionAbove float64
}

type canonicalChallengePolicyV1 struct {
	Enabled bool
	Count   int
	Trigger *canonicalChallengeTriggerV1
}

type canonicalRevisionPolicyV1 struct {
	Enabled bool
}

type canonicalPremortemPolicyV1 struct {
	Enabled              bool
	RequiredBeforeCommit bool
}

type canonicalForecastPolicyV1 struct {
	Required bool
}

type canonicalFinalizationPolicyV1 struct {
	Mode    string
	JudgeID string
}

type canonicalOptionProposalPolicyV1 struct {
	Enabled    bool
	MaxOptions int
}

type canonicalDisciplinePolicyV1 struct {
	Alternatives canonicalAlternativesPolicyV1
	Stop         canonicalStopPolicyV1
	Commit       canonicalCommitGatePolicyV1
	Replan       canonicalReplanPolicyV1
	Evidence     canonicalEvidenceIndependencePolicyV1
	Routing      canonicalRoutingPolicyV1
}

type canonicalRoutingPolicyV1 struct {
	CapabilityAware    bool
	AllowPinnedBinding bool
}

type canonicalAlternativesPolicyV1 struct {
	RequireNoActionOption bool
	RequireInfoOption     bool
	MinOptions            int
}

type canonicalStopPolicyV1 struct {
	MaxAttempts         int                        `json:"max_attempts,omitempty"`
	MaxToolCalls        int                        `json:"max_tool_calls,omitempty"`
	MaxTokens           int64                      `json:"max_tokens,omitempty"`
	MaxDuration         string                     `json:"max_duration,omitempty"`
	CheckpointEvery     int                        `json:"checkpoint_every,omitempty"`
	RequireKillCriteria bool                       `json:"require_kill_criteria,omitempty"`
	KillCriteria        []canonicalKillCriterionV1 `json:"kill_criteria,omitempty"`
}

type canonicalKillCriterionV1 struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	Threshold   float64 `json:"threshold,omitempty"`
	Description string  `json:"description,omitempty"`
}

type canonicalCommitGatePolicyV1 struct {
	RequiredForSideEffects []string
	RequireRollback        bool
	RequireReconcile       bool
	RequireObservability   bool
	RequireVerification    bool
	RequireEvidence        bool
}

type canonicalReplanPolicyV1 struct {
	OnCriticalAssumptionContradicted string
	OnMaterialEvidenceChanged        string
	OnRepeatedFailure                string
}

type canonicalEvidenceIndependencePolicyV1 struct {
	RequiredIndependentGroups int
	RejectCircularCitation    bool
	WarnSharedOrigin          bool
}

func canonicalDecisionCriteriaV1From(criteria []DecisionCriterion) []canonicalDecisionCriterionV1 {
	if len(criteria) == 0 {
		return nil
	}
	result := make([]canonicalDecisionCriterionV1, len(criteria))
	for i, criterion := range criteria {
		weight := criterion.Weight
		if weight == 0 {
			weight = 0
		}
		result[i] = canonicalDecisionCriterionV1{
			ID: criterion.ID, Statement: criterion.Statement, Weight: weight, Direction: criterion.Direction,
		}
	}
	return result
}

func canonicalRoutingPinV1From(pin *RoutingPin) *canonicalRoutingPinV1 {
	if pin == nil {
		return nil
	}
	return &canonicalRoutingPinV1{Agent: pin.Agent, Reason: pin.Reason}
}

func canonicalReferenceRolePolicyV1From(role *ReferenceRolePolicy) *canonicalReferenceRolePolicyV1 {
	if role == nil {
		return nil
	}
	return &canonicalReferenceRolePolicyV1{
		RequiredCapabilities:  normalizeCanonicalStrings(role.RequiredCapabilities),
		PreferredCapabilities: normalizeCanonicalStrings(role.PreferredCapabilities),
		Pin:                   canonicalRoutingPinV1From(role.Pin),
	}
}

func canonicalOutsideViewPolicyV1From(policy OutsideViewPolicy) canonicalOutsideViewPolicyV1 {
	return canonicalOutsideViewPolicyV1{
		Required: policy.Required, ReferenceEvidence: policy.ReferenceEvidence,
		Role: canonicalReferenceRolePolicyV1From(policy.Role),
	}
}

func canonicalJudgeRolePolicyV1From(role *JudgeRolePolicy) *canonicalJudgeRolePolicyV1 {
	if role == nil {
		return nil
	}
	return &canonicalJudgeRolePolicyV1{
		RequiredCapabilities: normalizeCanonicalStrings(role.RequiredCapabilities), PreferredCapabilities: normalizeCanonicalStrings(role.PreferredCapabilities),
		MinDistinctAgents: role.MinDistinctAgents, Pin: canonicalRoutingPinV1From(role.Pin),
		MinDistinctModels: role.MinDistinctModels, MinDistinctProviders: role.MinDistinctProviders,
		PreferDistinctModels: role.PreferDistinctModels, PreferDistinctProviders: role.PreferDistinctProviders,
		AllowRepeatedAgentDefinition: role.AllowRepeatedAgentDefinition,
	}
}

func canonicalChallengeRolePolicyV1From(role *ChallengeRolePolicy) *canonicalChallengeRolePolicyV1 {
	if role == nil {
		return nil
	}
	adaptive := role.AdaptiveCapabilities
	if len(adaptive) == 0 {
		adaptive = nil
	}
	return &canonicalChallengeRolePolicyV1{
		RequiredCapabilities: normalizeCanonicalStrings(role.RequiredCapabilities), PreferredCapabilities: normalizeCanonicalStrings(role.PreferredCapabilities),
		MinDistinctAgents: role.MinDistinctAgents, Pin: canonicalRoutingPinV1From(role.Pin),
		MinDistinctModels: role.MinDistinctModels, MinDistinctProviders: role.MinDistinctProviders,
		PreferDistinctModels: role.PreferDistinctModels, PreferDistinctProviders: role.PreferDistinctProviders,
		AllowRepeatedAgentDefinition: role.AllowRepeatedAgentDefinition, AdaptiveCapabilities: adaptive,
	}
}

func canonicalChallengePolicyV1From(policy ChallengePolicy) canonicalChallengePolicyV1 {
	var trigger *canonicalChallengeTriggerV1
	if policy.Trigger != nil {
		dispersion := policy.Trigger.DispersionAbove
		if dispersion == 0 {
			dispersion = 0
		}
		trigger = &canonicalChallengeTriggerV1{DispersionAbove: dispersion}
	}
	return canonicalChallengePolicyV1{Enabled: policy.Enabled, Count: policy.Count, Trigger: trigger}
}

func canonicalDisciplinePolicyV1From(policy DisciplinePolicy) canonicalDisciplinePolicyV1 {
	return canonicalDisciplinePolicyV1{
		Alternatives: canonicalAlternativesPolicyV1{
			RequireNoActionOption: policy.Alternatives.RequireNoActionOption,
			RequireInfoOption:     policy.Alternatives.RequireInfoOption, MinOptions: policy.Alternatives.MinOptions,
		},
		Stop: canonicalStopPolicyV1From(policy.Stop),
		Commit: canonicalCommitGatePolicyV1{
			RequiredForSideEffects: normalizeCanonicalStrings(policy.Commit.RequiredForSideEffects),
			RequireRollback:        policy.Commit.RequireRollback, RequireReconcile: policy.Commit.RequireReconcile,
			RequireObservability: policy.Commit.RequireObservability, RequireVerification: policy.Commit.RequireVerification,
			RequireEvidence: policy.Commit.RequireEvidence,
		},
		Replan: canonicalReplanPolicyV1{
			OnCriticalAssumptionContradicted: policy.Replan.OnCriticalAssumptionContradicted,
			OnMaterialEvidenceChanged:        policy.Replan.OnMaterialEvidenceChanged,
			OnRepeatedFailure:                policy.Replan.OnRepeatedFailure,
		},
		Evidence: canonicalEvidenceIndependencePolicyV1{
			RequiredIndependentGroups: policy.Evidence.RequiredIndependentGroups,
			RejectCircularCitation:    policy.Evidence.RejectCircularCitation, WarnSharedOrigin: policy.Evidence.WarnSharedOrigin,
		},
		Routing: canonicalRoutingPolicyV1{
			CapabilityAware: policy.Routing.CapabilityAware, AllowPinnedBinding: policy.Routing.AllowPinnedBinding,
		},
	}
}

func canonicalStopPolicyV1From(policy StopPolicy) canonicalStopPolicyV1 {
	var criteria []canonicalKillCriterionV1
	if len(policy.KillCriteria) > 0 {
		criteria = make([]canonicalKillCriterionV1, len(policy.KillCriteria))
		for i, criterion := range policy.KillCriteria {
			threshold := criterion.Threshold
			if threshold == 0 {
				threshold = 0
			}
			criteria[i] = canonicalKillCriterionV1{
				ID: criterion.ID, Kind: criterion.Kind, Threshold: threshold, Description: criterion.Description,
			}
		}
	}
	return canonicalStopPolicyV1{
		MaxAttempts: policy.MaxAttempts, MaxToolCalls: policy.MaxToolCalls, MaxTokens: policy.MaxTokens,
		MaxDuration: policy.MaxDuration, CheckpointEvery: policy.CheckpointEvery,
		RequireKillCriteria: policy.RequireKillCriteria, KillCriteria: criteria,
	}
}

func normalizeCanonicalStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return values
}
