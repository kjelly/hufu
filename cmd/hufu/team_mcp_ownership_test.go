package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/team"
)

// mcpOwnershipProbe records every manager the team loader builds and how
// often each one is closed.
type mcpOwnershipProbe struct {
	mu       sync.Mutex
	built    []*mcp.MCPToolManager
	closes   map[*mcp.MCPToolManager]int
	closeErr error
}

// installMCPOwnershipProbe replaces the MCP ownership seam for one test.
// A nil build keeps the production builder, so real partial-load behavior
// stays under test.
func installMCPOwnershipProbe(t *testing.T, build func(context.Context, *team.TeamSession, *config.Config) *mcp.MCPToolManager) *mcpOwnershipProbe {
	t.Helper()
	originalBuild, originalClose := buildTeamMCPManager, closeTeamMCPManager
	t.Cleanup(func() { buildTeamMCPManager, closeTeamMCPManager = originalBuild, originalClose })
	if build == nil {
		build = originalBuild
	}
	probe := &mcpOwnershipProbe{closes: make(map[*mcp.MCPToolManager]int)}
	buildTeamMCPManager = func(ctx context.Context, session *team.TeamSession, cfg *config.Config) *mcp.MCPToolManager {
		manager := build(ctx, session, cfg)
		probe.mu.Lock()
		probe.built = append(probe.built, manager)
		probe.mu.Unlock()
		return manager
	}
	closeTeamMCPManager = func(manager *mcp.MCPToolManager) error {
		if manager == nil {
			return nil
		}
		probe.mu.Lock()
		probe.closes[manager]++
		err := probe.closeErr
		probe.mu.Unlock()
		return errors.Join(err, originalClose(manager))
	}
	return probe
}

// requireSingleManagerClosed asserts one non-nil manager was built and was
// closed exactly want times.
func (p *mcpOwnershipProbe) requireSingleManagerClosed(t *testing.T, want int) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.built) != 1 || p.built[0] == nil {
		t.Fatalf("built MCP managers = %v, want exactly one non-nil manager", p.built)
	}
	if got := p.closes[p.built[0]]; got != want {
		t.Fatalf("MCP manager closed %d times, want %d", got, want)
	}
}

func emptyMCPManager(context.Context, *team.TeamSession, *config.Config) *mcp.MCPToolManager {
	return mcp.NewMCPToolManager("bash", "bash")
}

// mcpOwnershipSession returns a minimal team whose provider is a local test
// server, so loadTeamCommon can finish without a real model backend.
func mcpOwnershipSession(t *testing.T, name string) (*team.TeamSession, string) {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	session := &team.TeamSession{
		Dir:       t.TempDir(),
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name:             name,
			WorkerModel:      "ollama/mcp-ownership-model",
			CoordinatorModel: "ollama/mcp-ownership-model",
		},
		Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "ollama/mcp-ownership-model"}},
		},
	}
	return session, server.URL + "/v1"
}

func TestLoadTeamCommonClosesMCPManagerExactlyOnce(t *testing.T) {
	tests := []struct {
		name string
		// build is the MCP builder under test; nil keeps the production one.
		build func(context.Context, *team.TeamSession, *config.Config) *mcp.MCPToolManager
		// prepare shapes the session so loadTeamCommon fails at one stage.
		prepare func(t *testing.T, session *team.TeamSession)
		wantErr string
	}{
		{
			name:  "coordinator constructor fails",
			build: emptyMCPManager,
			prepare: func(_ *testing.T, session *team.TeamSession) {
				session.Config.GoalMode = "not-a-goal-mode"
			},
			wantErr: "failed to create coordinator",
		},
		{
			name:    "failure after the coordinator was built",
			build:   emptyMCPManager,
			prepare: saveInterruptedLegacyCheckpoint,
			wantErr: "legacy session has interrupted tasks",
		},
		{
			name: "partial MCP load failure then startup failure",
			prepare: func(t *testing.T, session *team.TeamSession) {
				session.MCPServers = map[string]mcp.MCPServerConfig{
					"missing": {Type: "local", Command: []string{"/nonexistent/hufu-test-mcp-server"}},
				}
				saveInterruptedLegacyCheckpoint(t, session)
			},
			wantErr: "legacy session has interrupted tasks",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalOpts := opts
			t.Cleanup(func() { opts = originalOpts })
			opts = runOptions{}
			probe := installMCPOwnershipProbe(t, tt.build)
			session, providerURL := mcpOwnershipSession(t, "mcp-ownership")
			tt.prepare(t, session)

			tc, err := loadTeamCommon(t.Context(), "mcp-ownership", session, providerURL, "", nil, nil, nil, false, false, true)
			if err == nil {
				_ = tc.Close()
				t.Fatalf("loadTeamCommon succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("loadTeamCommon error = %v, want %q", err, tt.wantErr)
			}
			probe.requireSingleManagerClosed(t, 1)
		})
	}
}

