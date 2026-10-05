package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/ollamaweb"
	"github.com/kjelly/hufu/internal/tools"
)

type ollamaWebTransportFunc func(*http.Request) (*http.Response, error)

func (fn ollamaWebTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestOllamaWebRegistryMatchesConcretePolicy(t *testing.T) {
	for _, policy := range []struct {
		noNet, forceMCP bool
	}{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		actual := agentToolNames(agent.BuildAllAgentTools(t.TempDir(), tools.WithNetworkBlock(policy.noNet), tools.WithForceMCP(policy.forceMCP)))
		offline := tools.BuiltinToolNames(policy.noNet, policy.forceMCP)
		if !slices.Equal(actual, offline) {
			t.Fatalf("policy %+v: concrete=%v offline=%v", policy, actual, offline)
		}
		for _, name := range []string{"web_search", "web_fetch"} {
			if slices.Contains(actual, name) != !(policy.noNet || policy.forceMCP) {
				t.Fatalf("policy %+v: tool %q visibility mismatch: %v", policy, name, actual)
			}
			if !tools.ForceMCPBlockedTools[name] || tools.GetToolLevel(name) != tools.ToolLevelMedium {
				t.Fatalf("tool %q missing force-MCP or medium-risk classification", name)
			}
		}
	}
}

func TestOllamaWebExplicitGrantAcrossWorkerSurfaces(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared string
		want     []string
	}{
		{"blank", "", nil}, {"all", "all", nil}, {"other", "view", nil},
		{"search", "view,web_search", []string{"web_search"}},
		{"fetch", "view,web_fetch", []string{"web_fetch"}},
		{"both", "view,web_search,web_fetch", []string{"web_search", "web_fetch"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newDirectTypedCoordinator(t, test.declared, []string{"web_search", "web_fetch"}, nil)
			c.ollamaWebKeyPresent = true
			def := c.session.Agents["worker"]
			for _, candidate := range [][]string{
				agentToolNames(c.selectWorkerTools(def)),
				agentToolNames(c.selectWorkerToolsForTask(def, TaskDef{Agent: def.Name})),
				c.staticToolGrantNames(def, TaskDef{Agent: def.Name}),
			} {
				for _, name := range []string{"web_search", "web_fetch"} {
					if slices.Contains(candidate, name) != slices.Contains(test.want, name) {
						t.Fatalf("tool %q visibility: names=%v want=%v", name, candidate, test.want)
					}
				}
			}
			base := tools.BuiltinToolNames(false, false)
			static, err := ResolveStaticWorkerTools(StaticToolResolutionInput{
				Session: c.session, Agent: def, BaseTools: base, WorkflowPhase: PhaseExecute,
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"web_search", "web_fetch"} {
				if slices.Contains(static.Names, name) != slices.Contains(test.want, name) {
					t.Fatalf("offline tool %q visibility: names=%v want=%v", name, static.Names, test.want)
				}
			}
			allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(t.Context(), def, []string{"web_search", "web_fetch"}))
			for _, name := range []string{"web_search", "web_fetch"} {
				if slices.Contains(allowed, name) != slices.Contains(test.want, name) {
					t.Fatalf("runtime allowlist for %q: allowed=%v want=%v", name, allowed, test.want)
				}
			}
		})
	}
}

func TestOllamaWebEffectivePolicyAndPreflight(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared string
		denied   []string
		noNet    bool
		forceMCP bool
		cliNoNet bool
		cliMCP   bool
		phase    bool
		sequence []string
		mode     WorkerToolResolutionMode
		missing  bool
	}{
		{name: "explicit missing key", declared: "view,web_search", missing: true},
		{name: "blank never needs key", declared: "", missing: false},
		{name: "team deny", declared: "view,web_search", denied: []string{"web_search"}},
		{name: "agent no-net", declared: "view,web_search", noNet: true},
		{name: "agent force-mcp", declared: "view,web_search", forceMCP: true},
		{name: "team or CLI no-net", declared: "view,web_search", cliNoNet: true},
		{name: "team or CLI force-mcp", declared: "view,web_search", cliMCP: true},
		{name: "prepare phase", declared: "view,web_search", phase: true},
		{name: "closed sequence", declared: "view,web_search", sequence: []string{"view"}},
		{name: "result-only", declared: "view,web_search", mode: WorkerToolResolutionResultRepair},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := newDirectTypedCoordinator(t, test.declared, nil, test.denied)
			def := c.session.Agents["worker"]
			def.NoNet, def.ForceMCP = test.noNet, test.forceMCP
			c.noNet, c.forceMCP = test.cliNoNet, test.cliMCP
			if test.phase {
				c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhasePrepare}
			}
			task := TaskDef{Agent: def.Name, Execution: ExecutionContract{ToolSequence: test.sequence}}
			if test.mode == WorkerToolResolutionResultRepair {
				task.Execution.RequiresResult = true
			}
			todoID := ""
			if test.mode == WorkerToolResolutionResultRepair {
				todoID = resolverTodoID(t, c, "repair")
			}
			resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{Task: task, TodoID: todoID, Mode: test.mode})
			if test.missing {
				if err == nil || !strings.Contains(err.Error(), "ollama_web_api_key_missing") {
					t.Fatalf("missing key error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(resolved.Names, "web_search") {
				t.Fatalf("policy did not remove web_search: %v", resolved.Names)
			}
		})
	}
}

