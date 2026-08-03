package team

import (
	"context"
	"slices"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
)

// TestWithEffectiveToolsAllowed_DefaultHelperBashPreservesRuntimePermissions
// catches the fast-path regression where a Helper exposed its declared tools
// to the model but never received them in the runtime permission context.
func TestWithEffectiveToolsAllowed_DefaultHelperBashPreservesRuntimePermissions(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "bash")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}

	ctx := (&Coordinator{session: session}).withEffectiveToolsAllowed(context.Background(), session.Agents["helper"])
	allowed := tools.GetToolsAllowed(ctx)
	for _, want := range []string{"view", "bash", "wait_for"} {
		if !slices.Contains(allowed, want) {
			t.Fatalf("runtime allowlist = %v, missing %q", allowed, want)
		}
	}
}

func TestWithEffectiveToolsAllowed_IncludesAgentSpecificMCPTools(t *testing.T) {
	session := &TeamSession{
		Config: agent.TeamConfig{Name: "team"},
		Agents: map[string]*agent.AgentDef{
			"helper": {Name: "helper", MCPTools: map[string]agent.MCPToolConfig{
				"run-tests": {Cmd: "go test ./..."},
			}},
		},
	}
	ctx := (&Coordinator{session: session}).withEffectiveToolsAllowed(context.Background(), session.Agents["helper"])
	allowed := tools.GetToolsAllowed(ctx)
	for _, want := range []string{"run-tests", "helper:run-tests"} {
		if !slices.Contains(allowed, want) {
			t.Fatalf("runtime allowlist = %v, missing %q", allowed, want)
		}
	}
	decision, err := (&Coordinator{}).authorizeStreamTool(context.Background(), "helper", "run-tests", map[string]bool{"run-tests": true, "helper:run-tests": true})
	if err != nil || decision.Code != DecisionAllow {
		t.Fatalf("agent-specific MCP decision = %#v, err %v", decision, err)
	}
}
