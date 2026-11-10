package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
)

func TestChromeCDPPolicyPinsBrowserSettingsAcrossResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	workspace := t.TempDir()
	start := func(endpoint string, dwell *mcp.ChromePageDwellConfig, verification bool) *Coordinator {
		c := newExecutionPolicySnapshotCoordinator(t, workspace, 2, 3)
		cfg := mcp.MCPServerConfig{Type: "chrome-cdp", URL: endpoint, PageDwell: dwell, PageVerification: verification, AllowedTools: []string{"browser_snapshot", "browser_wait_for_user"}, ToolPolicies: map[string]mcp.WorkerToolPolicy{
			"browser_snapshot": {SideEffect: "none", Filesystem: "none", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}},
		}}
		c.session.MCPServers = map[string]mcp.MCPServerConfig{"chrome": cfg}
		if err := c.rebuildExecutionPolicyStateForTest(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	first := start("http://127.0.0.1:9222", &mcp.ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 600}, false)
	first.initEventStore()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	closeWorkerModelResumeStore(t, first)
	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.ExecutionPolicySnapshot.MCPWorkerPolicies) != 1 {
		t.Fatal("missing native browser policy checkpoint")
	}
	for _, scenario := range []string{"same", "endpoint", "min_seconds", "max_seconds", "disabled", "verification"} {
		t.Run(scenario, func(t *testing.T) {
			endpoint := "http://127.0.0.1:9222"
			dwell := &mcp.ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 600}
			switch scenario {
			case "endpoint":
				endpoint = "http://127.0.0.1:9223"
			case "min_seconds":
				dwell.MinSeconds = 301
			case "max_seconds":
				dwell.MaxSeconds = 601
			case "disabled":
				dwell = nil
			}
			second := start(endpoint, dwell, scenario == "verification")
			second.SetSessionData(checkpoint)
			matches, err := second.executionPolicySnapshotMatchesCurrent(checkpoint.ExecutionPolicySnapshot)
			if err != nil || matches != (scenario == "same") {
				t.Fatalf("matches=%t error=%v", matches, err)
			}
		})
	}
	// Runtime actions use the same native backend and retain server identity.
	if _, err := newMCPActionProvider("browser.read", agent.ActionProviderConfig{Server: "chrome", Tool: "browser_snapshot"}, first.session.MCPServers); err != nil {
		t.Fatal(err)
	}
}
