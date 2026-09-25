package team

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func TestStaticTeamActionToolExposure(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", strings.Replace(actionCatalogTestEntry,
		"discover: [runtime-engineer, network-engineer, critic]", "discover: [runtime-engineer, network-engineer]", 1)))
	tests := []struct {
		name  string
		agent string
		mode  WorkerToolResolutionMode
		task  TaskDef
		want  bool
	}{
		{name: "discoverer", agent: "runtime-engineer", want: true},
		{name: "non-discoverer", agent: "critic"},
		{name: "result-only repair", agent: "runtime-engineer", mode: WorkerToolResolutionResultRepair},
		{name: "resume", agent: "runtime-engineer", mode: WorkerToolResolutionResume},
		{name: "sidecar task", agent: "runtime-engineer", task: TaskDef{Sidecar: true}},
		{name: "initial plan", agent: "runtime-engineer", mode: WorkerToolResolutionInitialPlan, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolution, err := ResolveStaticWorkerTools(StaticToolResolutionInput{
				Session: session, Agent: session.Agents[tt.agent], Task: tt.task, LifecycleMode: tt.mode,
				BaseTools: []string{"view"}, WorkflowPhase: PhaseExecute,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := slices.Contains(resolution.Names, teamActionListToolName) && slices.Contains(resolution.Names, teamActionGetToolName)
			if got != tt.want {
				t.Fatalf("catalog tools exposed = %v, want %v (names %v)", got, tt.want, resolution.Names)
			}
		})
	}
	session.Config.ToolsDenied = []string{teamActionGetToolName}
	resolution, err := ResolveStaticWorkerTools(StaticToolResolutionInput{Session: session, Agent: session.Agents["runtime-engineer"], BaseTools: []string{"view"}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(resolution.Names, teamActionGetToolName) || !slices.Contains(resolution.Names, teamActionListToolName) {
		t.Fatalf("tools-denied should omit only team_action_get, names %v", resolution.Names)
	}
}

func TestWorkerTeamActionToolsNeedAnOrdinaryTaskContext(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	c := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "runtime-engineer", Desc: "diagnose"}})[0]
	names := []string{teamActionListToolName, teamActionGetToolName}
	def := session.Agents["runtime-engineer"]
	tests := []struct {
		name string
		ctx  context.Context
		todo string
		want int
	}{
		{name: "ordinary task", ctx: context.Background(), todo: item.ID, want: 2},
		{name: "missing todo", ctx: context.Background(), todo: "missing"},
		{name: "no todo", ctx: context.Background()},
		{name: "extra-model leaf", ctx: context.WithValue(context.Background(), leafExecutionKey{}, true), todo: item.ID},
		{name: "direct agent", ctx: withDirectAgentInvocation(context.Background()), todo: item.ID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			built := c.workerTeamActionTools(tt.ctx, def, names, WorkerToolResolutionRequest{TodoID: tt.todo})
			if len(built) != tt.want {
				t.Fatalf("built %d catalog tools, want %d", len(built), tt.want)
			}
		})
	}
	if err := c.teamActionToolCollision([]fantasy.AgentTool{namedCoordinatorTool(teamActionListToolName)}); err == nil {
		t.Fatal("a concrete handler named team_action_list was not rejected in a catalog team")
	}
	plain := &Coordinator{session: &TeamSession{}}
	if err := plain.teamActionToolCollision([]fantasy.AgentTool{namedCoordinatorTool(teamActionListToolName)}); err != nil {
		t.Fatalf("team without a catalog rejected a same-named handler: %v", err)
	}
}

func workerTeamActionContext(todoID, agentName string) context.Context {
	ctx := context.WithValue(context.Background(), todoIDKey{}, todoID)
	return context.WithValue(ctx, tools.AgentNameKey, agentName)
}

func runTeamActionTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, input string) (string, bool) {
	t.Helper()
	response, err := tool.Run(ctx, fantasy.ToolCall{Name: tool.Info().Name, Input: input})
	if err != nil {
		t.Fatalf("%s: %v", tool.Info().Name, err)
	}
	return response.Content, response.IsError
}

