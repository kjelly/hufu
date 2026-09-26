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

// TestMCPActionProviderDocExamplesLoad keeps the MCP provider examples in
// docs/reference/action-providers.md loadable and lint-clean without starting
// their MCP servers.
func TestMCPActionProviderDocExamplesLoad(t *testing.T) {
	for _, dir := range []string{"docs-mcp-action-catalog", "docs-mcp-action-workflow"} {
		t.Run(dir, func(t *testing.T) {
			path := filepath.Join("testdata", dir)
			session, err := LoadTeam(path, nil, nil, nil)
			if err != nil {
				t.Fatalf("LoadTeam: %v", err)
			}
			if name := session.ProviderRegistry.ProviderName("diagnostics"); name != "mcp:diagnostics/collect_debug" {
				t.Fatalf("diagnostics provider = %q", name)
			}
			result, err := LintTeam(path, nil, nil, nil)
			if err != nil {
				t.Fatalf("LintTeam: %v", err)
			}
			for _, finding := range result.Findings {
				if finding.Severity == FindingSeverityError {
					t.Fatalf("lint error %s: %s", finding.Code, finding.Message)
				}
			}
			switch dir {
			case "docs-mcp-action-catalog":
				if _, ok := session.ActionCatalog.Lookup("collect-debug"); !ok {
					t.Fatal("catalog lacks collect-debug")
				}
			case "docs-mcp-action-workflow":
				var action *Action
				for _, task := range session.ContractTasks {
					if task.ID == "collect-debug" {
						action = task.Action
					}
				}
				if action == nil || action.Capability != "diagnostics" || action.Payload != `{"service":"api"}` {
					t.Fatalf("collect-debug action = %#v", action)
				}
			}
		})
	}
}
