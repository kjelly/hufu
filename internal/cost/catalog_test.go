package cost

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func meteredPrice() PriceConfig {
	return PriceConfig{
		BillingMode: BillingMetered, InputUSDPerMillion: "2.00", CacheReadUSDPerMillion: "0.20",
		CacheWriteUSDPerMillion: "2.00", OutputUSDPerMillion: "8.00",
	}
}

func TestCatalogCompilesDeterministicallyAndFallsBackCacheRates(t *testing.T) {
	configsA := map[string]PriceConfig{
		"openai/gpt":  {BillingMode: BillingMetered, InputUSDPerMillion: "2", OutputUSDPerMillion: "8"},
		"ollama/qwen": {BillingMode: BillingLocal},
	}
	configsB := map[string]PriceConfig{
		"ollama/qwen": {BillingMode: BillingLocal},
		"openai/gpt":  {BillingMode: BillingMetered, InputUSDPerMillion: "2", OutputUSDPerMillion: "8"},
	}
	first, err := NewCatalog(configsA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCatalog(configsB)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() == "" || first.Hash() != second.Hash() {
		t.Fatalf("catalog hashes = %q and %q", first.Hash(), second.Hash())
	}
	if !slices.EqualFunc(first.Snapshots(), second.Snapshots(), func(left, right PriceSnapshot) bool { return left.ID == right.ID }) {
		t.Fatal("catalog snapshot order or IDs are nondeterministic")
	}
	snapshot, ok := first.Resolve("openai/gpt")
	if !ok || snapshot.InputMicrosPerMillion == nil || snapshot.CacheReadMicrosPerMillion == nil || snapshot.CacheWriteMicrosPerMillion == nil {
		t.Fatalf("resolved snapshot = %#v", snapshot)
	}
	if *snapshot.CacheReadMicrosPerMillion != *snapshot.InputMicrosPerMillion || *snapshot.CacheWriteMicrosPerMillion != *snapshot.InputMicrosPerMillion {
		t.Fatalf("cache fallback rates = %d/%d, input = %d", *snapshot.CacheReadMicrosPerMillion, *snapshot.CacheWriteMicrosPerMillion, *snapshot.InputMicrosPerMillion)
	}
	*snapshot.InputMicrosPerMillion = 99
	again, _ := first.Resolve("openai/gpt")
	if *again.InputMicrosPerMillion == 99 {
		t.Fatal("Resolve exposed mutable catalog state")
	}
}

func TestCatalogHashCoversPriceFields(t *testing.T) {
	base, err := NewCatalog(map[string]PriceConfig{"openai/gpt": meteredPrice()})
	if err != nil {
		t.Fatal(err)
	}
	mutations := []PriceConfig{
		{BillingMode: BillingMetered, InputUSDPerMillion: "3", CacheReadUSDPerMillion: "0.20", CacheWriteUSDPerMillion: "2.00", OutputUSDPerMillion: "8.00"},
		{BillingMode: BillingMetered, InputUSDPerMillion: "2.00", CacheReadUSDPerMillion: "0.30", CacheWriteUSDPerMillion: "2.00", OutputUSDPerMillion: "8.00"},
		{BillingMode: BillingMetered, InputUSDPerMillion: "2.00", CacheReadUSDPerMillion: "0.20", CacheWriteUSDPerMillion: "3.00", OutputUSDPerMillion: "8.00"},
		{BillingMode: BillingMetered, InputUSDPerMillion: "2.00", CacheReadUSDPerMillion: "0.20", CacheWriteUSDPerMillion: "2.00", OutputUSDPerMillion: "9.00"},
		{BillingMode: BillingMetered, OpaqueMaxUSDPerInvocation: "0.10"},
	}
	for index, mutation := range mutations {
		changed, err := NewCatalog(map[string]PriceConfig{"openai/gpt": mutation})
		if err != nil {
			t.Fatalf("mutation %d: %v", index, err)
		}
		if changed.Hash() == base.Hash() {
			t.Fatalf("mutation %d did not change catalog hash", index)
		}
	}
}

func TestCatalogRejectsInvalidPrices(t *testing.T) {
	tests := []struct {
		name   string
		target string
		price  PriceConfig
	}{
		{name: "bare target", target: "gpt", price: meteredPrice()},
		{name: "noncanonical backend", target: "local/qwen", price: PriceConfig{BillingMode: BillingLocal}},
		{name: "unknown billing", target: "openai/gpt", price: PriceConfig{BillingMode: BillingUnknown}},
		{name: "invalid billing", target: "openai/gpt", price: PriceConfig{BillingMode: "prepaid"}},
		{name: "missing output", target: "openai/gpt", price: PriceConfig{BillingMode: BillingMetered, InputUSDPerMillion: "1"}},
		{name: "cache without token rates", target: "openai/gpt", price: PriceConfig{BillingMode: BillingMetered, CacheReadUSDPerMillion: "1", OpaqueMaxUSDPerInvocation: "2"}},
		{name: "unbounded metered", target: "openai/gpt", price: PriceConfig{BillingMode: BillingMetered}},
		{name: "local with rate", target: "ollama/qwen", price: PriceConfig{BillingMode: BillingLocal, InputUSDPerMillion: "1", OutputUSDPerMillion: "1"}},
		{name: "subscription with opaque max", target: "codex/gpt", price: PriceConfig{BillingMode: BillingSubscription, OpaqueMaxUSDPerInvocation: "1"}},
		{name: "malformed money", target: "openai/gpt", price: PriceConfig{BillingMode: BillingMetered, InputUSDPerMillion: "1e-3", OutputUSDPerMillion: "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewCatalog(map[string]PriceConfig{tt.target: tt.price}); err == nil {
				t.Fatal("NewCatalog succeeded")
			}
		})
	}
}

func TestPriceConfigYAMLIsStrictAndRequiresStringMoney(t *testing.T) {
	for _, document := range []string{
		"billing-mode: metered\ninput-usd-per-million: 2.0\noutput-usd-per-million: \"8\"\n",
		"billing-mode: metered\ninput-usd-per-million: \"2\"\noutput_usd_per_million: \"8\"\n",
	} {
		var config PriceConfig
		if err := yaml.Unmarshal([]byte(document), &config); err == nil {
			t.Fatalf("yaml.Unmarshal accepted:\n%s", document)
		}
	}
}