func TestLoadTeamCommonHandsMCPManagerToTeamContext(t *testing.T) {
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	opts = runOptions{}
	// The production builder keeps a failed optional server a warning: the
	// team still starts and the context owns the manager it returned.
	probe := installMCPOwnershipProbe(t, nil)
	session, providerURL := mcpOwnershipSession(t, "mcp-handoff")
	session.MCPServers = map[string]mcp.MCPServerConfig{
		"missing": {Type: "local", Command: []string{"/nonexistent/hufu-test-mcp-server"}},
	}

	tc, err := loadTeamCommon(t.Context(), "mcp-handoff", session, providerURL, "", nil, nil, nil, false, false, true)
	if err != nil {
		t.Fatalf("loadTeamCommon error = %v", err)
	}
	probe.requireSingleManagerClosed(t, 0)
	if tc.mcpManager != probe.built[0] {
		t.Fatal("teamContext does not own the manager loadTeamCommon built")
	}
	if err := tc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	probe.requireSingleManagerClosed(t, 1)
	if err := tc.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	probe.requireSingleManagerClosed(t, 1)
}

func TestLoadTeamCommonWithoutMCPBuildsNoManager(t *testing.T) {
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })
	opts = runOptions{}
	probe := installMCPOwnershipProbe(t, emptyMCPManager)
	session, providerURL := mcpOwnershipSession(t, "no-mcp")

	tc, err := loadTeamCommon(t.Context(), "no-mcp", session, providerURL, "", nil, nil, nil, false, false, false)
	if err != nil {
		t.Fatalf("loadTeamCommon error = %v", err)
	}
	if err := tc.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(probe.built) != 0 || tc.mcpManager != nil {
		t.Fatalf("built = %v, context manager = %v; the default team must not build MCP", probe.built, tc.mcpManager)
	}
}

// countingLease is a workspace lease whose Close is observable.
type countingLease struct {
	closes int
	err    error
}

func (l *countingLease) Close() error {
	l.closes++
	return l.err
}

func TestTeamContextCloseRunsEveryStepAndJoinsErrors(t *testing.T) {
	probe := installMCPOwnershipProbe(t, emptyMCPManager)
	probe.closeErr = errors.New("mcp close failed")
	manager := buildTeamMCPManager(t.Context(), nil, nil)
	session := &team.TeamSession{Dir: t.TempDir(), Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "close-order"}}
	coordinator, err := team.NewCoordinator(session, "", "", manager, nil, nil, team.RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator error = %v", err)
	}
	lease := &countingLease{err: errors.New("lease close failed")}
	session.WorkspaceLease = lease
	tc := &teamContext{session: session, coordinator: coordinator, mcpManager: manager}

	err = tc.Close()
	for _, want := range []string{"mcp close failed", "lease close failed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Close() error = %v, want it to contain %q", err, want)
		}
	}
	probe.requireSingleManagerClosed(t, 1)
	if lease.closes != 1 {
		t.Fatalf("workspace lease closed %d times, want 1 even though MCP close failed", lease.closes)
	}
}

// saveInterruptedLegacyCheckpoint makes startup fail after NewCoordinator:
// freezing the execution policy rejects a legacy interrupted session.
func saveInterruptedLegacyCheckpoint(t *testing.T, session *team.TeamSession) {
	t.Helper()
	checkpoint := team.NewSession()
	checkpoint.Tasks = []*team.TodoItem{{ID: "legacy-task", Agent: "worker", Desc: "interrupted", Model: "mcp-ownership-model", Status: team.TaskInProgress}}
	if err := team.SaveSession(session.Workspace, checkpoint); err != nil {
		t.Fatal(err)
	}
}
