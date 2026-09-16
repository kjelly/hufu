package agent

import (
	"reflect"
	"testing"
)

func TestBuiltInDecisionProfileCatalogRequiresExactVersion(t *testing.T) {
	catalog := BuiltInDecisionProfileCatalog()
	for _, ref := range []string{
		DecisionProfileBuiltinLightV1,
		DecisionProfileBuiltinStandardV1,
		DecisionProfileBuiltinHighStakesV1,
	} {
		policy, metadata, err := catalog.Resolve(DecisionProfileRef{Name: ref})
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		if metadata.Ref != ref || metadata.Origin != DecisionProfileOriginBuiltin || metadata.Version != "v1" {
			t.Fatalf("metadata for %q = %#v", ref, metadata)
		}
		if err := policy.Validate(); err != nil {
			t.Fatalf("policy %q: %v", ref, err)
		}
	}
	for _, ref := range []string{"", "standard", "builtin/standard", "builtin/standard@v2", "off"} {
		if _, _, err := catalog.Resolve(DecisionProfileRef{Name: ref}); err == nil {
			t.Fatalf("catalog resolved invalid reference %q", ref)
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
