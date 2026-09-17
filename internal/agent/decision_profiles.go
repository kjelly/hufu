package agent

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
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

// BuiltInDecisionProfileMetadata returns the immutable catalog identities in
// deterministic reference order for provider-free inspection commands.
func BuiltInDecisionProfileMetadata() []DecisionProfileMetadata {
	metadata := make([]DecisionProfileMetadata, 0, len(builtInDecisionProfiles)+len(decisionProfileBundleFiles))
	for _, entry := range builtInDecisionProfiles {
		metadata = append(metadata, entry.metadata)
	}
	for _, ref := range BuiltInDecisionProfileBundleRefs() {
		metadata = append(metadata, builtInProfileMetadata(ref, builtInDecisionProfileBundleDescription(ref)))
	}
	slices.SortFunc(metadata, func(a, b DecisionProfileMetadata) int { return strings.Compare(a.Ref, b.Ref) })
	return metadata
}

// ResolveDecisionProfileSpec resolves one exact built-in or team-local entry.
// The returned policy is always a deep copy. found is false only for a local
// name absent from both compatibility maps.
func ResolveDecisionProfileSpec(cfg DecisionConfig, name string, catalog DecisionProfileCatalog) (DecisionPolicy, DecisionProfileMetadata, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == DecisionProfileOff {
		return DecisionPolicy{}, DecisionProfileMetadata{}, false, nil
	}
	if catalog == nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, false, fmt.Errorf("decision profile catalog is unavailable")
	}
	if strings.HasPrefix(name, "builtin/") {
		policy, metadata, err := catalog.Resolve(DecisionProfileRef{Name: name})
		if err == nil {
			err = policy.Validate()
		}
		return policy, metadata, err == nil, err
	}

	spec, hasSpec := cfg.ProfileSpecs[name]
	legacy, hasLegacy := cfg.Profiles[name]
	if !hasSpec && !hasLegacy {
		return DecisionPolicy{}, DecisionProfileMetadata{}, false, nil
	}
	if !hasSpec {
		policy, err := CloneDecisionPolicy(legacy)
		if err == nil {
			err = policy.Validate()
		}
		return policy, DecisionProfileMetadata{Origin: DecisionProfileOriginTeamInline}, true, err
	}

	policy, metadata, err := resolveDeclaredDecisionProfileSpec(spec, catalog)
	if err != nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, false, err
	}
	if hasLegacy {
		left, leftErr := CanonicalDecisionPolicy(policy)
		right, rightErr := CanonicalDecisionPolicy(legacy)
		if leftErr != nil {
			return DecisionPolicy{}, DecisionProfileMetadata{}, false, leftErr
		}
		if rightErr != nil {
			return DecisionPolicy{}, DecisionProfileMetadata{}, false, rightErr
		}
		if !bytes.Equal(left, right) {
			return DecisionPolicy{}, DecisionProfileMetadata{}, false, fmt.Errorf("profile spec conflicts with compatibility policy projection")
		}
	}
	return policy, metadata, true, nil
}

func resolveDeclaredDecisionProfileSpec(spec DecisionProfileSpec, catalog DecisionProfileCatalog) (DecisionPolicy, DecisionProfileMetadata, error) {
	if (spec.Preset == nil) == (spec.Policy == nil) {
		return DecisionPolicy{}, DecisionProfileMetadata{}, fmt.Errorf("profile spec requires exactly one of preset or policy")
	}
	if spec.Preset != nil {
		return catalog.Resolve(*spec.Preset)
	}
	policy, err := CloneDecisionPolicy(*spec.Policy)
	if err != nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, err
	}
	if err := policy.Validate(); err != nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, err
	}
	return policy, DecisionProfileMetadata{Origin: DecisionProfileOriginTeamInline}, nil
}

