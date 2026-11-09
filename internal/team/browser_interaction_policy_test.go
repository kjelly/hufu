package team

import (
	"context"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
)

func TestBrowserInteractionRequiresExplicitTaskAndNativeAuthority(t *testing.T) {
	c := newTestCoordinator(t)
	def := c.session.Agents["worker"]
	if def == nil {
		t.Fatal("fixture worker unavailable")
	}
	def.Tools = "chrome__browser_snapshot,chrome__browser_click"
	def.SideEffect = string(SideEffectBrowserInteraction)
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhasePrepare}
	c.mcpManager = mcp.NewMCPToolManager("", "")
	t.Cleanup(func() { _ = c.mcpManager.Close() })
	object := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}
	cfg := mcp.MCPServerConfig{Type: "chrome-cdp", URL: "http://127.0.0.1:9222", AllowedTools: []string{"browser_snapshot", "browser_click"}, ToolPolicies: map[string]mcp.WorkerToolPolicy{
		"browser_snapshot": {SideEffect: "none", Filesystem: "none", InputSchema: object},
		"browser_click":    {SideEffect: "browser_interaction", Filesystem: "none", InputSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []any{"ref"}, "properties": map[string]any{"ref": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}}}},
	}}
	if err := c.mcpManager.LoadTools(t.Context(), map[string]mcp.MCPServerConfig{"chrome": cfg}); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"allowed", "read_only_task", "bound_workset", "no_net", "team_denied"} {
		t.Run(scenario, func(t *testing.T) {
			task := TaskDef{Agent: "worker", SideEffect: SideEffectBrowserInteraction}
			switch scenario {
			case "read_only_task":
				task.SideEffect = SideEffectNone
			case "bound_workset":
				task.WorksetBinding = &WorksetBinding{}
			case "no_net":
				c.noNet = true
				defer func() { c.noNet = false }()
			case "team_denied":
				c.session.Config.ToolsDenied = []string{"chrome__browser_click"}
				defer func() { c.session.Config.ToolsDenied = nil }()
			}
			ctx := withBrowserInteractionAuthority(context.WithValue(t.Context(), tools.AgentNameKey, "worker"), def, task)
			want := scenario == "allowed"
			if c.workerMCPBrowserInteractionGrant(ctx, "chrome__browser_click", nil) != want {
				t.Fatal("browser authority crossed task boundary")
			}
			if c.workerReadOnlyToolMutation(ctx, "bash", `{"command":"touch file"}`, nil) != true {
				t.Fatal("browser interaction widened filesystem authority")
			}
			if c.readOnlyToolCall("chrome__browser_click", `{"ref":"x"}`) || c.readOnlyToolCall(dynamicToolGatewayName, `{"action":"call","target":"chrome__browser_click","arguments":{"ref":"x"}}`) {
				t.Fatal("click was classified as replayable observation")
			}
			if scenario == "bound_workset" {
				return
			}
			snapshot, err := c.resolveNewTaskToolAuthorization(t.Context(), task, def)
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(snapshot.Targets))
			for _, target := range snapshot.Targets {
				names = append(names, target.Name)
			}
			if slices.Contains(names, "chrome__browser_click") != want {
				t.Fatalf("frozen authority=%v", names)
			}
			if scenario == "allowed" {
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "native browser authority", SideEffect: SideEffectBrowserInteraction, DynamicToolAuthorization: snapshot}})[0]
				for _, mode := range []WorkerToolResolutionMode{WorkerToolResolutionNormal, WorkerToolResolutionResultRepair, WorkerToolResolutionResume} {
					resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{Task: task, TodoID: item.ID, Mode: mode})
					if err != nil {
						t.Fatal(err)
					}
					if slices.Contains(resolved.AuthorizedNames, "chrome__browser_click") != (mode == WorkerToolResolutionNormal) {
						t.Fatalf("%s browser authority=%v", mode, resolved.AuthorizedNames)
					}
				}
			}

		})
	}
	policy := disciplinePolicy(CommitGatePolicy{RequireReconcile: true}, StopPolicy{}, ReplanPolicy{})
	if err := c.armDiscipline(t.Context(), "browser-task", TaskDef{Agent: "worker", SideEffect: SideEffectBrowserInteraction}, policy, nil); err != nil {
		t.Fatal(err)
	}
	if denial := c.commitGateDenial(t.Context(), "browser-task", "chrome__browser_snapshot", "{}"); denial != "" {
		t.Fatal("observation was treated as a browser mutation", denial)
	}
	if denial := c.commitGateDenial(t.Context(), "browser-task", "chrome__browser_click", `{"ref":"x"}`); denial == "" {
		t.Fatal("native click bypassed commit prerequisites")
	}
	if !nonReplayableSideEffect(SideEffectBrowserInteraction) || DefaultRecoveryPolicy(SideEffectBrowserInteraction, false) != RecoveryManual {
		t.Fatal("uncertain browser clicks can replay automatically")
	}
	if _, err := newMCPActionProvider("click", agent.ActionProviderConfig{Server: "chrome", Tool: "browser_click"}, map[string]mcp.MCPServerConfig{"chrome": cfg}); err == nil {
		t.Fatal("runtime action bypassed browser worker authority")
	}
}
