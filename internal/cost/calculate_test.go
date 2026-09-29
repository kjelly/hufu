package cost

import "testing"

func TestEstimateUsageCostPricesCacheComponents(t *testing.T) {
	catalog, err := NewCatalog(map[string]PriceConfig{"openai/gpt": meteredPrice()})
	if err != nil {
		t.Fatal(err)
	}
	price, _ := catalog.Resolve("openai/gpt")
	estimate, err := EstimateUsageCost(price, TokenUsage{
		InputTokens: 1_000_000, CacheReadTokens: 1_000_000,
		CacheCreationTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if estimate.Micros == nil || *estimate.Micros != 12_200_000 || estimate.Source != EstimateUsage {
		t.Fatalf("estimate = %#v", estimate)
	}
}

func TestEstimateModesStayDistinct(t *testing.T) {
	tests := []struct {
		mode   BillingMode
		source EstimateSource
	}{
		{mode: BillingLocal, source: EstimateNotMetered},
		{mode: BillingSubscription, source: EstimateSubscription},
		{mode: BillingUnknown, source: EstimateUnknown},
	}
	for _, tt := range tests {
		estimate, err := EstimateUsageCost(PriceSnapshot{BillingMode: tt.mode}, TokenUsage{})
		if err != nil {
			t.Fatal(err)
		}
		if estimate.Micros != nil || estimate.Source != tt.source || estimate.BillingMode != tt.mode {
			t.Fatalf("%s estimate = %#v", tt.mode, estimate)
		}
	}
}

func TestAdmissionBounds(t *testing.T) {
	catalog, err := NewCatalog(map[string]PriceConfig{
		"openai/gpt": {BillingMode: BillingMetered, InputUSDPerMillion: "2", CacheReadUSDPerMillion: "3", CacheWriteUSDPerMillion: "4", OutputUSDPerMillion: "8"},
		"codex/gpt":  {BillingMode: BillingMetered, OpaqueMaxUSDPerInvocation: "0.10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenPrice, _ := catalog.Resolve("openai/gpt")
	tokenEstimate, err := EstimateTokenAdmissionBound(tokenPrice, 1_000_000, 500_000)
	if err != nil {
		t.Fatal(err)
	}
	if tokenEstimate.Micros == nil || *tokenEstimate.Micros != 8_000_000 || tokenEstimate.Source != EstimateAdmissionBound {
		t.Fatalf("token admission estimate = %#v", tokenEstimate)
	}
	opaquePrice, _ := catalog.Resolve("codex/gpt")
	opaqueEstimate, err := EstimateOpaqueBound(opaquePrice)
	if err != nil {
		t.Fatal(err)
	}
	if opaqueEstimate.Micros == nil || *opaqueEstimate.Micros != 100_000 || opaqueEstimate.Source != EstimateAdmissionBound {
		t.Fatalf("opaque estimate = %#v", opaqueEstimate)
	}
	usageEstimate, err := EstimateUsageCost(opaquePrice, TokenUsage{InputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if usageEstimate.Micros == nil || *usageEstimate.Micros != 100_000 || usageEstimate.Source != EstimateAdmissionBound {
		t.Fatalf("opaque usage fallback = %#v", usageEstimate)
	}
}

func TestEstimateRejectsInvalidInputs(t *testing.T) {
	if _, err := EstimateUsageCost(PriceSnapshot{BillingMode: BillingUnknown}, TokenUsage{InputTokens: -1}); err == nil {
		t.Fatal("negative usage was accepted")
	}
	if _, err := EstimateTokenAdmissionBound(PriceSnapshot{BillingMode: BillingMetered}, 1, 1); err == nil {
		t.Fatal("missing token price was accepted")
	}
	if _, err := EstimateOpaqueBound(PriceSnapshot{BillingMode: BillingMetered}); err == nil {
		t.Fatal("missing opaque maximum was accepted")
	}
}