func TestOllamaWebRuntimeGateIgnoresSessionPermissionOverride(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", []string{"web_search"}, nil)
	ctx := context.WithValue(t.Context(), tools.AgentToolsSessionPermissionsKey, map[string]bool{"web_search": true})
	denial, err := c.authorizeToolInvocation(ctx, "worker", "web_search")
	if err != nil || !strings.Contains(denial, "explicit agent tools grant") {
		t.Fatalf("runtime gate: denial=%q err=%v", denial, err)
	}
	c.session.Agents["worker"].Tools = "view,web_search"
	denial, err = c.authorizeToolInvocation(ctx, "worker", "web_search")
	if err != nil || denial != "" {
		t.Fatalf("explicit grant rejected: denial=%q err=%v", denial, err)
	}
}

func TestOllamaWebReadOnlyAndPhaseTaxonomy(t *testing.T) {
	for _, name := range []string{"web_search", "web_fetch"} {
		if !tools.IsReadOnlyObservationTool(name) || readOnlyToolMutation(name, "") || !executionCapabilityTools[name] {
			t.Fatalf("%q must be read-only but phase-gated", name)
		}
	}
}

func TestOllamaWebFrozenGrantDoesNotWidenOnResume(t *testing.T) {
	f := newStaticGrantFixture(t, "view", nil)
	if ceiling := f.c.todoItemByID(f.todoID).DynamicToolAuthorization.StaticToolCeiling; slices.Contains(ceiling, "web_search") {
		t.Fatalf("old occurrence unexpectedly froze web_search: %v", ceiling)
	}
	// Model-visible tools were expanded only after this occurrence was created.
	// The old ceiling must prevent both exposure and the missing-key preflight.
	f.def.Tools = "view,web_search"
	for _, mode := range []WorkerToolResolutionMode{WorkerToolResolutionNormal, WorkerToolResolutionResume} {
		resolved, err := f.resolve(t, mode)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if slices.Contains(resolved.Names, "web_search") || slices.Contains(resolved.AuthorizedNames, "web_search") {
			t.Fatalf("mode %q widened frozen tool grant: %v", mode, resolved.Names)
		}
	}
}

func TestOllamaWebDefaultHelperExplicitGrant(t *testing.T) {
	for _, extra := range []string{"", "all", "web_search", "web_search,web_fetch"} {
		session, err := LoadDefaultTeam(t.TempDir(), nil, extra)
		if err != nil {
			t.Fatal(err)
		}
		def := session.Agents["helper"]
		selected := agent.SelectToolNames(tools.BuiltinToolNames(false, false), def.Tools)
		for _, name := range []string{"web_search", "web_fetch"} {
			want := extra == name || extra == "web_search,web_fetch"
			if slices.Contains(selected, name) != want {
				t.Fatalf("helper-tools %q: selected=%v, want %q=%t", extra, selected, name, want)
			}
		}
	}
}

func TestOllamaWebDirectAgentMissingKeyStopsBeforeModel(t *testing.T) {
	c, def, provider := newWorkerProviderSurfaceCoordinator(t, "ollama-web-key-preflight", "direct")
	def.Tools = "view,web_search"
	result, err := c.RunDirectAgent(t.Context(), "worker", "look up current information")
	if err == nil || !strings.Contains(err.Error(), "ollama_web_api_key_missing") {
		t.Fatalf("direct result=%+v error=%v, want missing-key preflight", result, err)
	}
	if requests := provider.providerRequests(); len(requests) != 0 {
		t.Fatalf("model received request before key preflight: %v", requests)
	}
}

