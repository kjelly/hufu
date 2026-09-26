package team

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/mcp"
)

func TestBindMCPActionProvidersAtCoordinatorStartup(t *testing.T) {
	rawTool := func(schema string) mcpgo.Tool {
		return mcpgo.NewToolWithRawSchema("collect_debug", "Collect diagnostics.", json.RawMessage(schema))
	}
	tests := []struct {
		name    string
		manager func(t *testing.T) *mcp.MCPToolManager
		wantErr string
	}{
		{name: "bound", manager: func(t *testing.T) *mcp.MCPToolManager {
			return newMCPActionManager(t, newMCPActionFake(collectDebugTool("v1")))
		}},
		{name: "no manager", manager: func(*testing.T) *mcp.MCPToolManager { return nil }, wantErr: "no MCP manager is loaded"},
		{name: "referenced server failed to load", manager: func(t *testing.T) *mcp.MCPToolManager {
			manager := mcp.NewMCPToolManager("", "")
			_ = manager.LoadTools(t.Context(), map[string]mcp.MCPServerConfig{"diagnostics": {Type: "bogus"}})
			return manager
		}, wantErr: `MCP server "diagnostics" failed to load`},
		{name: "tool excluded", manager: func(t *testing.T) *mcp.MCPToolManager {
			manager := mcp.NewMCPToolManager("", "")
			attachMCPActionFake(t, manager, "diagnostics", mcp.MCPServerConfig{ExcludedTools: []string{"collect_debug"}}, newMCPActionFake(collectDebugTool("v1")))
			t.Cleanup(func() { _ = manager.Close() })
			return manager
		}, wantErr: "does not expose tool"},
		{name: "invalid input schema", manager: func(t *testing.T) *mcp.MCPToolManager {
			return newMCPActionManager(t, newMCPActionFake(rawTool(`{"type":"object","properties":{"service":{"type":"bogus"}}}`)))
		}, wantErr: "compile input schema"},
		{name: "draft-07 input schema", manager: func(t *testing.T) *mcp.MCPToolManager {
			return newMCPActionManager(t, newMCPActionFake(rawTool(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"service":{"type":"string","minLength":1}},"required":["service"]}`)))
		}},
		{name: "unrelated server failed to load", manager: func(t *testing.T) *mcp.MCPToolManager {
			manager := newMCPActionManager(t, newMCPActionFake(collectDebugTool("v1")))
			_ = manager.LoadTools(t.Context(), map[string]mcp.MCPServerConfig{"other": {Type: "bogus"}})
			return manager
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("CODEX_HOME", t.TempDir())
			manager := tt.manager(t)
			teamDir := writeMCPActionCatalogTeam(t, mcpActionTestManifest+actionCatalogTestEntry)
			c, err := newMCPActionCoordinator(t, t.TempDir(), teamDir, manager)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), mcpActionBindFailed) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewCoordinator error = %v, want %s with %q", err, mcpActionBindFailed, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewCoordinator: %v", err)
			}
			reserved, err := manager.ReserveRuntimeTool("diagnostics", "collect_debug")
			if err != nil {
				t.Fatal(err)
			}
			digest, err := mcp.MCPToolDescriptorSHA256(reserved)
			if err != nil {
				t.Fatal(err)
			}
			pins := c.ExecutionPolicySnapshot().MCPActionProviders
			want := ExecutionMCPActionProviderSnapshot{
				Capability: "diagnostics", Server: "diagnostics", Tool: "collect_debug",
				ServerConfigHash: pins[0].ServerConfigHash, DescriptorSHA256: digest,
			}
			if len(pins) != 1 || pins[0] != want || !strings.HasPrefix(want.ServerConfigHash, "sha256:") {
				t.Fatalf("snapshot MCP providers = %#v, want %#v", pins, want)
			}
			for _, descriptor := range manager.SnapshotToolDescriptors() {
				if descriptor.Name == "diagnostics__collect_debug" {
					t.Fatal("the bound tool is still on the model-facing surface")
				}
			}
		})
	}
}

func TestExecutionPolicySnapshotOmitsMCPProvidersForOtherTeams(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	teamDir := writeActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	c := newActionCatalogCoordinator(t, t.TempDir(), teamDir, nil)
	encoded, err := json.Marshal(c.ExecutionPolicySnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "mcp_action_providers") {
		t.Fatalf("snapshot of a team without MCP providers = %s", encoded)
	}
}

func TestChangedMCPActionTargetFailsResumeClosed(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	// The server command must not change between restarts: it is part of the
	// pinned server configuration.
	manifest := strings.Replace(mcpActionTestManifest, "MARKER", filepath.Join(workspace, "started"), 1) + actionCatalogTestEntry
	start := func(manifest, description string) *Coordinator {
		teamDir := writeMCPActionCatalogTeam(t, manifest)
		c, err := newMCPActionCoordinator(t, workspace, teamDir, newMCPActionManager(t, newMCPActionFake(collectDebugTool(description))))
		if err != nil {
			t.Fatalf("NewCoordinator: %v", err)
		}
		if session := LoadSession(c.session.Workspace); session != nil {
			c.SetSessionData(session)
		}
		c.initEventStore()
		return c
	}
	first := start(manifest, "v1")
	if err := first.checkRunAdmission(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	closeWorkerModelResumeStore(t, first)

	for _, tt := range []struct {
		name, manifest, description string
	}{
		{name: "changed descriptor", manifest: manifest, description: "v2"},
		{name: "changed server configuration", manifest: strings.Replace(manifest, "allowedTools: [collect_debug]", "allowedTools: [collect_debug, restart]", 1), description: "v1"},
	} {
		changed := start(tt.manifest, tt.description)
		if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
			t.Fatalf("%s: resume error = %v, want policy snapshot drift", tt.name, err)
		}
		closeWorkerModelResumeStore(t, changed)
	}

	same := start(manifest, "v1")
	if err := same.checkRunAdmission(); err != nil {
		t.Fatalf("resume with an unchanged MCP target: %v", err)
	}
	closeWorkerModelResumeStore(t, same)
}

func TestLegacySnapshotCannotResumeMCPActionProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	teamDir := writeMCPActionCatalogTeam(t, mcpActionTestManifest+actionCatalogTestEntry)
	c, err := newMCPActionCoordinator(t, t.TempDir(), teamDir, newMCPActionManager(t, newMCPActionFake(collectDebugTool("v1"))))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := newExecutionPolicyStateForVersion(c, executionPolicyLegacySnapshotVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy.snapshot.MCPActionProviders) != 0 {
		t.Fatalf("v3 snapshot pins MCP providers: %#v", legacy.snapshot.MCPActionProviders)
	}
	if _, err := c.executionPolicySnapshotMatchesCurrent(legacy.snapshot); err == nil || !strings.Contains(err.Error(), "v3 cannot pin MCP action providers") {
		t.Fatalf("v3 resume error = %v", err)
	}
}

func TestValidateExecutionPolicyMCPActionProviders(t *testing.T) {
	entry := func(capability string) ExecutionMCPActionProviderSnapshot {
		return ExecutionMCPActionProviderSnapshot{Capability: capability, Server: "s", Tool: "t", ServerConfigHash: "sha256:x", DescriptorSHA256: "d"}
	}
	unbound := entry("b")
	unbound.DescriptorSHA256 = ""
	tests := []struct {
		name    string
		version int
		entries []ExecutionMCPActionProviderSnapshot
		wantErr string
	}{
		{name: "none", version: executionPolicyLegacySnapshotVersion},
		{name: "ordered", version: executionPolicySnapshotVersion, entries: []ExecutionMCPActionProviderSnapshot{entry("a"), entry("b")}},
		{name: "unbound", version: executionPolicySnapshotVersion, entries: []ExecutionMCPActionProviderSnapshot{entry("a"), unbound}, wantErr: "incomplete"},
		{name: "unordered", version: executionPolicySnapshotVersion, entries: []ExecutionMCPActionProviderSnapshot{entry("b"), entry("a")}, wantErr: "strict capability order"},
		{name: "duplicate", version: executionPolicySnapshotVersion, entries: []ExecutionMCPActionProviderSnapshot{entry("a"), entry("a")}, wantErr: "strict capability order"},
		{name: "legacy version", version: executionPolicyLegacySnapshotVersion, entries: []ExecutionMCPActionProviderSnapshot{entry("a")}, wantErr: "cannot pin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExecutionPolicyMCPActionProviders(tt.version, tt.entries)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDryRunCoordinatorNeedsNoMCPManager(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	session, err := LoadTeam(writeMCPActionCatalogTeam(t, mcpActionTestManifest+actionCatalogTestEntry), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewDryRunCoordinator(DryRunCoordinatorParams{Session: session})
	if err != nil {
		t.Fatalf("NewDryRunCoordinator: %v", err)
	}
	pins := c.ExecutionPolicySnapshot().MCPActionProviders
	if len(pins) != 1 || pins[0].DescriptorSHA256 != "" || pins[0].Tool != "collect_debug" {
		t.Fatalf("dry-run MCP providers = %#v", pins)
	}
}
