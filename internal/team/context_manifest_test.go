package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

type contextManifestCountingAgent struct{ calls int }

func (a *contextManifestCountingAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
}

func (a *contextManifestCountingAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.calls++
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
}

type failOnContextManifestSessionStore struct{ sd *SessionData }

func (s *failOnContextManifestSessionStore) SaveSession(_ string, data *SessionData) error {
	for _, item := range data.Tasks {
		if item != nil && len(item.ContextManifests) > 0 {
			return errors.New("forced context manifest checkpoint failure")
		}
	}
	return nil
}
func (s *failOnContextManifestSessionStore) SaveSessionMD(string, string) error { return nil }
func (s *failOnContextManifestSessionStore) SaveCompactionRecord(string, CompactionRecord) error {
	return nil
}
func (s *failOnContextManifestSessionStore) SessionData() *SessionData      { return s.sd }
func (s *failOnContextManifestSessionStore) SetSessionData(sd *SessionData) { s.sd = sd }

func TestContextManifestIsContentFreeAndMergesReasons(t *testing.T) {
	request := validTestContextRequest()
	compiled := CompiledContext{
		IncludedItems: []ContextItem{{ID: "current_task", Kind: "current_task", Content: "secret prompt body", Required: true, TokenCount: 3}, {ID: "context:memory-1", Kind: "observation", Content: "private memory body", Source: "shared_persistent", TokenCount: 4}},
		OmittedItems:  []ContextItem{{ID: "context:memory-2", Kind: "observation", Content: "omitted private body", Source: "shared_persistent", TokenCount: 5}},
	}
	decisions := []ContextRouteDecision{{ContextItemID: "memory-1", Included: true, Reason: ContextIncludedRelevant, BaseScore: .8, FinalScore: .9}, {ContextItemID: "memory-2", Reason: ContextOmittedPhase}}
	manifest := BuildContextInjectionManifest(request, compiled, decisions, "worker", time.Unix(100, 0))
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"secret prompt body", "private memory body", "omitted private body"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("manifest leaked content %q: %s", forbidden, text)
		}
	}
	if manifest.SchemaVersion != 1 || manifest.Fingerprint == "" || manifest.RequestHash != request.Fingerprint() {
		t.Fatalf("invalid manifest identity: %#v", manifest)
	}
	if manifest.Items[2].Reason != ContextOmittedPhase {
		t.Fatalf("router omission reason was not preserved: %#v", manifest.Items)
	}
	copyManifest := manifest
	copyManifest.CreatedAt = time.Unix(999, 0)
	if contextManifestFingerprint(copyManifest) != manifest.Fingerprint {
		t.Fatal("creation timestamp changed replay identity")
	}
}

func TestContextManifestCompilerOmissionOverridesPriorRouterInclusion(t *testing.T) {
	request := validTestContextRequest()
	compiled := CompiledContext{OmittedItems: []ContextItem{{ID: "context:memory-1", Kind: "observation", Source: "shared_persistent", TokenCount: 5}}}
	manifest := BuildContextInjectionManifest(request, compiled, []ContextRouteDecision{{ContextItemID: "memory-1", Included: true, Reason: ContextIncludedRelevant, BaseScore: .8, FinalScore: .8}}, "worker", time.Unix(100, 0))
	if len(manifest.Items) != 1 || manifest.Items[0].Included || manifest.Items[0].Reason != ContextOmittedBudget {
		t.Fatalf("compiler omission reason was overwritten by route inclusion: %#v", manifest.Items)
	}
}

