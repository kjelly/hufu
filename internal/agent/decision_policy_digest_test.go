package agent

import (
	"math"
	"reflect"
	"testing"
)

func TestDecisionPolicyDigestGoldenBuiltIns(t *testing.T) {
	want := map[string]string{
		DecisionProfileBuiltinLightV1:      "sha256:50e6853ad3715ea7c97c8c342bc52e9bc5dfb5ee603a2d6b75de9be030962780",
		DecisionProfileBuiltinStandardV1:   "sha256:8e5584348b79d0b87fd1a663449eec84089fa445bd005297a87d46783f6a5d2d",
		DecisionProfileBuiltinHighStakesV1: "sha256:c5d6893ceaf4082b7d13115c52b2975e2e05fb0fb63498e3a203635d32774a63",
	}
	catalog := BuiltInDecisionProfileCatalog()
	for ref, expected := range want {
		policy, _, err := catalog.Resolve(DecisionProfileRef{Name: ref})
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecisionPolicyDigest(policy)
		if err != nil {
			t.Fatal(err)
		}
		if got != expected {
			t.Errorf("digest %q = %q, want %q", ref, got, expected)
		}
	}
}

func TestDecisionPolicyDigestNormalizesDefaultsWithoutMutation(t *testing.T) {
	implicit := DecisionPolicy{IndependentJudgments: 2}
	explicit := implicit
	explicit.MinIndependentJudgments = 2
	explicit.MaxRounds = 1
	explicit.ContextIsolation = DecisionIsolationStrict
	explicit.Aggregation.Method = AggregationMeanScore
	explicit.Finalization.Mode = FinalizationAggregate
	explicit.BudgetDegradation = BudgetDegradationForbidden
	explicit.OptionProposal.MaxOptions = defaultMaxProposedOptions

	before := implicit
	a, err := DecisionPolicyDigest(implicit)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecisionPolicyDigest(explicit)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("equivalent defaults have different digests: %s != %s", a, b)
	}
	if !reflect.DeepEqual(implicit, before) {
		t.Fatalf("normalization mutated input: got %#v, want %#v", implicit, before)
	}
	normalized, err := NormalizeDecisionPolicy(implicit)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := NormalizeDecisionPolicy(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(normalized, twice) {
		t.Fatalf("normalization is not idempotent: %#v != %#v", normalized, twice)
	}
}

func TestDecisionPolicyDigestIsDeterministicAcrossMapInsertionOrder(t *testing.T) {
	base := DecisionPolicy{
		IndependentJudgments: 2,
		Criteria: []DecisionCriterion{
			{ID: "risk", Weight: 1},
			{ID: "cost", Weight: 1},
		},
		Challenge: ChallengePolicy{Enabled: true, Count: 1},
		ChallengeRole: &ChallengeRolePolicy{
			RequiredCapabilities: []string{"analysis"},
			AdaptiveCapabilities: map[string][]string{
				"risk": {"security"},
				"cost": {"finance"},
			},
		},
	}
	reordered, err := CloneDecisionPolicy(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered.ChallengeRole.AdaptiveCapabilities = make(map[string][]string)
	reordered.ChallengeRole.AdaptiveCapabilities["cost"] = []string{"finance"}
	reordered.ChallengeRole.AdaptiveCapabilities["risk"] = []string{"security"}
	a, err := DecisionPolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecisionPolicyDigest(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("map insertion order changed digest: %s != %s", a, b)
	}
}

func TestDecisionPolicyDigestRejectsInvalidFloatsAndDetectsChanges(t *testing.T) {
	invalid := DecisionPolicy{
		IndependentJudgments: 1,
		Criteria:             []DecisionCriterion{{ID: "risk", Weight: math.NaN()}},
	}
	if _, err := DecisionPolicyDigest(invalid); err == nil {
		t.Fatal("digest accepted NaN")
	}
	invalid.Criteria[0].Weight = math.Inf(1)
	if _, err := DecisionPolicyDigest(invalid); err == nil {
		t.Fatal("digest accepted infinity")
	}

	base := DecisionPolicy{IndependentJudgments: 1}
	changed := base
	changed.Forecast.Required = true
	a, err := DecisionPolicyDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecisionPolicyDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("changed policy field did not change digest")
	}
}

func TestCanonicalDecisionPolicyV1EnumeratesEveryPolicyField(t *testing.T) {
	policyType := reflect.TypeFor[DecisionPolicy]()
	canonicalType := reflect.TypeFor[canonicalDecisionPolicyV1]()
	if canonicalType.NumField() != policyType.NumField() {
		t.Fatalf("canonical fields = %d, DecisionPolicy fields = %d", canonicalType.NumField(), policyType.NumField())
	}
	for i := range policyType.NumField() {
		if got, want := canonicalType.Field(i).Name, policyType.Field(i).Name; got != want {
			t.Fatalf("canonical field %d = %q, want %q", i, got, want)
		}
	}
}