func TestWorkerTeamActionListAndGet(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	c := &Coordinator{session: session}
	list := &teamActionListTool{coordinator: c, todoID: "1", agent: "critic"}
	get := &teamActionGetTool{coordinator: c, todoID: "1", agent: "critic"}
	ctx := workerTeamActionContext("1", "critic")

	content, isError := runTeamActionTool(t, list, ctx, `{"query":"DIAGNOSTICS"}`)
	var listed teamActionListResult
	if isError || json.Unmarshal([]byte(content), &listed) != nil || len(listed.Actions) != 1 || listed.Actions[0].ProposalAllowed {
		t.Fatalf("list = %s (error %v)", content, isError)
	}
	if content, _ = runTeamActionTool(t, list, ctx, `{"query":"nothing-matches"}`); !strings.Contains(content, `"actions":[]`) {
		t.Fatalf("list without matches = %s", content)
	}

	content, isError = runTeamActionTool(t, get, ctx, `{"action":"collect-debug-bundle"}`)
	var contract teamActionContract
	if isError || json.Unmarshal([]byte(content), &contract) != nil || contract.SchemaDialect != teamActionSchemaDialect ||
		contract.InputSchema.Type != "object" || !contract.RequireProposal || contract.ProposalAllowed {
		t.Fatalf("get = %s (error %v)", content, isError)
	}
	for _, leak := range []string{"diagnostics.sh", "collect_debug_bundle", `"capability"`, `"agent"`, "provider"} {
		if strings.Contains(content, leak) {
			t.Fatalf("worker get leaks %q: %s", leak, content)
		}
	}
	if content, isError = runTeamActionTool(t, get, ctx, `{"action":"missing"}`); !isError || !strings.HasPrefix(content, teamActionNotDiscoverable) {
		t.Fatalf("get missing = %s", content)
	}
	outsider := &teamActionGetTool{coordinator: c, todoID: "1", agent: "coordinator"}
	if content, isError = runTeamActionTool(t, outsider, workerTeamActionContext("1", "coordinator"), `{"action":"collect-debug-bundle"}`); !isError || !strings.HasPrefix(content, teamActionNotDiscoverable) {
		t.Fatalf("get by a non-discoverer = %s", content)
	}
	if content, isError = runTeamActionTool(t, list, ctx, `{"query":"x","extra":1}`); !isError || !strings.HasPrefix(content, teamActionInvalidToolRequest) {
		t.Fatalf("list with an unknown field = %s", content)
	}
}

func TestWorkerTeamActionToolsCheckTheirCaller(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	list := &teamActionListTool{coordinator: &Coordinator{session: session}, todoID: "1", agent: "critic"}
	repair := context.WithValue(workerTeamActionContext("1", "critic"), protocolRepairExecutionKey{}, true)
	leaf := context.WithValue(workerTeamActionContext("1", "critic"), leafExecutionKey{}, true)
	for name, ctx := range map[string]context.Context{
		"other task": workerTeamActionContext("2", "critic"), "other agent": workerTeamActionContext("1", "runtime-engineer"),
		"protocol repair": repair, "leaf": leaf,
	} {
		if content, isError := runTeamActionTool(t, list, ctx, `{}`); !isError || !strings.HasPrefix(content, teamActionCallerInvalid) {
			t.Fatalf("%s: response %q, want %s", name, content, teamActionCallerInvalid)
		}
	}
}

func TestTeamActionToolsAreReadOnlyAndBoundArtifactSafe(t *testing.T) {
	for _, name := range []string{teamActionListToolName, teamActionGetToolName} {
		if readOnlyToolMutation(name, `{}`) {
			t.Errorf("%s is treated as a mutation", name)
		}
		if !isReadOnlyToolCall(name, `{}`) {
			t.Errorf("%s is not a read-only call", name)
		}
	}
	for _, tool := range []fantasy.AgentTool{&teamActionListTool{}, &teamActionGetTool{}} {
		if !artifactScopeToolTrusted(tool) {
			t.Errorf("%s lacks the bound-artifact marker", tool.Info().Name)
		}
		describer, ok := tool.(interface {
			DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor
		})
		if !ok || describer.DescribeWorkspaceScope() != (tools.ToolWorkspaceScopeDescriptor{}) {
			t.Errorf("%s does not describe an empty workspace scope", tool.Info().Name)
		}
	}
}

func TestTeamActionToolInfoIsDeterministic(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	c := &Coordinator{session: session}
	for _, pair := range [][2]fantasy.AgentTool{
		{&teamActionGetTool{coordinator: c, todoID: "1", agent: "critic"}, &teamActionGetTool{coordinator: c, todoID: "2", agent: "critic"}},
		{&teamActionListTool{coordinator: c, todoID: "1", agent: "critic"}, &teamActionListTool{coordinator: c, todoID: "2", agent: "critic"}},
		{&coordinatorTeamActionGetTool{coordinator: c}, &coordinatorTeamActionGetTool{coordinator: c}},
	} {
		if !reflect.DeepEqual(pair[0].Info(), pair[1].Info()) {
			t.Fatalf("Info differs between resolutions: %#v vs %#v", pair[0].Info(), pair[1].Info())
		}
	}
}

