package team

import (
	"path/filepath"
	"testing"
)

// TestActionProvidersDocExampleLoads keeps the binding example in
// docs/reference/action-providers.md loadable: the same team lives in
// testdata/docs-action-provider-example.
func TestActionProvidersDocExampleLoads(t *testing.T) {
	session, err := LoadTeam(filepath.Join("testdata", "docs-action-provider-example"), nil, nil, nil)
	if err != nil {
		t.Fatalf("LoadTeam(doc example): %v", err)
	}
	if name := session.ProviderRegistry.ProviderName("prepare-workset"); name == "" || name == "golang" {
		t.Fatalf("prepare-workset provider = %q, want a prepared golang program", name)
	}
	var action *Action
	for _, task := range session.ContractTasks {
		if task.ID == "prepare-workset" {
			action = task.Action
		}
	}
	if action == nil || action.Capability != "prepare-workset" || action.Type != "prepare" {
		t.Fatalf("prepare-workset action = %#v", action)
	}
}

// TestActionCatalogDocExamplesLoad keeps the action catalog examples in
// docs/reference/action-providers.md loadable.
func TestActionCatalogDocExamplesLoad(t *testing.T) {
	for _, tt := range []struct {
		dir     string
		entries []string
	}{
		{dir: "docs-action-catalog-dynamic", entries: []string{"collect-debug-bundle", "rotate-service-logs"}},
		{dir: "docs-action-catalog-workflow", entries: []string{"check-release-window"}},
	} {
		session, err := LoadTeam(filepath.Join("testdata", tt.dir), nil, nil, nil)
		if err != nil {
			t.Fatalf("LoadTeam(%s): %v", tt.dir, err)
		}
		for _, id := range tt.entries {
			if _, ok := session.ActionCatalog.Lookup(id); !ok {
				t.Fatalf("%s: catalog lacks %q", tt.dir, id)
			}
		}
		if len(session.ActionCatalog.Entries) != len(tt.entries) {
			t.Fatalf("%s: catalog has %d entries, want %d", tt.dir, len(session.ActionCatalog.Entries), len(tt.entries))
		}
	}
}
