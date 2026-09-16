package agent

import (
	"fmt"
	"strings"
)

const (
	DecisionProfileOriginBuiltin       = "builtin"
	DecisionProfileOriginTeamInline    = "team-inline"
	DecisionProfileOriginRequestInline = "request-inline"
	DecisionProfileOriginLegacyInline  = "legacy-inline"

	DecisionProfileBuiltinLightV1      = "builtin/light@v1"
	DecisionProfileBuiltinStandardV1   = "builtin/standard@v1"
	DecisionProfileBuiltinHighStakesV1 = "builtin/high-stakes@v1"
)

// DecisionProfileRef identifies an immutable catalog entry.
type DecisionProfileRef struct {
	Name string
}

// DecisionProfileSpec is exactly one preset reference or inline policy.
type DecisionProfileSpec struct {
	Preset *DecisionProfileRef `yaml:"preset,omitempty"`
	Policy *DecisionPolicy     `yaml:"policy,omitempty"`
}

// DecisionProfileMetadata describes catalog identity without affecting policy behavior.
type DecisionProfileMetadata struct {
	Ref         string
	Origin      string
	Version     string
	Description string
	Stability   string
}

// MaterializedDecisionProfile is the complete immutable policy selected at admission.
type MaterializedDecisionProfile struct {
	RequestedName string
	Ref           string
	Origin        string
	Version       string
	Policy        DecisionPolicy
	PolicyDigest  string
}

// DecisionProfileCatalog resolves exact, versioned profile references.
type DecisionProfileCatalog interface {
	Resolve(ref DecisionProfileRef) (DecisionPolicy, DecisionProfileMetadata, error)
}

type builtInDecisionProfileCatalog struct{}

type builtInDecisionProfile struct {
	policy   DecisionPolicy
	metadata DecisionProfileMetadata
}

// BuiltInDecisionProfileCatalog returns the immutable catalog shipped with Hufu.
func BuiltInDecisionProfileCatalog() DecisionProfileCatalog {
	return builtInDecisionProfileCatalog{}
}

func (builtInDecisionProfileCatalog) Resolve(ref DecisionProfileRef) (DecisionPolicy, DecisionProfileMetadata, error) {
	name := strings.TrimSpace(ref.Name)
	entry, ok := builtInDecisionProfiles[name]
	if !ok {
		return DecisionPolicy{}, DecisionProfileMetadata{}, fmt.Errorf("unknown decision profile preset %q", name)
	}
	policy, err := CloneDecisionPolicy(entry.policy)
	if err != nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, fmt.Errorf("clone decision profile preset %q: %w", name, err)
	}
	return policy, entry.metadata, nil
}

