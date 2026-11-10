package team

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func dependencyViewManifestFixture(t *testing.T) ContextInjectionManifest {
	t.Helper()
	result, source := dependencyViewRenderFixture()
	view, err := buildDependencyResultView(&DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary", "verification"}}, []TaskResult{result}, []DependencyResultViewSource{source})
	if err != nil {
		t.Fatal(err)
	}
	request := ContextRequest{RequestID: "request", RunID: "consumer-run", TaskID: "consumer", Attempt: 1, AgentRole: "worker", Trigger: ContextTriggerTaskDispatch, Purpose: contextPurposeForTrigger(ContextTriggerTaskDispatch)}
	compiled := CompiledContext{IncludedItems: []ContextItem{{ID: "dependency_results", Kind: "dependency_result", Content: view.content, Required: true, DependencyResultView: view.attribution.Clone()}}}
	manifest, err := BuildContextInjectionManifest(request, compiled, nil, "worker", time.Unix(100, 0), agent.MemoryLearningPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Items[0].ContentHash != dependencyViewDigest([]byte(view.content)) {
		t.Fatal("manifest did not hash the complete selected context item")
	}
	return manifest
}

func TestDependencyResultViewManifestVersionAndQueryComposition(t *testing.T) {
	manifest := dependencyViewManifestFixture(t)
	if manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion || !supportedDeliveryContextManifest(manifest) {
		t.Fatal("selected view did not receive a validated schema4 manifest")
	}
	before, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	plan, sources, row := querySelectionFixture()
	selection, err := SelectContextQuerySources(t.Context(), &querySelectionRepo{rows: []contextstore.SearchResult{row}}, plan, sources, stableQueryRevision)
	if err != nil {
		t.Fatal(err)
	}
	composed, err := WithContextQueryAttribution(manifest, selection.Attribution)
	if err != nil || composed.SchemaVersion != DependencyResultViewManifestSchemaVersion || composed.Query == nil || !supportedDeliveryContextManifest(composed) {
		t.Fatalf("query attribution displaced selected provenance: %v", err)
	}
	after, err := json.Marshal(manifest)
	if err != nil || string(before) != string(after) {
		t.Fatal("query composition modified the original manifest")
	}
	clone := cloneContextInjectionManifest(&composed)
	clone.Items[0].DependencyResultView.Policy.Fields[0] = "findings"
	clone.Items[0].DependencyResultView.Sources[0].BranchID = "other"
	if err := ValidateContextQueryManifest(composed); err != nil {
		t.Fatalf("cloned provenance aliases the original manifest: %v", err)
	}
	if err := ValidateContextQueryManifest(*clone); err == nil {
		t.Fatal("modified provenance retained a valid fingerprint")
	}
	if _, err := WithContextQueryAttribution(composed, selection.Attribution); err == nil {
		t.Fatal("existing query attribution was overwritten")
	}
	for _, version := range []int{1, ContextManifestSchemaVersion, ContextQueryManifestSchemaVersion} {
		legacy := *cloneContextInjectionManifest(&manifest)
		legacy.SchemaVersion = version
		legacy.Fingerprint = contextManifestFingerprint(legacy)
		if err := ValidateContextQueryManifest(legacy); err == nil {
			t.Fatalf("schema%d ignored selected provenance", version)
		}
	}
}

func TestDependencyResultViewManifestRejectsIncompleteProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ContextInjectionManifest)
	}{
		{"missing attribution", func(m *ContextInjectionManifest) { m.Items[0].DependencyResultView = nil }},
		{"missing item", func(m *ContextInjectionManifest) { m.Items = nil }},
		{"duplicate item", func(m *ContextInjectionManifest) { m.Items = append(m.Items, m.Items[0]) }},
		{"omitted", func(m *ContextInjectionManifest) { m.Items[0].Included = false }},
		{"compressed", func(m *ContextInjectionManifest) { m.Items[0].Compressed = true }},
		{"not required", func(m *ContextInjectionManifest) { m.Items[0].Reason = ContextIncludedRelevant }},
		{"wrong kind", func(m *ContextInjectionManifest) { m.Items[0].Kind = "previous_response" }},
		{"missing content hash", func(m *ContextInjectionManifest) { m.Items[0].ContentHash = "" }},
		{"policy hash", func(m *ContextInjectionManifest) {
			m.Items[0].DependencyResultView.PolicySHA256 = strings.Repeat("c", 64)
		}},
		{"full policy", func(m *ContextInjectionManifest) {
			m.Items[0].DependencyResultView.Policy = DependencyResultViewSpec{Mode: "full"}
		}},
		{"policy order", func(m *ContextInjectionManifest) {
			m.Items[0].DependencyResultView.Policy.Fields = []string{"verification", "summary"}
		}},
		{"missing source", func(m *ContextInjectionManifest) { m.Items[0].DependencyResultView.Sources = nil }},
		{"missing revision", func(m *ContextInjectionManifest) { m.Items[0].DependencyResultView.Sources[0].OccurrenceRevision = 0 }},
		{"missing handoff hash", func(m *ContextInjectionManifest) { m.Items[0].DependencyResultView.Sources[0].FullViewSHA256 = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := dependencyViewManifestFixture(t)
			tc.change(&manifest)
			manifest.Fingerprint = dependencyResultViewManifestFingerprint(manifest)
			if err := ValidateContextQueryManifest(manifest); err == nil {
				t.Fatal("incomplete source/delivery proof was accepted")
			}
		})
	}
}
