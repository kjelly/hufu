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