func TestMemoryManifestIsProjectionOfGeneralManifestSubset(t *testing.T) {
	request := validTestContextRequest()
	compiled := CompiledContext{IncludedItems: []ContextItem{{ID: "current_task", Kind: "current_task", Required: true, TokenCount: 3}, {ID: "context:memory-a", Kind: "pattern", Source: "shared_persistent", TokenCount: 7, BaseScore: .8, FinalScore: .9}, {ID: "context:memory-b", Kind: "observation", Source: "worker_long_term", TokenCount: 5, BaseScore: .6, FinalScore: .7}}}
	general := BuildContextInjectionManifest(request, compiled, nil, "worker", time.Unix(100, 0))
	// A compiled-only item added after the general attribution boundary must
	// never leak into the legacy memory manifest. Selection is owned by the
	// general manifest; compiled context only enriches its selected IDs.
	compiled.IncludedItems = append(compiled.IncludedItems, ContextItem{ID: "context:compiled-only", Kind: "observation", Source: "shared_persistent", TokenCount: 99})
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	memory := buildMemoryInjectionManifestFromContextManifest(compiled, &general, request.RunID, request.TaskID, request.Attempt, "worker", request.RetrievalQuery(), policy)
	if memory == nil {
		t.Fatal("missing memory manifest")
	}
	var subset []ContextManifestItem
	for _, item := range general.Items {
		if item.Included && (strings.HasPrefix(item.Source, "shared_") || strings.HasPrefix(item.Source, "worker_")) {
			subset = append(subset, item)
		}
	}
	if len(subset) != len(memory.Items) {
		t.Fatalf("general subset=%#v memory=%#v", subset, memory.Items)
	}
	for i := range subset {
		if subset[i].ID != memory.Items[i].ContextItemID || subset[i].Tokens != memory.Items[i].TokenCount {
			t.Fatalf("item %d mismatch: %#v vs %#v", i, subset[i], memory.Items[i])
		}
	}
}

