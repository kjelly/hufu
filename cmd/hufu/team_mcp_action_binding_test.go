package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

const mcpActionBindingTeamManifest = `name: mcp-action
mcp-servers:
  diagnostics:
    type: local
    command: [/nonexistent/hufu-test-mcp-server]
action-providers:
  diagnostics:
    runtime: mcp
    server: diagnostics
    tool: collect_debug
`

// TestLoadTeamCommonFailsWhenAnMCPActionProviderCannotBind pins the startup
// rule for MCP action providers: a referenced server that fails to load is
// not the ordinary MCP warning. The run does not start, and the manager the
// loader built is closed exactly once.
func TestLoadTeamCommonFailsWhenAnMCPActionProviderCannotBind(t *testing.T) {
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	opts = runOptions{}
	probe := installMCPOwnershipProbe(t, nil)

	dir := t.TempDir()
	files := map[string]string{
		"team.yaml":      mcpActionBindingTeamManifest,
		"coordinator.md": "---\nname: coordinator\nrole: coordinator\ndescription: Coordinates.\n---\nCoordinate.",
		"worker.md":      "---\nname: worker\ndescription: Works.\ntools: view\n---\nWork.",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	session, err := team.LoadTeam(dir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	session.Workspace = t.TempDir()
	session.Config.WorkerModel = "ollama/mcp-ownership-model"
	session.Config.CoordinatorModel = "ollama/mcp-ownership-model"

	tc, err := loadTeamCommon(t.Context(), "mcp-action", session, server.URL+"/v1", "", nil, nil, nil, false, false, true)
	if err == nil {
		_ = tc.Close()
		t.Fatal("loadTeamCommon started a team whose MCP action provider cannot bind")
	}
	if !strings.Contains(err.Error(), "mcp_action_bind_failed") || !strings.Contains(err.Error(), `MCP server "diagnostics" failed to load`) {
		t.Fatalf("loadTeamCommon error = %v", err)
	}
	probe.requireSingleManagerClosed(t, 1)
}