var builtInDecisionProfiles = map[string]builtInDecisionProfile{
	DecisionProfileBuiltinLightV1: {
		policy: DecisionPolicy{
			IndependentJudgments: 2,
			ContextIsolation:     DecisionIsolationStrict,
			Aggregation:          AggregationPolicy{Method: AggregationMeanScore},
			OptionProposal:       OptionProposalPolicy{Enabled: true},
			BudgetDegradation:    BudgetDegradationForbidden,
			Discipline: DisciplinePolicy{
				Alternatives: AlternativesPolicy{RequireNoActionOption: true, MinOptions: 2},
				Stop:         StopPolicy{MaxAttempts: 2},
				Evidence:     EvidenceIndependencePolicy{RequiredIndependentGroups: 1},
			},
		},
		metadata: builtInProfileMetadata(DecisionProfileBuiltinLightV1, "Lower-cost independent decision review"),
	},
	DecisionProfileBuiltinStandardV1: {
		policy: DecisionPolicy{
			IndependentJudgments: 3,
			ContextIsolation:     DecisionIsolationStrict,
			OutsideView: OutsideViewPolicy{
				Required: true, ReferenceEvidence: true,
				Role: &ReferenceRolePolicy{
					RequiredCapabilities:  []string{"evidence-research"},
					PreferredCapabilities: []string{"architecture"},
				},
			},
			JudgeRole: &JudgeRolePolicy{
				RequiredCapabilities: []string{"decision-analysis"},
				MinDistinctAgents:    3,
				PreferDistinctModels: true,
			},
			Aggregation: AggregationPolicy{Method: AggregationMeanScore},
			Challenge:   ChallengePolicy{Enabled: true, Count: 1},
			ChallengeRole: &ChallengeRolePolicy{
				RequiredCapabilities: []string{"adversarial-analysis"},
			},
			Premortem:         PremortemPolicy{Enabled: true},
			Revision:          RevisionPolicy{Enabled: true},
			Forecast:          ForecastPolicy{Required: true},
			OptionProposal:    OptionProposalPolicy{Enabled: true},
			BudgetDegradation: BudgetDegradationForbidden,
			Discipline: DisciplinePolicy{
				Alternatives: AlternativesPolicy{RequireNoActionOption: true, RequireInfoOption: true, MinOptions: 3},
				Stop: StopPolicy{
					MaxAttempts: 3, CheckpointEvery: 1, RequireKillCriteria: true,
				},
				Commit: CommitGatePolicy{RequireVerification: true, RequireEvidence: true},
				Replan: ReplanPolicy{
					OnCriticalAssumptionContradicted: ReplanReplan,
					OnMaterialEvidenceChanged:        ReplanReplan,
					OnRepeatedFailure:                ReplanReplan,
				},
				Evidence: EvidenceIndependencePolicy{
					RequiredIndependentGroups: 2, RejectCircularCitation: true, WarnSharedOrigin: true,
				},
			},
			MaxRounds: 2,
		},
		metadata: builtInProfileMetadata(DecisionProfileBuiltinStandardV1, "Balanced evidence-first decision review"),
	},
	DecisionProfileBuiltinHighStakesV1: {
		policy: DecisionPolicy{
			IndependentJudgments: 5,
			ContextIsolation:     DecisionIsolationSealed,
			OutsideView: OutsideViewPolicy{
				Required: true, ReferenceEvidence: true,
				Role: &ReferenceRolePolicy{
					RequiredCapabilities:  []string{"evidence-research"},
					PreferredCapabilities: []string{"architecture"},
				},
			},
			JudgeRole: &JudgeRolePolicy{
				RequiredCapabilities: []string{"decision-analysis"},
				MinDistinctAgents:    3,
				PreferDistinctModels: true,
			},
			Aggregation: AggregationPolicy{Method: AggregationMeanScore},
			Challenge:   ChallengePolicy{Enabled: true, Count: 2},
			ChallengeRole: &ChallengeRolePolicy{
				RequiredCapabilities: []string{"adversarial-analysis"},
				MinDistinctAgents:    2,
			},
			Premortem: PremortemPolicy{Enabled: true, RequiredBeforeCommit: true},
			Revision:  RevisionPolicy{Enabled: true},
			Forecast:  ForecastPolicy{Required: true},
			OptionProposal: OptionProposalPolicy{
				Enabled: true,
			},
			BudgetDegradation: BudgetDegradationForbidden,
			Discipline: DisciplinePolicy{
				Alternatives: AlternativesPolicy{RequireNoActionOption: true, RequireInfoOption: true, MinOptions: 3},
				Stop:         StopPolicy{CheckpointEvery: 1, RequireKillCriteria: true},
				Commit: CommitGatePolicy{
					RequireReconcile: true, RequireObservability: true,
					RequireVerification: true, RequireEvidence: true,
				},
				Replan: ReplanPolicy{
					OnCriticalAssumptionContradicted: ReplanReplan,
					OnMaterialEvidenceChanged:        ReplanReplan,
					OnRepeatedFailure:                ReplanStop,
				},
				Evidence: EvidenceIndependencePolicy{
					RequiredIndependentGroups: 2, RejectCircularCitation: true, WarnSharedOrigin: true,
				},
			},
			MaxRounds: 2,
		},
		metadata: builtInProfileMetadata(DecisionProfileBuiltinHighStakesV1, "Maximum built-in decision rigor"),
	},
}

func builtInProfileMetadata(ref, description string) DecisionProfileMetadata {
	_, version, _ := strings.Cut(ref, "@")
	return DecisionProfileMetadata{
		Ref: ref, Origin: DecisionProfileOriginBuiltin, Version: version,
		Description: description, Stability: "stable",
	}
}
