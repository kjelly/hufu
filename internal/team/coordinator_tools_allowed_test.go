package team

import (
	"context"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
)

type namedCoordinatorTool string

func (t namedCoordinatorTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: string(t)}
}

func (namedCoordinatorTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (namedCoordinatorTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (t namedCoordinatorTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse(""), nil
}

// TestWithEffectiveToolsAllowed_DefaultHelperBashPreservesRuntimePermissions
// catches the fast-path regression where a Helper exposed its declared tools
// to the model but never received them in the runtime permission context.
func TestWithEffectiveToolsAllowed_DefaultHelperBashPreservesRuntimePermissions(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "bash")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}

	toolsForWorker := agent.SelectTools(agent.BuildAllAgentTools(t.TempDir()), session.Agents["helper"].Tools)
	ctx := (&Coordinator{session: session}).withEffectiveToolsAllowed(context.Background(), session.Agents["helper"], agentToolNames(toolsForWorker))
	allowed := tools.GetToolsAllowed(ctx)
	for _, want := range []string{"view", "bash", "wait_for"} {
		if !slices.Contains(allowed, want) {
			t.Fatalf("runtime allowlist = %v, missing %q", allowed, want)
		}
	}
}

func TestWithEffectiveToolsAllowed_IncludesActualAgentSpecificMCPTools(t *testing.T) {
	session := &TeamSession{
		Config: agent.TeamConfig{Name: "team"},
		Agents: map[string]*agent.AgentDef{
			"helper": {Name: "helper", MCPTools: map[string]agent.MCPToolConfig{
				"run-tests": {Cmd: "go test ./..."},
			}},
		},
	}
	actualTools := []fantasy.AgentTool{namedCoordinatorTool("run-tests")}
	ctx := (&Coordinator{session: session}).withEffectiveToolsAllowed(context.Background(), session.Agents["helper"], agentToolNames(actualTools))
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

func TestBuildOrchestratorToolsAreRuntimeAllowed(t *testing.T) {
	allowed := coordinatorAllowedToolNames()
	coreTools := make([]fantasy.AgentTool, 0, len(coordinatorCoreToolNames))
	for name := range coordinatorCoreToolNames {
		coreTools = append(coreTools, namedCoordinatorTool(name))
	}

	for _, forcePlanFirst := range []bool{false, true} {
		c := &Coordinator{
			coreTools:      coreTools,
			forcePlanFirst: forcePlanFirst,
			session:        &TeamSession{Agents: map[string]*agent.AgentDef{}},
		}
		for _, tool := range c.buildOrchestratorTools() {
			if !slices.Contains(allowed, tool.Info().Name) {
				t.Errorf("forcePlanFirst=%t: exposed coordinator tool %q is missing from runtime allowlist %v", forcePlanFirst, tool.Info().Name, allowed)
			}
		}
	}
}

// workerCoordinatorToolNames are the coordinator-supplied tools appended to
// c.coreTools by NewCoordinator, on top of BuildAllAgentTools. SelectTools can
// hand any of them to a worker (alwaysIncludeTools forces several in regardless
// of the declared tool list), so the invariant test must model them.
var workerCoordinatorToolNames = []string{
	"request_agent", "todo", "load_skill", "save_skill", "stm_write", "ltm_update",
	"team_info", "terminal", "terminal_start", "terminal_write", "terminal_read",
	"terminal_wait", "terminal_close", "terminal_list", "terminal_reconcile",
	"reconcile_task", "memory_save", "memory_query",
}

func workerInvariantCoreTools(t *testing.T) []fantasy.AgentTool {
	t.Helper()
	core := agent.BuildAllAgentTools(t.TempDir())
	for _, name := range workerCoordinatorToolNames {
		core = append(core, namedCoordinatorTool(name))
	}
	return core
}

// TestWorkerExposedToolsAreRuntimeAllowed is the worker counterpart to
// TestBuildOrchestratorToolsAreRuntimeAllowed, and guards the invariant whose
// absence deadlocked every worker task: a tool the model is shown must be
// callable.
//
// The stream authorization gate is fail-closed and returns an error from
// OnToolCall, which aborts the whole attempt. So an exposed-but-ungranted tool
// does not restrict the worker — it destroys the attempt as soon as the worker
// accepts the invitation. submit_result is the case that mattered: ExecuteTasks
// marks it mandatory for every non-sidecar task and createTaskAgentWithResultTool
// appends it to the tool set, but it appears in no agent's declared tool list.
func TestWorkerExposedToolsAreRuntimeAllowed(t *testing.T) {
	core := workerInvariantCoreTools(t)

	for _, tc := range []struct {
		name        string
		helperTools string
	}{
		{name: "read-only baseline", helperTools: ""},
		{name: "bash", helperTools: "bash"},
		{name: "bash and terminal", helperTools: "bash,terminal"},
		{name: "all tools", helperTools: "all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := LoadDefaultTeam(t.TempDir(), nil, tc.helperTools)
			if err != nil {
				t.Fatalf("LoadDefaultTeam: %v", err)
			}
			c := &Coordinator{session: session, coreTools: core}

			for defName, def := range session.Agents {
				if def.Role == "coordinator" {
					continue // covered by TestBuildOrchestratorToolsAreRuntimeAllowed
				}
				actualTools := append(agent.SelectTools(c.coreTools, def.Tools), namedCoordinatorTool("submit_result"))
				allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(context.Background(), def, agentToolNames(actualTools)))
				for _, name := range agentToolNames(actualTools) {
					if !slices.Contains(allowed, name) {
						t.Errorf("agent %q: exposed tool %q is missing from runtime allowlist %v", defName, name, allowed)
					}
				}
			}
		})
	}
}

