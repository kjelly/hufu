package team

import (
	"context"
	"slices"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/mcp"
)

// newMCPIsolationCoordinator binds the catalog team's MCP provider to a
// server that also exposes an unbound "list" tool.
func newMCPIsolationCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	fake := newMCPActionFake(collectDebugTool("v1"))
	fake.server.AddTool(mcpgo.NewTool("list", mcpgo.WithDescription("List services.")), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("api"), nil
	})
	manager := mcp.NewMCPToolManager("", "")
	attachMCPActionFake(t, manager, "diagnostics", mcp.MCPServerConfig{}, fake)
	t.Cleanup(func() { _ = manager.Close() })
	c, err := newMCPActionCoordinator(t, t.TempDir(), writeMCPActionCatalogTeam(t, mcpActionTestManifest+actionCatalogTestEntry), manager)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return c
}

func TestBoundMCPToolLeavesWorkerToolSurfaces(t *testing.T) {
	c := newMCPIsolationCoordinator(t)
	def := c.session.Agents["critic"]
	def.Tools = "all"
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic", Desc: "review"}})[0]
	task := TaskDef{Agent: "critic", Goal: "review"}

	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{Task: task, TodoID: item.ID})
	if err != nil {
		t.Fatalf("ResolveTaskTools: %v", err)
	}
	if slices.Contains(resolved.Names, "diagnostics__collect_debug") || !slices.Contains(resolved.Names, "diagnostics__list") {
		t.Fatalf("worker tools = %v, want the unbound tool without the reserved one", resolved.Names)
	}
	for _, tool := range resolved.Tools {
		if tool.Info().Name == "diagnostics__collect_debug" {
			t.Fatal("worker received a handler for the reserved tool")
		}
	}

	snapshot, err := c.resolveNewTaskToolAuthorization(t.Context(), task, def)
	if err != nil {
		t.Fatalf("resolveNewTaskToolAuthorization: %v", err)
	}
	var targets []string
	for _, target := range snapshot.Targets {
		targets = append(targets, target.Name)
	}
	if slices.Contains(targets, "diagnostics__collect_debug") || !slices.Contains(targets, "diagnostics__list") {
		t.Fatalf("dynamic authorization targets = %v, want the unbound tool without the reserved one", targets)
	}
}

func TestWorkerCatalogViewsHideTheMCPTarget(t *testing.T) {
	c := newMCPIsolationCoordinator(t)
	ctx := workerTeamActionContext("1", "runtime-engineer")
	list := &teamActionListTool{coordinator: c, todoID: "1", agent: "runtime-engineer"}
	get := &teamActionGetTool{coordinator: c, todoID: "1", agent: "runtime-engineer"}
	for name, run := range map[string]func() (string, bool){
		"list": func() (string, bool) { return runTeamActionTool(t, list, ctx, `{"query":""}`) },
		"get":  func() (string, bool) { return runTeamActionTool(t, get, ctx, `{"action":"collect-debug-bundle"}`) },
	} {
		content, isError := run()
		if isError || !strings.Contains(content, "collect-debug-bundle") {
			t.Fatalf("%s = %s (error %v)", name, content, isError)
		}
		for _, leak := range []string{"collect_debug", "diagnostics__", "mcp:", `"server"`, `"tool"`, `"capability"`, "descriptor"} {
			if strings.Contains(content, leak) {
				t.Fatalf("worker %s leaks %q: %s", name, leak, content)
			}
		}
	}
}