func TestContextManifestSummary(t *testing.T) {
	manifest := ContextInjectionManifest{Items: []ContextManifestItem{{Included: true, Tokens: 7}, {Reason: ContextOmittedBudget, Tokens: 11}}}
	summary := SummarizeContextManifests([]ContextInjectionManifest{manifest})
	if summary.Requests != 1 || summary.Included != 1 || summary.Omitted != 1 || summary.IncludedTokens != 7 || summary.OmittedTokens != 11 || summary.OmitReasons[string(ContextOmittedBudget)] != 1 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestToolFailureRecoveryManifestBindsCallAndRedactsInput(t *testing.T) {
	c := newDirectTerminationCoordinator(t, &contextManifestCountingAgent{})
	c.executionRunID = "run-tool-failure"
	ctx := context.WithValue(context.Background(), executionAttemptKey{}, 1)
	recovery, err := c.prepareToolFailureRecovery(ctx, "worker", "call-42", "bash", `{"command":"echo sk-proj-super-secret-value"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recovery, "sk-proj-super-secret-value") {
		t.Fatalf("recovery leaked raw tool input: %q", recovery)
	}
	if c.sessionData == nil || len(c.sessionData.CoordinatorContextManifests) != 1 {
		t.Fatalf("missing tool-failure manifest: %#v", c.sessionData)
	}
	manifest := c.sessionData.CoordinatorContextManifests[0]
	if manifest.Trigger != ContextTriggerToolFailure || manifest.ToolCallID != "call-42" || manifest.FailureClass != "tool_error" {
		t.Fatalf("tool-failure manifest not bound to next-turn evidence: %#v", manifest)
	}
	data, _ := json.Marshal(manifest)
	if strings.Contains(string(data), "echo") || strings.Contains(string(data), "super-secret") {
		t.Fatalf("manifest persisted raw input/output: %s", data)
	}
}

func TestAuxiliaryCompilerIsolatesHistoryAndPersistsPurpose(t *testing.T) {
	c := newDirectTerminationCoordinator(t, &contextManifestCountingAgent{})
	c.executionRunID = "run-aux"
	prompt, err := c.prepareAuxiliaryPrompt(context.Background(), "guard_reviewer", "Review candidate output api_key=raw-secret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "raw-secret") {
		t.Fatalf("auxiliary compiler did not redact prompt: %q", prompt)
	}
	if strings.Contains(prompt, "Short-term Memory") || strings.Contains(prompt, "Long-term Memory") {
		t.Fatalf("auxiliary reviewer inherited unrelated history: %q", prompt)
	}
	manifests := c.sessionData.CoordinatorContextManifests
	if len(manifests) != 1 || manifests[0].Purpose != "guard_reviewer" || !manifests[0].ModelCalled || manifests[0].Outcome != "model_call" {
		t.Fatalf("auxiliary manifest = %#v", manifests)
	}
}

func TestAuxiliaryFallbackManifestDistinguishesNoModel(t *testing.T) {
	c := newDirectTerminationCoordinator(t, &contextManifestCountingAgent{})
	c.executionRunID = "run-fallback"
	if err := c.recordAuxiliaryFallback(context.Background(), "skill_matcher", "keyword_fallback"); err != nil {
		t.Fatal(err)
	}
	manifest := c.sessionData.CoordinatorContextManifests[0]
	if manifest.ModelCalled || manifest.Outcome != "keyword_fallback" || manifest.Purpose != "skill_matcher" {
		t.Fatalf("fallback manifest = %#v", manifest)
	}
}

func TestTodoContextManifestCheckpointClone(t *testing.T) {
	list := &TodoList{items: []*TodoItem{{ID: "task-1"}}}
	manifest := ContextInjectionManifest{RequestID: "request-1", RunID: "run-1", TaskID: "task-1", Attempt: 1, Items: []ContextManifestItem{{ID: "goal", Included: true}}}
	if err := list.SetContextManifest("task-1", &manifest); err != nil {
		t.Fatal(err)
	}
	items := list.Items()
	items[0].ContextManifests[0].Items[0].ID = "mutated"
	if got := list.Items()[0].ContextManifests[0].Items[0].ID; got != "goal" {
		t.Fatalf("manifest clone leaked mutation: %q", got)
	}
}

func TestContextManifestEventReplay(t *testing.T) {
	manifest := ContextInjectionManifest{SchemaVersion: 1, RequestID: "request-1", RequestHash: "hash", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch, Fingerprint: "fingerprint"}
	retryManifest := manifest
	retryManifest.RequestID = "request-2"
	retryManifest.Attempt = 2
	retryManifest.Trigger = ContextTriggerRetry
	retryManifest.Fingerprint = "retry-fingerprint"
	created, _ := json.Marshal(map[string]any{"id": "task-1", "status": "in_progress", "agent": "worker"})
	manifestPayload, _ := json.Marshal(map[string]any{"id": "task-1", "context_manifests": []ContextInjectionManifest{manifest}})
	retryPayload, _ := json.Marshal(map[string]any{"id": "task-1", "context_manifests": []ContextInjectionManifest{retryManifest}})
	tasks := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "task-1", Payload: created}, {Type: "task_context_manifest", TaskID: "task-1", Payload: manifestPayload}, {Type: "task_context_manifest", TaskID: "task-1", Payload: retryPayload}})
	if len(tasks) != 1 || len(tasks[0].ContextManifests) != 2 || tasks[0].ContextManifests[0].Fingerprint != "fingerprint" || tasks[0].ContextManifests[1].Fingerprint != "retry-fingerprint" {
		t.Fatalf("task replay = %#v", tasks)
	}
	coordinator := manifest
	coordinator.TaskID = ""
	coordinator.RequestID = "coordinator-request"
	coordinatorPayload, _ := json.Marshal(coordinator)
	session := ReduceToSessionData([]RunEvent{{Type: "context_manifest", Payload: coordinatorPayload}})
	if len(session.CoordinatorContextManifests) != 1 || session.CoordinatorContextManifests[0].RequestID != "coordinator-request" {
		t.Fatalf("coordinator replay = %#v", session.CoordinatorContextManifests)
	}
}

func TestContextManifestKeepsConcurrentModelExecutionsDistinct(t *testing.T) {
	list := &TodoList{items: []*TodoItem{{ID: "task-1"}}}
	base := ContextInjectionManifest{SchemaVersion: 1, RequestID: "request-shared", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker", ModelExecutionID: "model-a"}
	other := base
	other.ModelExecutionID = "model-b"
	if err := list.SetContextManifest("task-1", &base); err != nil {
		t.Fatal(err)
	}
	if err := list.SetContextManifest("task-1", &other); err != nil {
		t.Fatal(err)
	}
	items := list.Items()
	if len(items[0].ContextManifests) != 2 {
		t.Fatalf("concurrent manifests were overwritten: %#v", items[0].ContextManifests)
	}
	if got := mergeContextInjectionManifests([]ContextInjectionManifest{base}, []ContextInjectionManifest{other}); len(got) != 2 {
		t.Fatalf("merge overwrote model executions: %#v", got)
	}
}

func TestContextManifestRecordsSkillDisclosureLevelWithoutSkillContent(t *testing.T) {
	request := validTestContextRequest()
	compiled := CompiledContext{IncludedItems: []ContextItem{{ID: "skill:review", Kind: "skill_summary", Content: "summary only", Required: true, TokenCount: 3}}}
	manifest := BuildContextInjectionManifest(request, compiled, nil, "worker", time.Now())
	if len(manifest.Items) != 1 || manifest.Items[0].DisclosureLevel != "1" {
		t.Fatalf("skill disclosure = %#v", manifest.Items)
	}
	data, _ := json.Marshal(manifest)
	if strings.Contains(string(data), "summary only") {
		t.Fatalf("manifest leaked skill content: %s", data)
	}
}

func TestContextRoutingStatusIncludesIdentityCountsReasonsAndFallback(t *testing.T) {
	var got StatusEvent
	c := &Coordinator{reportStatus: func(event StatusEvent) { got = event }}
	c.reportContextRouted(&ContextInjectionManifest{RequestID: "ctx-1", Attempt: 2, Trigger: ContextTriggerToolFailure, Purpose: "recovery", ModelCalled: false, Items: []ContextManifestItem{{Included: true, Tokens: 9}, {Reason: ContextOmittedPhase}, {Reason: ContextOmittedBudget}}})
	for _, want := range []string{"request=ctx-1", "attempt=2", "included=1", "tokens=9", "omitted=2", "phase_mismatch:1", "token_budget:1", "fallback/no-model"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("status %q missing %q", got.Message, want)
		}
	}
}

func TestContextManifestPersistenceFailsBeforeDirectModelCall(t *testing.T) {
	worker := &contextManifestCountingAgent{}
	c := newDirectTerminationCoordinator(t, worker)
	c.SetSessionStore(&failOnContextManifestSessionStore{})
	result, err := c.RunDirectAgent(context.Background(), "worker", "perform direct work")
	if err != nil {
		t.Fatalf("RunDirectAgent top-level error = %v, want terminal result error", err)
	}
	if result == nil || result.Error == nil || !strings.Contains(result.Error.Error(), "context manifest") {
		t.Fatalf("direct manifest failure result = %#v", result)
	}
	if worker.calls != 0 {
		t.Fatalf("model was called %d time(s) after manifest checkpoint failure", worker.calls)
	}
}

func TestLearningOffStillPersistsGeneralManifestOnly(t *testing.T) {
	worker := &contextManifestCountingAgent{}
	c := newDirectTerminationCoordinator(t, worker)
	c.session.Config.MemoryLearning.Mode = agent.MemoryLearningOff
	_, err := c.RunDirectAgent(context.Background(), "worker", "perform direct work")
	if err != nil {
		t.Fatal(err)
	}
	items := c.taskTracker.TodoList().Items()
	if worker.calls != 1 || len(items) != 1 || len(items[0].ContextManifests) != 1 {
		t.Fatalf("off-mode direct attribution = calls:%d tasks:%#v", worker.calls, items)
	}
	if len(items[0].MemoryManifests) != 0 {
		t.Fatalf("off mode persisted learning manifest: %#v", items[0].MemoryManifests)
	}
}
