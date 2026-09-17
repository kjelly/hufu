package agent

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBuiltInDecisionProfileCatalogRequiresExactVersion(t *testing.T) {
	catalog := BuiltInDecisionProfileCatalog()
	for _, ref := range []string{
		DecisionProfileBuiltinLightV1,
		DecisionProfileBuiltinStandardV1,
		DecisionProfileBuiltinHighStakesV1,
		DecisionProfileBuiltinLightV2,
		DecisionProfileBuiltinStandardV2,
		DecisionProfileBuiltinHighStakesV2,
	} {
		policy, metadata, err := catalog.Resolve(DecisionProfileRef{Name: ref})
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		_, expectedVersion, _ := strings.Cut(ref, "@")
		if metadata.Ref != ref || metadata.Origin != DecisionProfileOriginBuiltin || metadata.Version != expectedVersion {
			t.Fatalf("metadata for %q = %#v", ref, metadata)
		}
		if err := policy.Validate(); err != nil {
			t.Fatalf("policy %q: %v", ref, err)
		}
	}
	for _, ref := range []string{"", "standard", "builtin/standard", "builtin/standard@v3", "off"} {
		if _, _, err := catalog.Resolve(DecisionProfileRef{Name: ref}); err == nil {
			t.Fatalf("catalog resolved invalid reference %q", ref)
		}
	}
}

func TestBuiltInDecisionProfileBundlesAreFrozenAndComplete(t *testing.T) {
	for _, ref := range BuiltInDecisionProfileBundleRefs() {
		first, err := ResolveBuiltInDecisionProfileBundle(ref)
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		if first.BundleDigest != decisionProfileBundleDigests[ref] || first.RoleResolution.Version != decisionRoleResolverV2 || first.Evidence.Collector != "primary-collector@v1" {
			t.Fatalf("bundle %q identity = %#v", ref, first)
		}
		first.RawJSON[0] = 'x'
		first.RoleResolution.Roles.Judge.RequiredCapabilities = append(first.RoleResolution.Roles.Judge.RequiredCapabilities, "mutated")
		second, err := ResolveBuiltInDecisionProfileBundle(ref)
		if err != nil {
			t.Fatal(err)
		}
		if second.RawJSON[0] == 'x' || slices.Contains(second.RoleResolution.Roles.Judge.RequiredCapabilities, "mutated") {
			t.Fatalf("bundle %q was mutated through returned data", ref)
		}
	}
}

func TestMergeDecisionRoleConstraintsOnlyTightens(t *testing.T) {
	base := DefaultDecisionRoleConstraintsV1()
	base.RequiredCapabilities.Judge = []string{"decision-analysis"}
	base.CandidateLimit = 32
	extra := DefaultDecisionRoleConstraintsV1()
	extra.RequiredCapabilities.Judge = []string{"security-review"}
	extra.Diversity.MinDistinctProviders = 2
	extra.Fallback = "forbid"
	extra.CandidateLimit = 48
	merged, err := MergeDecisionRoleConstraints(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(merged.RequiredCapabilities.Judge, []string{"decision-analysis", "security-review"}) || merged.Diversity.MinDistinctProviders != 2 || merged.Fallback != "forbid" || merged.CandidateLimit != 32 {
		t.Fatalf("merged constraints = %#v", merged)
	}
}

func TestResolvePrimaryDecisionProfileRefKeepsAuxiliaryNamespaceStable(t *testing.T) {
	cfg := DecisionConfig{ProfileSpecs: map[string]DecisionProfileSpec{
		"standard":         {Preset: &DecisionProfileRef{Name: DecisionProfileBuiltinStandardV1}},
		"primary-standard": {Preset: &DecisionProfileRef{Name: DecisionProfileBuiltinStandardV2}},
	}}
	ref, origin, err := ResolvePrimaryDecisionProfileRef(cfg, "primary-standard")
	if err != nil {
		t.Fatal(err)
	}
	if ref != DecisionProfileBuiltinStandardV2 || origin != DecisionProfileOriginTeamInline {
		t.Fatalf("primary profile = %q/%q", ref, origin)
	}
	if _, _, err := ResolvePrimaryDecisionProfileRef(cfg, "standard"); err == nil {
		t.Fatal("local V1 profile must win name resolution and be rejected for primary use")
	}
	policy, _, ok, err := ResolveDecisionProfileSpec(cfg, "standard", BuiltInDecisionProfileCatalog())
	if err != nil || !ok || policy.IndependentJudgments == 0 {
		t.Fatalf("legacy auxiliary profile changed: ok=%v err=%v policy=%#v", ok, err, policy)
	}
}

func TestResolvePrimaryDecisionProfileRefUsesV2OnlyAliases(t *testing.T) {
	for requested, want := range map[string]string{
		"light": DecisionProfileBuiltinLightV2, "standard": DecisionProfileBuiltinStandardV2,
		"high": DecisionProfileBuiltinHighStakesV2, "high-stakes": DecisionProfileBuiltinHighStakesV2,
	} {
		got, origin, err := ResolvePrimaryDecisionProfileRef(DecisionConfig{}, requested)
		if err != nil {
			t.Fatalf("%s: %v", requested, err)
		}
		if got != want || origin != DecisionProfileOriginBuiltin {
			t.Fatalf("%s = %q/%q, want %q/builtin", requested, got, origin, want)
		}
	}
}

func TestBuiltInDecisionProfileCatalogReturnsDeepCopies(t *testing.T) {
	catalog := BuiltInDecisionProfileCatalog()
	first, _, err := catalog.Resolve(DecisionProfileRef{Name: DecisionProfileBuiltinStandardV1})
	if err != nil {
		t.Fatal(err)
	}
	first.OutsideView.Role.RequiredCapabilities[0] = "mutated"
	first.JudgeRole.MinDistinctAgents = 1
	second, _, err := catalog.Resolve(DecisionProfileRef{Name: DecisionProfileBuiltinStandardV1})
	if err != nil {
		t.Fatal(err)
	}
	if second.OutsideView.Role.RequiredCapabilities[0] != "evidence-research" || second.JudgeRole.MinDistinctAgents != 3 {
		t.Fatalf("catalog state was mutated through a returned policy: %#v", second)
	}
}

func TestBuiltInDecisionProfilesMatchFrozenStrategicPolicies(t *testing.T) {
	want := map[string]DecisionPolicy{
		DecisionProfileBuiltinLightV1:      builtInDecisionProfiles[DecisionProfileBuiltinLightV1].policy,
		DecisionProfileBuiltinStandardV1:   builtInDecisionProfiles[DecisionProfileBuiltinStandardV1].policy,
		DecisionProfileBuiltinHighStakesV1: builtInDecisionProfiles[DecisionProfileBuiltinHighStakesV1].policy,
	}
	catalog := BuiltInDecisionProfileCatalog()
	for ref, expected := range want {
		got, _, err := catalog.Resolve(DecisionProfileRef{Name: ref})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, expected) {
			t.Fatalf("catalog policy %q differs from frozen source", ref)
		}
	}
}
