package catalog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
)

func testEntry() Entry {
	return Entry{Backend: "rule", Version: "1", Kind: decisionrt.KindChoice, Question: "Choose a category", Options: []Option{{ID: "a"}, {ID: "b"}}, Inputs: map[string]string{"summary": "string"}, Agents: []string{"helper"}}
}

func TestCatalogValidatesTrustedConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Entry)
	}{
		{"backend", func(e *Entry) { e.Backend = "unknown" }},
		{"empty version", func(e *Entry) { e.Version = "" }},
		{"empty question", func(e *Entry) { e.Question = "" }},
		{"duplicate option", func(e *Entry) { e.Options[1].ID = "a" }},
		{"unknown input type", func(e *Entry) { e.Inputs["summary"] = "object" }},
		{"missing grants", func(e *Entry) { e.Agents = nil }},
		{"duplicate grants", func(e *Entry) { e.Agents = []string{"helper", "helper"} }},
		{"timeout", func(e *Entry) { e.Timeout = time.Minute }},
		{"confidence", func(e *Entry) { e.MinConfidence = new(1.1) }},
		{"fallback", func(e *Entry) { e.Fallback = "systemone" }},
		{"call limit", func(e *Entry) { e.MaxCalls = -1 }},
		{"invalid credential ref", func(e *Entry) { e.APIKeyEnv = "$KEY" }},
		{"singleton integer", func(e *Entry) {
			e.Backend, e.Model, e.Kind = "systemone", "nimble", decisionrt.KindIntegerRange
			e.Options = nil
			e.Range = &IntegerRange{Min: 1, Max: 1}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := testEntry()
			test.edit(&entry)
			if _, err := New(map[string]Entry{"classify": entry}); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}

func TestCatalogRejectsUndeclaredContextAndUnauthorizedAgents(t *testing.T) {
	service, err := New(map[string]Entry{"classify": testEntry()})
	if err != nil {
		t.Fatal(err)
	}
	for _, context := range []map[string]any{nil, {"summary": true}, {"summary": "text", "endpoint": "http://other"}, {"summary": map[string]any{"nested": "value"}}} {
		if _, _, err := service.Request("classify", "helper", context); err == nil {
			t.Fatalf("accepted context: %#v", context)
		}
	}
	if _, _, err := service.Request("classify", "other", map[string]any{"summary": "text"}); err == nil {
		t.Fatal("unauthorized agent accepted")
	}
	request, limit, err := service.Request("classify", "helper", map[string]any{"summary": "text"})
	if err != nil || limit != 100 {
		t.Fatalf("request: %v, limit %d", err, limit)
	}
	result, receipt, err := service.Decide(t.Context(), "classify", request)
	if err != nil || result.Status != decisionrt.StatusAbstained {
		t.Fatalf("decide: %#v %v", result, err)
	}
	if err := service.ValidatePublication("classify", receipt.RequestDigest, result, receipt); err != nil {
		t.Fatal(err)
	}
	forged := result
	forged.Status, forged.ReasonCode, forged.Value.Choice = decisionrt.StatusDecided, "", "a"
	forgedReceipt := receipt
	forgedReceipt.Status, forgedReceipt.ReasonCode = decisionrt.StatusDecided, ""
	if service.ValidatePublication("classify", receipt.RequestDigest, forged, forgedReceipt) == nil {
		t.Fatal("rule backend cannot publish a decision")
	}
	receipt.SpecVersion = "2"
	if err := service.ValidatePublication("classify", receipt.RequestDigest, result, receipt); err == nil {
		t.Fatal("stale receipt accepted")
	}
}

func TestCatalogFreezesConfigurationAndCredentialRevisions(t *testing.T) {
	t.Setenv("HUFU_CATALOG_TEST_KEY", "catalog-test-key-a")
	entry := testEntry()
	entry.Backend, entry.Model, entry.APIKeyEnv = "systemone", "nimble", "HUFU_CATALOG_TEST_KEY"
	entries := map[string]Entry{"classify": entry}
	service, err := New(entries)
	if err != nil {
		t.Fatal(err)
	}
	entry.Options[0].ID, entry.Inputs["summary"], entry.Agents[0] = "changed", "number", "other"
	request, _, err := service.Request("classify", "helper", map[string]any{"summary": "text"})
	if err != nil || request.Spec.Options[0].ID != "a" {
		t.Fatalf("catalog mutated: %v", err)
	}
	request.Spec.Options[0].ID = "changed"
	request, _, err = service.Request("classify", "helper", map[string]any{"summary": "text"})
	if err != nil || request.Spec.Options[0].ID != "a" {
		t.Fatal("request aliases catalog")
	}
	entry = testEntry()
	entry.Backend, entry.Model, entry.APIKeyEnv = "systemone", "nimble", "HUFU_CATALOG_TEST_KEY"
	t.Setenv("HUFU_CATALOG_TEST_KEY", "catalog-test-key-b")
	changed, err := New(map[string]Entry{"classify": entry})
	if err != nil {
		t.Fatal(err)
	}
	if service.Hash() == changed.Hash() {
		t.Fatal("credential change did not alter hash")
	}
	encoded, err := json.Marshal(service.Description("helper"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "catalog-test-key") || strings.Contains(string(encoded), "systemone") {
		t.Fatal("transport exposed to model")
	}
}

func TestCatalogBoundsAndFreezesToolDescription(t *testing.T) {
	entry := testEntry()
	entry.Agents = []string{"helper", "reviewer"}
	entries := map[string]Entry{"classify": entry}
	service, err := New(entries)
	if err != nil {
		t.Fatal(err)
	}
	description := service.ToolDescription()
	entry.Inputs["summary"] = "boolean"
	entry.Options[0].Description = "mutated"
	if service.ToolDescription() != description || strings.Count(description, `"question"`) != 1 || strings.Contains(description, DefaultEndpoint) {
		t.Fatal("tool description is mutable, duplicated, or exposes transport")
	}
	large := make(map[string]Entry)
	// Machine IDs are validated independently; use valid ID strings for
	// the descriptor-size boundary so this cannot pass for another reason.
	for i := range 64 {
		entry := testEntry()
		entry.Question = strings.Repeat("q", 4096)
		large["decision-"+strings.Repeat("x", i+1)] = entry
	}
	if _, err := New(large); err == nil || !strings.Contains(err.Error(), "256 KiB") {
		t.Fatalf("oversized tool contract accepted: %v", err)
	}
}
