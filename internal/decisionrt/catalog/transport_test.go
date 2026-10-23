package catalog

import (
	"testing"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
)

func TestRuntimeDefaultsFreezeTransport(t *testing.T) {
	entry := Entry{Version: "1", Kind: decisionrt.KindChoice, Question: "Select the supported value", Options: []Option{{ID: "yes"}, {ID: "no"}}, Inputs: map[string]string{}, Agents: []string{"worker"}}
	defaults := settings.Config{Backend: "systemone", SystemOne: settings.SystemOne{Model: "ollama/clef:cloud", APIKeyEnv: "TEST_DECISION_RUNTIME_KEY"}}.WithSource("global")
	t.Setenv("TEST_DECISION_RUNTIME_KEY", "first-secret")
	first, err := New(map[string]Entry{"test": entry}, WithRuntimeConfig(defaults))
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Transports()["test"]; got.Model != "ollama/clef:cloud" || got.Sources["model"] != "global" {
		t.Fatalf("transport = %#v", got)
	}
	t.Setenv("TEST_DECISION_RUNTIME_KEY", "second-secret")
	second, err := New(map[string]Entry{"test": entry}, WithRuntimeConfig(defaults))
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() == second.Hash() {
		t.Fatal("credential revision did not change catalog hash")
	}
	defaults.SystemOne.Model = "other"
	third, err := New(map[string]Entry{"test": entry}, WithRuntimeConfig(defaults))
	if err != nil {
		t.Fatal(err)
	}
	if third.Hash() == second.Hash() {
		t.Fatal("model change did not change catalog hash")
	}
	entry.Backend = "rule"
	if _, err := New(map[string]Entry{"test": entry}, WithRuntimeConfig(defaults)); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredTransportRequiresFinalValidation(t *testing.T) {
	entry := Entry{Backend: "systemone", Version: "1", Kind: decisionrt.KindBoolean, Question: "Is this valid?", Inputs: map[string]string{}, Agents: []string{"worker"}}
	if _, err := New(map[string]Entry{"test": entry}, ValidationOnly(), AllowInheritedTransport()); err != nil {
		t.Fatal(err)
	}
	if _, err := New(map[string]Entry{"test": entry}); err == nil {
		t.Fatal("unresolved model accepted at runtime")
	}
}
