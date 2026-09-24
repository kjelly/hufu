package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

// routeRegistryCoordinator has two LLM backends and one agent backend, so
// route compilation can be tested without a provider.
func routeRegistryCoordinator(t *testing.T, agents map[string]*agent.AgentDef, routes map[string]config.ExecutionRouteConfig) *Coordinator {
	t.Helper()
	registry := NewExecutionRegistry()
	for _, backend := range []ExecutionBackend{
		fakeLanguageModelBackend{fakeExecutionBackend{name: "ollama", kind: execution.BackendKindLLM}},
		fakeLanguageModelBackend{fakeExecutionBackend{name: "openai", kind: execution.BackendKindLLM}},
		fakeExecutionBackend{name: "codex", kind: execution.BackendKindAgent},
	} {
		if err := registry.Register(backend); err != nil {
			t.Fatal(err)
		}
	}
	c := &Coordinator{session: &TeamSession{Agents: agents, ExecutionRouteConfigs: routes}}
	c.SetExecutionRegistry(registry)
	return c
}

func TestCompileExecutionRoute(t *testing.T) {
	tests := []struct {
		name  string
		route config.ExecutionRouteConfig
		want  []execution.ExecutionTarget
		err   string
	}{
		{name: "ordered canonical candidates", route: config.ExecutionRouteConfig{Candidates: []string{"local/qwen3:32b", "openai/gpt-5"}, FallbackOn: []string{"rate_limited", "provider_unavailable"}},
			want: []execution.ExecutionTarget{{Backend: "ollama", Model: "qwen3:32b"}, {Backend: "openai", Model: "gpt-5"}}},
		{name: "single candidate needs no fallback-on", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/qwen3"}},
			want: []execution.ExecutionTarget{{Backend: "ollama", Model: "qwen3"}}},
		{name: "no candidates", route: config.ExecutionRouteConfig{}, err: "1 to 4 candidates"},
		{name: "too many candidates", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/a", "ollama/b", "ollama/c", "ollama/d", "ollama/e"}, FallbackOn: []string{"rate_limited"}}, err: "1 to 4 candidates"},
		{name: "bare model", route: config.ExecutionRouteConfig{Candidates: []string{"qwen3"}}, err: "must name its backend"},
		{name: "unknown backend", route: config.ExecutionRouteConfig{Candidates: []string{"nowhere/model"}}, err: "nowhere"},
		{name: "agent backend", route: config.ExecutionRouteConfig{Candidates: []string{"codex/gpt-5"}}, err: "agent backend"},
		{name: "duplicate candidate", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/a", "local/a"}, FallbackOn: []string{"rate_limited"}}, err: "more than once"},
		{name: "several candidates without fallback-on", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/a", "openai/b"}}, err: "no fallback-on"},
		{name: "never-eligible fallback class", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/a", "openai/b"}, FallbackOn: []string{"auth_failed"}}, err: "never triggers a fallback"},
		{name: "unknown fallback class", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/a", "openai/b"}, FallbackOn: []string{"semantic_rejection"}}, err: "unknown fallback-on"},
		{name: "duplicate fallback class", route: config.ExecutionRouteConfig{Candidates: []string{"ollama/a", "openai/b"}, FallbackOn: []string{"rate_limited", "rate_limited"}}, err: "more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := routeRegistryCoordinator(t, nil, nil)
			route, err := c.compileExecutionRoute("coding", map[string]config.ExecutionRouteConfig{"coding": tt.route})
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) || !strings.Contains(err.Error(), executionRouteInvalidCode) {
					t.Fatalf("error = %v, want %s containing %q", err, executionRouteInvalidCode, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(route.Candidates, tt.want) {
				t.Fatalf("candidates = %v, want %v", route.Candidates, tt.want)
			}
			again, _ := c.compileExecutionRoute("coding", map[string]config.ExecutionRouteConfig{"coding": tt.route})
			if route.Digest == "" || route.Digest != again.Digest {
				t.Fatalf("digest %q is not deterministic (%q)", route.Digest, again.Digest)
			}
		})
	}
	c := routeRegistryCoordinator(t, nil, nil)
	if _, err := c.compileExecutionRoute("missing", nil); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("unknown route error = %v", err)
	}
}

func TestBindExecutionRoutes(t *testing.T) {
	single := map[string]config.ExecutionRouteConfig{
		"coding": {Candidates: []string{"ollama/qwen3"}},
		"review": {Candidates: []string{"openai/gpt-5"}},
		"multi":  {Candidates: []string{"ollama/a", "openai/b"}, FallbackOn: []string{"rate_limited"}},
	}
	tests := []struct {
		name   string
		agents map[string]*agent.AgentDef
		team   agent.TeamConfig
		want   map[string]string
		err    string
	}{
		{
			name: "precedence: own route, own model, team route; coordinators never bind",
			agents: map[string]*agent.AgentDef{
				"coder":    {Name: "coder", Role: "worker"},
				"reviewer": {Name: "reviewer", Role: "worker", ExecutionRoute: "review"},
				"planner":  {Name: "planner", Role: "worker", Generation: agent.GenerationParams{Model: "ollama/planner"}},
				"lead":     {Name: "lead", Role: "coordinator"},
			},
			team: agent.TeamConfig{ExecutionRoute: "coding"},
			want: map[string]string{"coder": "coding", "reviewer": "review"},
		},
		{name: "unknown team route", agents: map[string]*agent.AgentDef{"coder": {Name: "coder", Role: "worker", Generation: agent.GenerationParams{Model: "ollama/x"}}}, team: agent.TeamConfig{ExecutionRoute: "nope"}, err: "not defined"},
		{name: "multi-candidate with extra-models", agents: map[string]*agent.AgentDef{"coder": {Name: "coder", Role: "worker", ExecutionRoute: "multi", ExtraModels: []string{"ollama/c"}}}, err: executionRouteConflictCode},
		{name: "multi-candidate with escalate-on-retry", agents: map[string]*agent.AgentDef{"coder": {Name: "coder", Role: "worker", ExecutionRoute: "multi"}}, team: agent.TeamConfig{EscalateOnRetry: true}, err: executionRouteConflictCode},
		{name: "multi-candidate route binds", agents: map[string]*agent.AgentDef{"coder": {Name: "coder", Role: "worker", ExecutionRoute: "multi"}}, want: map[string]string{"coder": "multi"}},
		{name: "single-candidate route allows extra-models and escalate-on-retry", agents: map[string]*agent.AgentDef{"coder": {Name: "coder", Role: "worker", ExecutionRoute: "coding", ExtraModels: []string{"ollama/c"}}}, team: agent.TeamConfig{EscalateOnRetry: true}, want: map[string]string{"coder": "coding"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := routeRegistryCoordinator(t, tt.agents, single)
			c.session.Config = tt.team
			err := c.bindExecutionRoutes()
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make(map[string]string, len(c.session.AgentExecutionRoutes))
			for agentName, route := range c.session.AgentExecutionRoutes {
				got[agentName] = route
			}
			if len(got) != len(tt.want) {
				t.Fatalf("bindings = %v, want %v", got, tt.want)
			}
			for agentName, route := range tt.want {
				if got[agentName] != route {
					t.Fatalf("bindings = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestMultiCandidateRouteConflictsWithDecisionRoles(t *testing.T) {
	agents := map[string]*agent.AgentDef{"judge": {Name: "judge", Role: "worker", ExecutionRoute: "multi"}}
	c := routeRegistryCoordinator(t, agents, map[string]config.ExecutionRouteConfig{"multi": {Candidates: []string{"ollama/a", "openai/b"}, FallbackOn: []string{"rate_limited"}}})
	c.session.Config.Decision.Profiles = map[string]agent.DecisionPolicy{
		"standard": {JudgeRole: &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, Pin: &agent.RoutingPin{Agent: "judge"}}},
	}
	if err := c.bindExecutionRoutes(); err == nil || !strings.Contains(err.Error(), executionRouteConflictCode) {
		t.Fatalf("error = %v, want %s for a decision-role agent", err, executionRouteConflictCode)
	}
}

func TestAdmittedExecutionRoute(t *testing.T) {
	primary := execution.ExecutionTarget{Backend: "ollama", Model: "a"}
	other := execution.ExecutionTarget{Backend: "openai", Model: "b"}
	def := &agent.AgentDef{Name: "coder", Role: "worker"}
	session := func(candidates ...execution.ExecutionTarget) *TeamSession {
		route := &ExecutionRouteDefinition{Name: "coding", Candidates: candidates, FallbackOn: []ProviderFailureClass{ProviderRateLimited}}
		route.Digest = executionRouteDigest(route.Name, route.Candidates, route.FallbackOn)
		return &TeamSession{ExecutionRoutes: map[string]*ExecutionRouteDefinition{"coding": route}, AgentExecutionRoutes: map[string]string{"coder": "coding"}}
	}
	tests := []struct {
		name    string
		session *TeamSession
		task    TaskDef
		bound   bool
		err     string
	}{
		{name: "primary target binds the route", session: session(primary, other), task: TaskDef{ResolvedExecutionTarget: primary}, bound: true},
		{name: "escalate conflicts with a multi-candidate route", session: session(primary, other), task: TaskDef{ResolvedExecutionTarget: primary, Escalate: true}, err: executionRouteConflictCode},
		{name: "another model conflicts with a multi-candidate route", session: session(primary, other), task: TaskDef{ResolvedExecutionTarget: other}, err: executionRouteConflictCode},
		{name: "another model on a single-candidate route admits a plain target", session: session(primary), task: TaskDef{ResolvedExecutionTarget: other}},
		{name: "escalate on a single-candidate route is allowed", session: session(primary), task: TaskDef{ResolvedExecutionTarget: primary, Escalate: true}, bound: true},
		{name: "a sidecar task never binds a route", session: session(primary, other), task: TaskDef{ResolvedExecutionTarget: primary, Sidecar: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{session: tt.session}
			binding, err := c.admittedExecutionRoute(tt.task, def)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (binding != nil) != tt.bound {
				t.Fatalf("binding = %+v, want bound=%v", binding, tt.bound)
			}
			if binding != nil && !execution.TargetsEqual(binding.Candidates[0], tt.task.ResolvedExecutionTarget) {
				t.Fatalf("binding primary %v != task target %v", binding.Candidates[0], tt.task.ResolvedExecutionTarget)
			}
		})
	}
}

func TestRouteBoundWorkerSkipsComplexityModelSelection(t *testing.T) {
	def := &agent.AgentDef{Name: "coder", Role: "worker"}
	route := &ExecutionRouteDefinition{Name: "coding", Candidates: []execution.ExecutionTarget{{Backend: "ollama", Model: "routed"}}}
	c := &Coordinator{
		session:   &TeamSession{ExecutionRoutes: map[string]*ExecutionRouteDefinition{"coding": route}, AgentExecutionRoutes: map[string]string{"coder": "coding"}},
		modelList: []config.ModelEntry{{ID: "ollama/cheap"}, {ID: "ollama/strong"}},
	}
	if got := c.selectTaskModel(TaskDef{Agent: "coder", Goal: "refactor the whole codebase"}, def); got != "" {
		t.Fatalf("selectTaskModel = %q, want no complexity override for a route-bound worker", got)
	}
	if got := c.resolveAgentModel(def, ""); got != "ollama/routed" {
		t.Fatalf("resolveAgentModel = %q, want the route's primary", got)
	}
}

func TestModelTaskPayloadCannotSetExecutionRoute(t *testing.T) {
	for _, key := range []string{"execution_route", "execution-route", "execution_candidates", "fallback_on"} {
		payload := `{"tasks":[{"agent":"worker","goal":"x","` + key + `":"coding"}]}`
		if _, err := decodeModelTaskDefs([]byte(payload)); err == nil {
			t.Fatalf("a coordinator payload set %s", key)
		}
	}
}

// unknownCapabilityIntrospector reports tool support as missing for models
// containing weak, reports nothing for models containing unknown, and
// supports everything else.
type unknownCapabilityIntrospector struct{ weak, unknown string }

func (r unknownCapabilityIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, modelID string) (providerintrospection.RuntimeModelInfo, error) {
	switch {
	case r.weak != "" && strings.Contains(modelID, r.weak):
		return providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": providerintrospection.CapabilityNo}}, nil
	case r.unknown != "" && strings.Contains(modelID, r.unknown):
		return providerintrospection.RuntimeModelInfo{}, nil
	}
	return providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": providerintrospection.CapabilityYes}}, nil
}

func TestCapabilityValidationChecksEveryRouteCandidate(t *testing.T) {
	tests := []struct {
		name       string
		introspect unknownCapabilityIntrospector
		wantErr    bool
		wantWarn   bool
	}{
		{name: "known incompatible candidate fails", introspect: unknownCapabilityIntrospector{weak: "fallback-weak"}, wantErr: true},
		{name: "unknown candidate warns", introspect: unknownCapabilityIntrospector{unknown: "fallback-weak"}, wantWarn: true},
		{name: "compatible candidates pass", introspect: unknownCapabilityIntrospector{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			runtime := &ModelProfileRuntime{
				manager: manager,
				resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
					return tt.introspect
				}, modelprofile.ProfileCacheOptions{}),
			}
			coder := &agent.AgentDef{Name: "coder", Role: "worker", Requirements: agent.ContractRequirements{Model: agent.ModelRequirements{Tools: true}}}
			route := &ExecutionRouteDefinition{Name: "coding", Candidates: []execution.ExecutionTarget{{Backend: "ollama", Model: "primary"}, {Backend: "ollama", Model: "fallback-weak"}}}
			c := &Coordinator{
				modelProfileRuntime: runtime,
				session: &TeamSession{
					Agents:          map[string]*agent.AgentDef{"coder": coder},
					ExecutionRoutes: map[string]*ExecutionRouteDefinition{"coding": route}, AgentExecutionRoutes: map[string]string{"coder": "coding"},
				},
			}
			validation := c.ValidateModelCapabilities(t.Context())
			if gotErr := validation.Err() != nil; gotErr != tt.wantErr {
				t.Fatalf("validation error = %v, want error=%v", validation.Err(), tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(validation.Err().Error(), "fallback-weak") {
				t.Fatalf("validation error %v does not name the incompatible candidate", validation.Err())
			}
			warned := slices.ContainsFunc(validation.Warnings, func(w string) bool { return strings.Contains(w, "fallback-weak") })
			if warned != tt.wantWarn {
				t.Fatalf("warnings = %v, want candidate warning=%v", validation.Warnings, tt.wantWarn)
			}
		})
	}
}

// routeSubmittingAgent is a fake worker that submits a successful result
// through the real submit_result tool.
type routeSubmittingAgent struct{ c *Coordinator }

func (a routeSubmittingAgent) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a routeSubmittingAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a routeSubmittingAgent) run(ctx context.Context) (*fantasy.AgentResult, error) {
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	response, err := (&submitResultTool{coordinator: a.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{
		Name: submitResultToolName, Input: `{"status":"success","summary":"done on the routed target"}`,
	})
	if err != nil {
		return nil, err
	}
	if response.IsError {
		return nil, errors.New(response.Content)
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
}

// loadRouteTeam loads a team from disk the way the CLI does: LoadTeam, then
// hufu.yaml's routes on the session, then NewCoordinator.
func loadRouteTeam(t *testing.T, workspace string, routes map[string]config.ExecutionRouteConfig) *Coordinator {
	t.Helper()
	dir := filepath.Join(workspace, "team")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: routed-team\nexecution-route: coding\nmax-retries: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, dir, "coder.md", "name: coder\ndescription: Codes.\ntools: view", "Code.")
	writeAgentFile(t, dir, "reviewer.md", "name: reviewer\ndescription: Reviews.\ntools: view\nexecution-route: review", "Review.")
	writeAgentFile(t, dir, "planner.md", "name: planner\ndescription: Plans.\ntools: view\nmodel: ollama/planner", "Plan.")
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
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
	session.ExecutionRouteConfigs = routes
	c, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(func() {
		if c.eventStore != nil {
			_ = c.eventStore.Close()
		}
		c.CloseContextPreflight()
	})
	return c
}

var testRoutes = map[string]config.ExecutionRouteConfig{
	"coding": {Candidates: []string{"ollama/qwen3:32b"}},
	"review": {Candidates: []string{"local/reviewer-model"}},
}

func TestLoadedTeamFreezesExecutionRoutesIntoOccurrences(t *testing.T) {
	c := loadRouteTeam(t, t.TempDir(), testRoutes)
	for agentName, want := range map[string]string{"coder": "ollama/qwen3:32b", "reviewer": "ollama/reviewer-model", "planner": "ollama/planner"} {
		if got := c.resolveAgentModel(c.session.Agents[agentName], ""); got != want {
			t.Fatalf("%s resolves to %q, want %q", agentName, got, want)
		}
	}
	pinned := make(map[string]string)
	for _, binding := range c.executionPolicy.snapshot.ExecutionRoutes {
		pinned[binding.Owner] = binding.Route
	}
	if pinned["agent:coder"] != "coding" || pinned["agent:reviewer"] != "review" || pinned["agent:planner"] != "" {
		t.Fatalf("policy snapshot routes = %+v", c.executionPolicy.snapshot.ExecutionRoutes)
	}

	store, err := NewEventStore(c.session.Workspace, "run-routes", "session-routes")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	c.SetEventJournal(eventStoreJournal{store: store})
	c.executionRunID = "run-routes"
	c.contextRepo = nil
	c.workerAgentOverride = routeSubmittingAgent{c: c}
	if _, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "coder", Goal: "write the feature"}, {Agent: "planner", Goal: "plan the feature"}}); err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	var coder, planner *TodoItem
	for _, item := range c.taskTracker.TodoList().Items() {
		switch item.Agent {
		case "coder":
			coder = item
		case "planner":
			planner = item
		}
	}
	coding := c.session.ExecutionRoutes["coding"]
	if coder == nil || coder.ExecutionRoute == nil || coder.ExecutionRoute.Name != "coding" || coder.ExecutionRoute.Digest != coding.Digest ||
		!slices.Equal(coder.ExecutionRoute.Candidates, coding.Candidates) || !execution.TargetsEqual(coder.ExecutionTarget, coding.Candidates[0]) {
		t.Fatalf("coder occurrence = target %v route %+v, want the frozen coding route", coder.ExecutionTarget, coder.ExecutionRoute)
	}
	if !slices.Equal(coder.ExecutionTopology, []execution.ExecutionTarget{coding.Candidates[0]}) {
		t.Fatalf("coder topology = %v, want only the primary", coder.ExecutionTopology)
	}
	if planner == nil || planner.ExecutionRoute != nil {
		t.Fatalf("planner (own model) occurrence route = %+v, want none", planner.ExecutionRoute)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type != string(EventTaskCreated) || event.TaskID != coder.ID {
			continue
		}
		var payload struct {
			ExecutionRoute *ExecutionRouteBinding `json:"execution_route"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		found = payload.ExecutionRoute != nil && payload.ExecutionRoute.Digest == coding.Digest
	}
	if !found {
		t.Fatal("task_created does not carry the frozen execution route")
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		t.Fatalf("ReplayTodoList: %v", err)
	}
	for _, item := range replayed {
		if item.ID == coder.ID && (item.ExecutionRoute == nil || item.ExecutionRoute.Digest != coding.Digest) {
			t.Fatalf("replayed coder route = %+v", item.ExecutionRoute)
		}
	}
	if task := taskDefFromTodoItem(coder); task.ExecutionRoute == nil || task.ExecutionRoute.Digest != coding.Digest {
		t.Fatalf("resumed task definition lost the route: %+v", task.ExecutionRoute)
	}

}

func TestDirectAgentFreezesItsExecutionRoute(t *testing.T) {
	c := loadRouteTeam(t, t.TempDir(), testRoutes)
	c.contextRepo = nil
	c.workerAgentOverride = routeSubmittingAgent{c: c}
	if _, err := c.RunDirectAgent(context.Background(), "reviewer", "review the feature"); err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	found := false
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.Agent == "reviewer" {
			found = true
			if item.ExecutionRoute == nil || item.ExecutionRoute.Name != "review" || item.ExecutionTarget.Model != "reviewer-model" {
				t.Fatalf("direct reviewer occurrence = target %v route %+v", item.ExecutionTarget, item.ExecutionRoute)
			}
		}
	}
	if !found {
		t.Fatal("no direct reviewer occurrence")
	}
}

func TestLoadTeamRejectsRouteReferencesItCannotHonor(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		want  string
	}{
		{name: "route and model", agent: "name: coder\ntools: view\nmodel: ollama/x\nexecution-route: coding", want: executionRouteConflictCode},
		{name: "invalid route name", agent: "name: coder\ntools: view\nexecution-route: Coding_Route", want: executionRouteInvalidCode},
		{name: "coordinator route", agent: "name: lead\nrole: coordinator\nexecution-route: coding", want: "only to workers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: routes\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeAgentFile(t, dir, "agent.md", tt.agent, "Work.")
			if _, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadTeam error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestChangedExecutionRouteFailsResumeClosed(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	start := func(routes map[string]config.ExecutionRouteConfig) *Coordinator {
		c := loadRouteTeam(t, workspace, routes)
		if session := LoadSession(c.session.Workspace); session != nil {
			c.SetSessionData(session)
		}
		c.initEventStore()
		return c
	}
	first := start(testRoutes)
	if err := first.checkRunAdmission(); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	closeWorkerModelResumeStore(t, first)

	changed := map[string]config.ExecutionRouteConfig{"coding": {Candidates: []string{"ollama/qwen3:14b"}}, "review": testRoutes["review"]}
	drifted := start(changed)
	if err := drifted.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "snapshot drift detected") {
		t.Fatalf("resume with a changed route error = %v, want policy snapshot drift", err)
	}
	closeWorkerModelResumeStore(t, drifted)

	same := start(testRoutes)
	if err := same.checkRunAdmission(); err != nil {
		t.Fatalf("resume with unchanged routes: %v", err)
	}
}

// flakyPrimaryAgent fails its model call with a rate limit on the primary
// model and submits a result on any other.
type flakyPrimaryAgent struct {
	c       *Coordinator
	primary string
	models  []string
}

func (a *flakyPrimaryAgent) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a *flakyPrimaryAgent) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx)
}

func (a *flakyPrimaryAgent) run(ctx context.Context) (*fantasy.AgentResult, error) {
	model, _ := ctx.Value(modelKey{}).(string)
	a.models = append(a.models, model)
	if model == a.primary {
		return nil, &fantasy.ProviderError{StatusCode: 429, Message: "rate limited"}
	}
	return routeSubmittingAgent{c: a.c}.run(ctx)
}

func TestDirectAgentFallsBackOnItsRoute(t *testing.T) {
	routes := map[string]config.ExecutionRouteConfig{
		"coding": testRoutes["coding"],
		"review": {Candidates: []string{"ollama/review-primary", "ollama/review-fallback"}, FallbackOn: []string{"rate_limited"}},
	}
	c := loadRouteTeam(t, t.TempDir(), routes)
	c.contextRepo = nil
	agentOverride := &flakyPrimaryAgent{c: c, primary: "review-primary"}
	c.workerAgentOverride = agentOverride
	if _, err := c.RunDirectAgent(context.Background(), "reviewer", "review with fallback"); err != nil {
		t.Fatalf("RunDirectAgent: %v (models %v)", err, agentOverride.models)
	}
	if !slices.Equal(agentOverride.models, []string{"review-primary", "review-fallback"}) {
		t.Fatalf("direct attempts ran models %v, want primary then fallback", agentOverride.models)
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.Agent == "reviewer" && item.Status != TaskDone {
			t.Fatalf("direct reviewer status = %s (%s)", item.Status, item.Detail)
		}
	}
}
