package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/cost"
)

func TestCostCatalogMergesByCompleteTargetEntry(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home.yaml")
	project := filepath.Join(dir, "project.yaml")
	writeConfig := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(home, `cost:
  prices:
    openai/gpt:
      billing-mode: metered
      input-usd-per-million: "2"
      output-usd-per-million: "8"
    ollama/qwen:
      billing-mode: local
`)
	writeConfig(project, `cost:
  prices:
    openai/gpt:
      billing-mode: subscription
    codex/gpt:
      billing-mode: metered
      opaque-max-usd-per-invocation: "0.10"
`)
	cfg := &Config{}
	cfg.mergeFromFile(home)
	cfg.mergeFromFile(project)
	catalog, err := cfg.Cost.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Len() != 3 {
		t.Fatalf("catalog length = %d, want 3", catalog.Len())
	}
	overridden, _ := catalog.Resolve("openai/gpt")
	if overridden.BillingMode != cost.BillingSubscription || overridden.InputMicrosPerMillion != nil {
		t.Fatalf("project entry did not replace complete home entry: %#v", overridden)
	}
	if _, ok := catalog.Resolve("ollama/qwen"); !ok {
		t.Fatal("unrelated home entry was not preserved")
	}
}

func TestCostConfigRejectsUnknownKeysAndNumericMoney(t *testing.T) {
	tests := []string{
		"cost:\n  pricez: {}\n",
		"cost:\n  prices:\n    openai/gpt:\n      billing-mode: metered\n      input-usd-per-million: 2.0\n      output-usd-per-million: \"8\"\n",
		"cost:\n  prices:\n    openai/gpt:\n      billing-mode: metered\n      input-usd-per-million: \"2\"\n      output-usd-per-millon: \"8\"\n",
	}
	for index, document := range tests {
		path := filepath.Join(t.TempDir(), "hufu.yaml")
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := &Config{Model: "unchanged"}
		cfg.mergeFromFile(path)
		if len(cfg.Cost.Prices) != 0 {
			t.Fatalf("case %d accepted invalid cost config: %#v", index, cfg.Cost)
		}
	}
}
