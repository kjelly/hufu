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