func TestOllamaWebKeyIsCoordinatorOwnedAndRedacted(t *testing.T) {
	const key = "fake-ollama-web-key-for-test-only"
	t.Setenv("OLLAMA_API_KEY", key)
	workspace := t.TempDir()
	session := &TeamSession{
		Workspace: workspace, Dir: t.TempDir(),
		Config: agent.TeamConfig{Name: "web-key-test", GoalMode: "exploratory"},
	}
	c, err := newCoordinator(coordinatorParams{Session: session}, RuntimeServices{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !c.ollamaWebKeyPresent {
		t.Fatal("nonempty key was not captured at coordinator creation")
	}
	if got := c.SecretRegistry().RedactText("Bearer " + key); strings.Contains(got, key) {
		t.Fatalf("credential was not redacted: %q", got)
	}
	for _, ref := range c.SecretRegistry().References() {
		if strings.Contains(ref.Name, key) || strings.Contains(ref.Source, key) || ref.ExactValue != "" {
			t.Fatalf("secret reference exposed key: %+v", ref)
		}
	}
	cloned := cloneCoordinator(c, session)
	if !cloned.ollamaWebKeyPresent || cloned.SecretRegistry().RedactText(key) == key {
		t.Fatal("extra-model coordinator lost web credential preflight or redaction")
	}
	t.Setenv("OLLAMA_API_KEY", "")
	if !c.ollamaWebKeyPresent || c.SecretRegistry().RedactText(key) == key {
		t.Fatal("coordinator re-read changed environment instead of retaining captured key")
	}
}

func TestOllamaWebDirectAgentFakeModelAndHTTP(t *testing.T) {
	for _, test := range []struct {
		name, toolName, toolArgs, apiPath, apiResult, marker string
	}{
		{"search", "web_search", `{"query":"release news"}`, "/api/web_search", `{"results":[{"title":"News","url":"https://example.com/news","content":"Current release"}]}`, "https://example.com/news"},
		{"fetch", "web_fetch", `{"url":"https://example.com/news"}`, "/api/web_fetch", `{"title":"News","content":"Current release","links":[]}`, "Current release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			modelID := "ollama-web-end-to-end-" + test.name
			c, def, _ := newWorkerProviderSurfaceCoordinator(t, modelID, "")
			def.Tools = "view," + test.toolName
			c.session.Config.ToolsAllowed = []string{test.toolName, "submit_result"}
			c.unattended = true
			c.ollamaWebKeyPresent = true
			key := "fake-web-key-" + test.name
			registry := tools.NewSecretRegistry()
			if err := registry.Register(tools.SecretRef{Name: "ollama.web.api_key", ExactValue: key}); err != nil {
				t.Fatal(err)
			}
			c.secretRegistry = registry
			webCalls := 0
			transport := ollamaWebTransportFunc(func(req *http.Request) (*http.Response, error) {
				webCalls++
				if req.URL.Scheme != "https" || req.URL.Host != "ollama.com" || req.URL.Path != test.apiPath || req.Header.Get("Authorization") != "Bearer "+key {
					t.Errorf("unexpected hosted request: url=%s auth=%q", req.URL, req.Header.Get("Authorization"))
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.apiResult)), Header: make(http.Header)}, nil
			})
			c.coreTools = agent.BuildAllAgentTools(c.projectDir, tools.WithOllamaWebClient(ollamaweb.NewHTTPClient(key, transport)))

			modelCalls := 0
			modelSawToolResult := false
			provider := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Errorf("read model request: %v", err)
				}
				modelCalls++
				if modelCalls > 1 && strings.Contains(string(body), test.marker) {
					modelSawToolResult = true
				}
				w.Header().Set("Content-Type", "text/event-stream")
				name, arguments := test.toolName, test.toolArgs
				if modelCalls > 1 {
					name, arguments = "submit_result", `{"status":"success","summary":"verified answer"}`
				}
				// Fantasy consumes OpenAI-compatible SSE tool-call deltas.
				fmt.Fprintf(w, "data: {\"id\":\"web-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"web-call-%d\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", modelID, modelCalls, name, arguments)
				fmt.Fprint(w, "data: {\"id\":\"web-test\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			t.Cleanup(provider.Close)
			manager, err := agent.NewProviderManager(provider.URL, "", map[string]config.ProviderConfig{"ollama": {ProviderURL: provider.URL}})
			if err != nil {
				t.Fatal(err)
			}
			c.providerManager = manager
			c.modelProfileRuntime.manager = manager
			result, err := c.RunDirectAgent(t.Context(), "worker", "verify current information")
			if err != nil || result == nil {
				t.Fatalf("direct result=%+v error=%v", result, err)
			}
			if webCalls != 1 || modelCalls < 2 || !modelSawToolResult {
				t.Fatalf("fake flow incomplete: webCalls=%d modelCalls=%d sawResult=%t", webCalls, modelCalls, modelSawToolResult)
			}
			if encoded, err := json.Marshal(result); err != nil || strings.Contains(string(encoded), key) {
				t.Fatalf("result exposed credential: encoded=%s err=%v", encoded, err)
			}
		})
	}
}
