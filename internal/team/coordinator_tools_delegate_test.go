package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func newDelegateTestCoordinator(agents map[string]*agent.AgentDef) *Coordinator {
	return &Coordinator{
		session: &TeamSession{
			Config: agent.TeamConfig{Name: "test"},
			Agents: agents,
		},
	}
}

// A real run's Helper agent once tried to delegate to an agent named "exec",
// which was never a valid worker — an invented name with no enum to catch it
// at the schema level, costing a wasted round trip. The "agent" parameter
// must list the team's actual worker names so most providers steer the model
// away from names that were never valid, and reject them outright when they
// enforce the enum.
func TestRequestAgentToolInfoListsValidAgentsAsEnum(t *testing.T) {
	c := newDelegateTestCoordinator(map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator"},
		"deployer":    {Name: "deployer", Role: "worker"},
		"verifier":    {Name: "verifier", Role: "worker"},
		"helper":      {Name: "helper", Role: "worker"},
	})
	tool := &requestAgentTool{coordinator: c}
	info := tool.Info()

	agentParam, ok := info.Parameters["agent"].(map[string]any)
	if !ok {
		t.Fatalf("expected an 'agent' parameter, got %#v", info.Parameters["agent"])
	}
	enum, ok := agentParam["enum"].([]string)
	if !ok {
		t.Fatalf("expected the 'agent' parameter to carry an enum, got %#v", agentParam["enum"])
	}

	want := map[string]bool{"deployer": true, "verifier": true, "helper": true}
	if len(enum) != len(want) {
		t.Fatalf("enum = %v, want exactly %v", enum, want)
	}
	for _, name := range enum {
		if !want[name] {
			t.Errorf("enum contains unexpected agent %q", name)
		}
	}
	// The coordinator itself is never a valid delegation target.
	for _, name := range enum {
		if name == "coordinator" {
			t.Errorf("enum must not list the coordinator as a delegation target: %v", enum)
		}
	}
}

func TestRequestAgentToolInfoOmitsEnumWithNoWorkers(t *testing.T) {
	c := newDelegateTestCoordinator(map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator"},
	})
	tool := &requestAgentTool{coordinator: c}
	info := tool.Info()

	agentParam, ok := info.Parameters["agent"].(map[string]any)
	if !ok {
		t.Fatalf("expected an 'agent' parameter, got %#v", info.Parameters["agent"])
	}
	if _, hasEnum := agentParam["enum"]; hasEnum {
		t.Errorf("expected no enum when there are no workers, got %#v", agentParam["enum"])
	}
}
