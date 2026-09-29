package cost

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolveRunPolicyDefaultsAndHardLimit(t *testing.T) {
	unset, err := ResolveRunPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if unset.Configured || unset.UnknownPricePolicy != UnknownPriceAllow {
		t.Fatalf("unset policy = %#v, want unconfigured allow", unset)
	}

	empty, err := ResolveRunPolicy(&PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Configured || empty.UnknownPricePolicy != UnknownPriceAllow {
		t.Fatalf("empty policy = %#v, want configured allow", empty)
	}

	hard, err := ResolveRunPolicy(&PolicyConfig{MaxRunUSD: "1.50", WarningRunUSD: "1.00"})
	if err != nil {
		t.Fatal(err)
	}
	if hard.MaxRunMicros == nil || *hard.MaxRunMicros != 1_500_000 || hard.WarningRunMicros == nil || *hard.WarningRunMicros != 1_000_000 || hard.UnknownPricePolicy != UnknownPriceDeny {
		t.Fatalf("hard policy = %#v, want 1.50/1.00 USD and deny", hard)
	}
}

func TestResolveRunPolicyRejectsInvalidCombinations(t *testing.T) {
	tests := []PolicyConfig{
		{MaxRunUSD: "0"},
		{WarningRunUSD: "-1"},
		{MaxRunUSD: "1", WarningRunUSD: "2"},
		{MaxRunUSD: "1", UnknownPricePolicy: UnknownPriceAllow},
		{UnknownPricePolicy: "guess"},
	}
	for _, config := range tests {
		if _, err := ResolveRunPolicy(&config); err == nil {
			t.Fatalf("ResolveRunPolicy(%#v) succeeded", config)
		}
	}
}

func TestPolicyConfigYAMLIsStrictAndRequiresStringMoney(t *testing.T) {
	for _, document := range []string{
		"max-run-usd: 1.5\n",
		"warning-run-usd: \"1\"\nunknown-field: true\n",
		"unknown-price-policy: guess\n",
	} {
		var config PolicyConfig
		if err := yaml.Unmarshal([]byte(document), &config); err == nil {
			t.Fatalf("yaml.Unmarshal accepted:\n%s", document)
		}
	}
}

func TestPolicySnapshotIsDeterministicDefensiveAndTamperEvident(t *testing.T) {
	catalog, err := NewCatalog(map[string]PriceConfig{
		"openai/b": {BillingMode: BillingMetered, InputUSDPerMillion: "2", OutputUSDPerMillion: "8"},
		"openai/a": {BillingMode: BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := ResolveRunPolicy(&PolicyConfig{MaxRunUSD: "2", WarningRunUSD: "1", UnknownPricePolicy: UnknownPriceDeny})
	if err != nil {
		t.Fatal(err)
	}
	prices := catalog.Snapshots()
	slices.Reverse(prices)
	first, err := NewPolicySnapshot(policy, prices)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewPolicySnapshot(policy, catalog.Snapshots())
	if err != nil {
		t.Fatal(err)
	}
	if first.PolicyHash == "" || first.PolicyHash != second.PolicyHash || first.Prices[0].ExecutionTarget != "openai/a" {
		t.Fatalf("nondeterministic policy snapshots: %#v %#v", first, second)
	}

	clone := ClonePolicySnapshot(first)
	*clone.MaxRunMicros = 99
	*clone.Prices[0].InputMicrosPerMillion = 99
	if *first.MaxRunMicros == 99 || *first.Prices[0].InputMicrosPerMillion == 99 {
		t.Fatal("ClonePolicySnapshot exposed mutable state")
	}
	if err := ValidatePolicySnapshot(clone); err == nil {
		t.Fatal("tampered policy snapshot validated")
	}
}

func TestPolicySnapshotOmittedWhenCostIsUnused(t *testing.T) {
	policy, err := ResolveRunPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewPolicySnapshot(policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot != nil {
		t.Fatalf("unused snapshot = %#v, want nil", snapshot)
	}
}