// TestWorkerExposedToolsIncludeResultProtocol pins the specific regression:
// submit_result reaches the model via createTaskAgentWithResultTool rather than
// any declared tool list, so it must be granted even though no agent declares
// it. Without this, every task fails its result protocol on the first pass and
// only the repair turn can ever complete one.
func TestWorkerExposedToolsIncludeResultProtocol(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "bash,terminal")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}
	c := &Coordinator{session: session, coreTools: workerInvariantCoreTools(t)}
	actualTools := append(agent.SelectTools(c.coreTools, session.Agents["helper"].Tools), namedCoordinatorTool("submit_result"))
	allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(context.Background(), session.Agents["helper"], agentToolNames(actualTools)))

	for _, want := range []string{"submit_result"} {
		if !slices.Contains(allowed, want) {
			t.Errorf("runtime allowlist is missing result-protocol tool %q: %v", want, allowed)
		}
	}
	// The declared grants must survive the union.
	for _, want := range []string{"view", "write", "bash", "wait_for", "terminal_wait"} {
		if !slices.Contains(allowed, want) {
			t.Errorf("runtime allowlist lost declared tool %q: %v", want, allowed)
		}
	}
	// alwaysIncludeTools reach the model regardless of the declared list.
	for _, want := range []string{"stm_write", "todo", "team_info"} {
		if !slices.Contains(allowed, want) {
			t.Errorf("runtime allowlist is missing always-included tool %q: %v", want, allowed)
		}
	}
}

// TestWithEffectiveToolsAllowed_NoDeclaredGrantsLeavesPolicyUnset guards the
// fallback the exposed-tools union must not disturb. The stream gate engages
// only once an allowlist is attached; when a team and agent declare nothing, the
// tool adapter stays the source of truth. Attaching a protocol-tools-only
// allowlist here would silently convert an unconstrained agent into a deny-all
// agent.
func TestWithEffectiveToolsAllowed_NoDeclaredGrantsLeavesPolicyUnset(t *testing.T) {
	session := &TeamSession{
		Config: agent.TeamConfig{Name: "team"},
		Agents: map[string]*agent.AgentDef{"worker": {Name: "worker"}},
	}
	c := &Coordinator{session: session, coreTools: workerInvariantCoreTools(t)}

	ctx := c.withEffectiveToolsAllowed(context.Background(), session.Agents["worker"], agentToolNames(agent.SelectTools(c.coreTools, session.Agents["worker"].Tools)))
	if allowed := tools.GetToolsAllowed(ctx); allowed != nil {
		t.Fatalf("allowlist should stay unset with no declared grants, got %v", allowed)
	}
}

// TestWorkerExposedToolsAllowlistIsDeduped keeps the union from degrading the
// allowlist into a bag of repeats as grant sources overlap.
func TestWorkerExposedToolsAllowlistIsDeduped(t *testing.T) {
	session, err := LoadDefaultTeam(t.TempDir(), nil, "bash")
	if err != nil {
		t.Fatalf("LoadDefaultTeam: %v", err)
	}
	c := &Coordinator{session: session, coreTools: workerInvariantCoreTools(t)}
	actualTools := agent.SelectTools(c.coreTools, session.Agents["helper"].Tools)
	allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(context.Background(), session.Agents["helper"], agentToolNames(actualTools)))

	seen := map[string]bool{}
	for _, name := range allowed {
		if seen[name] {
			t.Errorf("runtime allowlist contains duplicate %q: %v", name, allowed)
		}
		seen[name] = true
	}
}