func TestCoordinatorTeamActionToolsAndPrompt(t *testing.T) {
	session := loadActionCatalogTeam(t, actionCatalogTestManifest("", actionCatalogTestEntry))
	c := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	built := c.coordinatorTeamActionTools()
	if names := agentToolNames(built); !slices.Equal(names, []string{teamActionListToolName, teamActionGetToolName}) {
		t.Fatalf("coordinator catalog tools = %v", names)
	}
	orchestrator := agentToolNames(c.buildOrchestratorToolsFor(nil))
	if !slices.Contains(orchestrator, teamActionListToolName) || !slices.Contains(orchestrator, teamActionGetToolName) {
		t.Fatalf("orchestrator tools %v lack the catalog tools", orchestrator)
	}
	content, isError := runTeamActionTool(t, built[0], context.Background(), `{}`)
	if isError || !strings.Contains(content, `"agent":"runtime-engineer"`) || !strings.Contains(content, `"invocations_used":0`) || !strings.Contains(content, `"proposal_counts":{`) {
		t.Fatalf("coordinator list = %s", content)
	}
	content, isError = runTeamActionTool(t, built[1], context.Background(), `{"action":"collect-debug-bundle"}`)
	if isError || !strings.Contains(content, `"input_schema"`) || !strings.Contains(content, `"proposals":[]`) {
		t.Fatalf("coordinator get = %s", content)
	}
	for _, leak := range []string{"diagnostics.sh", `"capability"`, "collect_debug_bundle", "provider"} {
		if strings.Contains(content, leak) {
			t.Fatalf("coordinator get leaks %q: %s", leak, content)
		}
	}
	var prompt strings.Builder
	c.appendActionCatalogPrompt(&prompt)
	if !strings.Contains(prompt.String(), "catalog_action") {
		t.Fatalf("catalog prompt = %q", prompt.String())
	}

	plain := &Coordinator{session: &TeamSession{}, taskTracker: NewTaskTracker()}
	if tools := plain.coordinatorTeamActionTools(); tools != nil {
		t.Fatalf("team without a catalog got coordinator catalog tools %v", agentToolNames(tools))
	}
	var none strings.Builder
	plain.appendActionCatalogPrompt(&none)
	if none.Len() != 0 {
		t.Fatalf("team without a catalog got catalog prompt %q", none.String())
	}
}

func TestTeamLintKnowsCatalogToolsOnlyWithACatalog(t *testing.T) {
	manifest := actionCatalogTestManifest("", actionCatalogTestEntry)
	dir := writeActionCatalogTeam(t, manifest)
	writeAgentFile(t, dir, "critic.md", "name: critic\ndescription: Critiques.\ntools: view,team_action_list,team_action_get", "Critique.")
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range result.Findings {
		if finding.Code == FindingDeclaredToolMissing {
			t.Fatalf("catalog team reported %s: %#v", FindingDeclaredToolMissing, finding)
		}
	}
}

func TestResolveTaskToolsIncludesCatalogToolsForDiscoverers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	teamDir := writeActionCatalogTeam(t, actionCatalogTestManifest("default-llm-backend: ollama\n", strings.Replace(actionCatalogTestEntry,
		"discover: [runtime-engineer, network-engineer, critic]", "discover: [runtime-engineer, network-engineer]", 1)))
	c := newActionCatalogCoordinator(t, t.TempDir(), teamDir, nil)
	for _, tt := range []struct {
		agent string
		want  bool
	}{{"runtime-engineer", true}, {"critic", false}} {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: tt.agent, Desc: "inspect"}})[0]
		resolved, err := c.ToolResolver().ResolveTaskTools(context.Background(), c.session.Agents[tt.agent], WorkerToolResolutionRequest{
			Task: TaskDef{Agent: tt.agent, Goal: "inspect"}, TodoID: item.ID,
		})
		if err != nil {
			t.Fatalf("%s: ResolveTaskTools: %v", tt.agent, err)
		}
		got := slices.Contains(resolved.Names, teamActionListToolName) && slices.Contains(resolved.Names, teamActionGetToolName)
		if got != tt.want {
			t.Fatalf("%s: catalog tools resolved = %v, want %v (names %v)", tt.agent, got, tt.want, resolved.Names)
		}
	}
}
