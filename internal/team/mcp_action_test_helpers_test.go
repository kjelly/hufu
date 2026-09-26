package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/kjelly/hufu/internal/mcp"
)

// mcpActionFake is an in-process MCP server with one tool whose handler a
// test can replace and whose calls are counted.
type mcpActionFake struct {
	server *mcpserver.MCPServer
	calls  atomic.Int64

	mu      sync.Mutex
	handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}

func collectDebugTool(description string) mcpgo.Tool {
	return mcpgo.NewTool("collect_debug", mcpgo.WithDescription(description), mcpgo.WithString("service", mcpgo.Required()))
}

func newMCPActionFake(tool mcpgo.Tool) *mcpActionFake {
	fake := &mcpActionFake{server: mcpserver.NewMCPServer("fake", "1.0.0", mcpserver.WithToolCapabilities(false))}
	fake.handler = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultStructuredOnly(map[string]any{"summary": "ok"}), nil
	}
	fake.server.AddTool(tool, func(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		fake.calls.Add(1)
		fake.mu.Lock()
		handler := fake.handler
		fake.mu.Unlock()
		return handler(ctx, request)
	})
	return fake
}

func (f *mcpActionFake) setHandler(handler func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
}

// attachMCPActionFake connects fake to manager as server name.
func attachMCPActionFake(t *testing.T, manager *mcp.MCPToolManager, name string, cfg mcp.MCPServerConfig, fake *mcpActionFake) {
	t.Helper()
	cli, err := mcpclient.NewInProcessClient(fake.server)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.AttachClient(t.Context(), name, cfg, cli); err != nil {
		t.Fatalf("AttachClient(%s): %v", name, err)
	}
}

// newMCPActionManager returns a manager serving fake as "diagnostics".
func newMCPActionManager(t *testing.T, fake *mcpActionFake) *mcp.MCPToolManager {
	t.Helper()
	manager := mcp.NewMCPToolManager("", "")
	attachMCPActionFake(t, manager, "diagnostics", mcp.MCPServerConfig{AllowedTools: []string{"collect_debug"}}, fake)
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// writeMCPActionCatalogTeam writes the dynamic catalog team with its catalog
// entry bound to the MCP diagnostics provider.
func writeMCPActionCatalogTeam(t *testing.T, manifest string) string {
	t.Helper()
	manifest = strings.Replace(manifest, "MARKER", filepath.Join(t.TempDir(), "started"), 1)
	return writeActionCatalogTeam(t, manifest)
}

// newMCPActionCoordinator loads teamDir into workspace and builds a
// coordinator on manager, returning the constructor error.
func newMCPActionCoordinator(t *testing.T, workspace, teamDir string, manager *mcp.MCPToolManager) (*Coordinator, error) {
	t.Helper()
	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	session.Workspace = filepath.Join(workspace, "session")
	if err := os.MkdirAll(session.Workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(workspace, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := session.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	c, err := NewCoordinator(session, "", "", manager, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c, nil
}
