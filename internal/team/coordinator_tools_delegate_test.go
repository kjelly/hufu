package team

import (
	"context"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
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

func TestRequestAgentToolUsesTypedResultWorkerSurface(t *testing.T) {
	modelID := "request-agent-worker-surface-test"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, modelID, "sub-agent")
	c.taskResultCache = make(map[string][]cachedTaskEntry)
	if err := c.startProviderExecutionBoundary(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer c.stopProviderExecutionBoundary()

	response, err := (&requestAgentTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{
		Name:  "request_agent",
		Input: `{"goal":"perform delegated work","agent":"worker"}`,
	})
	if err != nil || response.IsError {
		t.Fatalf("request_agent response=%#v err=%v", response, err)
	}

	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("nested task items=%#v, want one task", items)
	}
	item := items[0]
	if item.Agent != def.Name || item.Source != TaskSourceSubagent || !item.Execution.RequiresResult {
		t.Fatalf("nested task=%#v, want selected worker, subagent source, and required result", item)
	}
	if item.Status != TaskDone {
		t.Fatalf("nested task status=%s, want %s", item.Status, TaskDone)
	}
	result := c.GetTaskResult(item.ID)
	if result == nil || result.Status != TaskResultStatusSuccess || result.Summary != "direct provider result" {
		t.Fatalf("nested typed result=%#v, want submitted success", result)
	}
	if !strings.Contains(response.Content, result.Summary) {
		t.Fatalf("request_agent response=%q does not contain typed result summary %q", response.Content, result.Summary)
	}

	requests := capture.providerRequests()
	if len(requests) == 0 {
		t.Fatal("nested worker made no provider request")
	}
	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: taskDefFromTodoItem(item), TodoID: item.ID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(requests[0], resolved.Names) {
		t.Fatalf("nested provider tools=%v, want=%v", requests[0], resolved.Names)
	}
	for _, forbidden := range []string{"agent", "finish", "request_agent", "approve_plan"} {
		if slices.Contains(requests[0], forbidden) {
			t.Fatalf("nested provider tools=%v contains coordinator-only tool %q", requests[0], forbidden)
		}
	}
}

func TestExecuteSubAgentFailsClosedWithoutCanonicalTodo(t *testing.T) {
	c := newDelegateTestCoordinator(map[string]*agent.AgentDef{
		"worker": {Name: "worker", Role: "worker"},
	})
	c.taskTracker = NewTaskTracker()

	if _, err := c.ExecuteSubAgent(context.Background(), "worker", "goal", ""); err == nil || !strings.Contains(err.Error(), "Todo ID is required") {
		t.Fatalf("missing Todo ID error=%v, want fail-closed identity error", err)
	}
	invalidTodo := context.WithValue(context.Background(), todoIDKey{}, "missing")
	if _, err := c.ExecuteSubAgent(invalidTodo, "worker", "goal", ""); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("invalid Todo ID error=%v, want canonical Todo error", err)
	}
}