// MaterializeDecisionConfig returns a validated deep copy whose compatibility
// map is rebuilt from the authoritative profile specs.
func MaterializeDecisionConfig(cfg DecisionConfig, catalog DecisionProfileCatalog) (DecisionConfig, error) {
	if catalog == nil {
		return DecisionConfig{}, fmt.Errorf("decision profile catalog is unavailable")
	}
	for i, hint := range cfg.RoutingHints {
		if err := hint.Validate(); err != nil {
			return DecisionConfig{}, fmt.Errorf("decision.routing-hints[%d]: %w", i, err)
		}
	}

	result := cfg
	result.Profiles = make(map[string]DecisionPolicy, len(cfg.Profiles)+len(cfg.ProfileSpecs))
	result.ProfileSpecs = make(map[string]DecisionProfileSpec, len(cfg.Profiles)+len(cfg.ProfileSpecs))
	names := make(map[string]struct{}, len(cfg.Profiles)+len(cfg.ProfileSpecs))
	for name := range cfg.Profiles {
		names[name] = struct{}{}
	}
	for name := range cfg.ProfileSpecs {
		names[name] = struct{}{}
	}
	for name := range names {
		if name == DecisionProfileOff || strings.HasPrefix(name, "builtin/") || strings.TrimSpace(name) == "" {
			return DecisionConfig{}, fmt.Errorf("decision.profiles: invalid reserved profile name %q", name)
		}
		policy, _, ok, err := ResolveDecisionProfileSpec(cfg, name, catalog)
		if err != nil {
			return DecisionConfig{}, fmt.Errorf("decision.profiles.%s: %w", name, err)
		}
		if !ok {
			return DecisionConfig{}, fmt.Errorf("decision.profiles.%s is not resolvable", name)
		}
		result.Profiles[name] = policy
		if spec, exists := cfg.ProfileSpecs[name]; exists {
			result.ProfileSpecs[name] = cloneDecisionProfileSpec(spec)
		} else {
			inline := policy
			result.ProfileSpecs[name] = DecisionProfileSpec{Policy: &inline}
		}
	}
	if result.DefaultProfile != "" && result.DefaultProfile != DecisionProfileOff {
		if _, _, ok, err := ResolveDecisionProfileSpec(result, result.DefaultProfile, catalog); err != nil || !ok {
			if err != nil {
				return DecisionConfig{}, fmt.Errorf("%s: decision.default-profile %q: %w", ReasonDecisionProfileUnknown, result.DefaultProfile, err)
			}
			return DecisionConfig{}, fmt.Errorf("%s: decision.default-profile %q is not defined", ReasonDecisionProfileUnknown, result.DefaultProfile)
		}
	}
	return result, nil
}

func cloneDecisionProfileSpec(spec DecisionProfileSpec) DecisionProfileSpec {
	result := DecisionProfileSpec{}
	if spec.Preset != nil {
		result.Preset = &DecisionProfileRef{Name: spec.Preset.Name}
	}
	if spec.Policy != nil {
		policy, err := CloneDecisionPolicy(*spec.Policy)
		if err == nil {
			result.Policy = &policy
		}
	}
	return result
}

// EqualMaterializedDecisionPolicies compares complete policy semantics.
func EqualMaterializedDecisionPolicies(a, b DecisionPolicy) bool {
	left, leftErr := NormalizeDecisionPolicy(a)
	right, rightErr := NormalizeDecisionPolicy(b)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(left, right)
}

func (builtInDecisionProfileCatalog) Resolve(ref DecisionProfileRef) (DecisionPolicy, DecisionProfileMetadata, error) {
	name := strings.TrimSpace(ref.Name)
	entry, ok := builtInDecisionProfiles[name]
	if ok {
		policy, err := CloneDecisionPolicy(entry.policy)
		if err != nil {
			return DecisionPolicy{}, DecisionProfileMetadata{}, fmt.Errorf("clone decision profile preset %q: %w", name, err)
		}
		return policy, entry.metadata, nil
	}
	bundle, err := ResolveBuiltInDecisionProfileBundle(name)
	if err != nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, fmt.Errorf("unknown decision profile preset %q: %w", name, err)
	}
	policy, err := CloneDecisionPolicy(bundle.Policy)
	if err != nil {
		return DecisionPolicy{}, DecisionProfileMetadata{}, fmt.Errorf("clone decision profile preset %q: %w", name, err)
	}
	return policy, builtInProfileMetadata(name, builtInDecisionProfileBundleDescription(name)), nil
}

func builtInDecisionProfileBundleDescription(ref string) string {
	switch ref {
	case DecisionProfileBuiltinLightV2:
		return "Bounded primary decision formation"
	case DecisionProfileBuiltinStandardV2:
		return "Evidence-first primary decision formation"
	case DecisionProfileBuiltinHighStakesV2:
		return "High-rigor primary decision formation"
	default:
		return ""
	}
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
