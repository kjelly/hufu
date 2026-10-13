package team

import (
	"context"
	json "encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

func TestContextQueryProductionCompactionPreservesReceiverManifest(t *testing.T) {
	model := "query-compaction-provider"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, model, "sub-agent")
	// Leave room for the required result schema and protected prompt; the
	// uncompressed history still exceeds this explicitly declared window.
	def.Generation.ContextWindow = 16384
	c.session.Config.Compaction = agent.DefaultCompactionPolicy()
	c.session.Config.Compaction.VerifiedHistoryTargetTokens = 2000
	c.session.Config.ContextRouting.QueryMode = "active"
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy = state
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	task := TaskDef{Agent: "worker", Goal: "perform compacted work", Execution: ExecutionContract{RequiresResult: true}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: task.Goal, Execution: task.Execution}})[0]
	identity := submitResultRuntimeIdentity{RunID: c.contextRunID(), TaskID: item.ID, Attempt: 1, Agent: "worker"}
	c.openTaskOccurrence(identity)
	ctx := context.WithValue(t.Context(), todoIDKey{}, item.ID)
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	request := c.newTaskContextRequest(task, item.ID, 1, ContextTriggerTaskDispatch, "worker", "worker", nil)
	compiled, err := c.ContextCompiler().CompileWorkerContext(ctx, WorkerContextInput{Request: request, Goal: task.Goal, AgentInstructions: "complete the compacted task", TaskDef: task, AgentDef: def, ModelContext: ModelContextSpec{ModelID: model, ContextWindow: 16384, MaxOutputTokens: 128, SafetyMarginTokens: 32}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(request, compiled, nil, "worker", time.Now(), c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.persistContextManifest(&manifest, ctx); err != nil {
		t.Fatal(err)
	}
	resolved, err := c.ToolResolver().ResolveTaskTools(ctx, def, WorkerToolResolutionRequest{Task: task, TodoID: item.ID, Mode: WorkerToolResolutionNormal})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.startProviderExecutionBoundary(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.stopProviderExecutionBoundary()
	history := []fantasy.Message{fantasy.NewUserMessage("historical task anchor")}
	for range 40 {
		history = append(history, fantasy.NewUserMessage(strings.Repeat("old historical detail ", 300)))
	}
	result, err := NewHufuLocalSubagentProvider(c).RunAttempt(ctx, AttemptRequest{RunID: c.contextRunID(), TaskID: item.ID, Attempt: 1, Agent: def, Task: task, Prompt: compiled.Prompt, ModelID: model, MaxSteps: 4, Tools: resolved, History: history})
	if err != nil || result.TypedResult == nil {
		t.Fatalf("compacted provider attempt: %v", err)
	}
	assertWorkerProviderRequiredContext(t, capture, task.Goal)
	events, err := c.eventStore.QueryEvents(EventQuery{Types: []string{string(EventContextWindowAdmission)}})
	if err != nil {
		t.Fatal(err)
	}
	compacted := false
	for _, event := range events {
		var telemetry ContextWindowTelemetryEvent
		// Worker projection compaction does not increment the coordinator's
		// durable compaction counter. Its admission decision records the work.
		if json.Unmarshal(event.Payload, &telemetry) == nil && (telemetry.Decision == string(ContextWindowCompactPreTurn) || telemetry.Decision == string(ContextWindowCompactMidTurn)) {
			compacted = true
		}
	}
	if !compacted {
		t.Fatal("provider did not pass transient worker compaction")
	}
	if !strings.Contains(capture.contexts[0], verifiedHistoryPrefix) {
		t.Fatal("receiver did not receive compacted history")
	}
	if manifest.SchemaVersion != 3 || manifest.Query == nil || !supportedDeliveryContextManifest(manifest) {
		t.Fatal("compaction lost frozen active query manifest")
	}
}

func TestContextQueryProductionProviderFallbackKeepsCanonicalManifests(t *testing.T) {
	primary, fallback := "query-route-primary", "query-route-fallback"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, primary, "direct")
	capture.rateLimitModel = primary
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: fallback, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})
	c.session.Config.ContextRouting.QueryMode = "active"
	def.ExecutionRoute = "query-route"
	c.session.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"query-route": {Candidates: []string{"ollama/" + primary, "ollama/" + fallback}, FallbackOn: []string{"rate_limited"}}}
	if err := c.bindExecutionRoutes(); err != nil {
		t.Fatal(err)
	}
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.executionPolicy = state
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunDirectAgent(t.Context(), "worker", "perform fallback work"); err != nil {
		t.Fatal(err)
	}
	assertWorkerProviderRequiredContext(t, capture, "perform fallback work")
	if c.DualWriteFailures() != 0 {
		t.Fatal("fallback lifecycle projection differs from canonical events")
	}
	if !slices.Equal(capture.models, []string{primary, fallback}) {
		t.Fatalf("provider targets: %v", capture.models)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskDone || len(items[0].ExecutionReceipts) != 2 || len(items[0].ContextManifests) != 2 {
		t.Fatalf("fallback provenance: %+v", items)
	}
	for _, manifest := range items[0].ContextManifests {
		if manifest.SchemaVersion != 3 || manifest.Query == nil || !supportedDeliveryContextManifest(manifest) {
			t.Fatal("fallback lost active query identity")
		}
	}
	if items[0].ContextManifests[0].RequestID == items[0].ContextManifests[1].RequestID {
		t.Fatal("fallback reused primary request identity")
	}
}

func TestContextQueryProductionDirectAndNestedProviderPaths(t *testing.T) {
	for _, path := range []string{"direct", "nested"} {
		t.Run(path, func(t *testing.T) {
			mode := "direct"
			if path == "nested" {
				mode = "sub-agent"
			}
			c, _, capture := newWorkerProviderSurfaceCoordinator(t, "query-production-"+path, mode)
			c.session.Config.ContextRouting.QueryMode = "active"
			state, err := newExecutionPolicyState(c)
			if err != nil {
				t.Fatal(err)
			}
			c.executionPolicy = state
			if err := c.AdmitExecutionPolicy(); err != nil {
				t.Fatal(err)
			}
			goal := "perform delegated work"
			if path == "direct" {
				if _, err := c.RunDirectAgent(t.Context(), "worker", goal); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.startProviderExecutionBoundary(t.Context()); err != nil {
					t.Fatal(err)
				}
				defer c.stopProviderExecutionBoundary()
				response, err := (&requestAgentTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Name: "request_agent", Input: `{"goal":"perform delegated work","agent":"worker"}`})
				if err != nil || response.IsError {
					t.Fatalf("nested: %v %+v", err, response)
				}
			}
			assertWorkerProviderRequiredContext(t, capture, goal)
			items := c.taskTracker.TodoList().Items()
			if len(items) != 1 || items[0].Status != TaskDone || len(items[0].ContextManifests) == 0 {
				t.Fatal("missing completed occurrence/manifest")
			}
			for _, manifest := range items[0].ContextManifests {
				if !supportedDeliveryContextManifest(manifest) || manifest.SchemaVersion != 3 || manifest.Query == nil || manifest.Query.Mode != QueryModeLegacy {
					t.Fatal("no-hints provider path lost frozen query fallback attribution")
				}
			}
		})
	}
}
