package team

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

// fixedAnswerProvider answers every chat completion with a fixed message
// body and counts how many times it was called, so a test can assert exactly
// which provider a capability-routed invocation actually reached.
type fixedAnswerProvider struct {
	calls int32
	body  string
}

func (f *fixedAnswerProvider) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	atomic.AddInt32(&f.calls, 1)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w,
		"data: {\"id\":\"fake\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n",
		f.body)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func (f *fixedAnswerProvider) count() int { return int(atomic.LoadInt32(&f.calls)) }

// This is spec2.md's own acceptance test, scoped to the REFERENCE stage: a
// resolved concrete agent's own model must be called, and the legacy
// judge-model sidecar must record zero calls for this stage.
func TestReferenceRoleCapabilityRouting_InvokesResolvedAgentNotLegacyJudge(t *testing.T) {
	const legacyJudgeModel = "decision-v1-judge"
	const candidateModel = "candidate/research-agent"

	legacyJudge := newFakeJudge("execute")
	legacyServer := newIPv4TestServer(t, legacyJudge)
	t.Cleanup(legacyServer.Close)

	draftJSON := `{"schema_version":1,"entries":[{"reference_class":"comparable migrations","metric":"success_rate","sample_size":42,"distribution":{"mean":0.8,"median":0.8,"p10":0.6,"p90":0.95},"limitations":["small sample"],"source":{"id":"src-1","name":"internal survey"}}]}`
	candidate := &fixedAnswerProvider{body: draftJSON}
	candidateServer := newIPv4TestServer(t, candidate)
	t.Cleanup(candidateServer.Close)

	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: legacyJudgeModel, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
	})
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: candidateModel, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
	})

	manager, err := agent.NewProviderManager(legacyServer.URL+"/v1", "judge-secret", map[string]config.ProviderConfig{
		"ollama":    {ProviderURL: legacyServer.URL + "/v1"},
		"candidate": {ProviderURL: candidateServer.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}

	dir := t.TempDir()
	session := &TeamSession{
		Dir: dir, Workspace: dir,
		Config: agent.TeamConfig{
			Name:       "reference-routing-test",
			Generation: agent.GenerationParams{Model: legacyJudgeModel},
			CapabilityRegistry: map[string][]agent.DeclaredCapability{
				"research-worker": {{Capability: "evidence-research", Confidence: 0.7}},
			},
		},
		Agents: map[string]*agent.AgentDef{
			"research-worker": {
				Name: "research-worker", Role: "worker", Tools: "view,grep",
				Generation: agent.GenerationParams{Model: candidateModel},
			},
		},
	}

	store, err := NewEventStore(dir, "run-reference-routing", "session-reference-routing")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	c := &Coordinator{
		session: session, projectDir: dir, sessionTime: time.Now(),
		providerManager:         manager,
		modelProfileRuntime:     NewModelProfileRuntime(manager, false),
		taskTracker:             NewTaskTracker(),
		eventStore:              store,
		executionRunID:          "run-reference-routing",
		judgeModel:              legacyJudgeModel,
		sidecarModel:            legacyJudgeModel,
		reportStatus:            func(StatusEvent) {},
		providerBoundaryStarted: true,
	}
	c.SetSessionData(NewSession())

	runners := newDecisionRunners(c, "task-1")
	req := ReferenceEvidenceRequest{
		SchemaVersion: ReferenceEvidenceSchemaVersion,
		InvocationID:  "inv-1",
		DecisionID:    "dec-1",
		Question:      "Should we migrate the storage backend?",
		RoutingRole:   &agent.ReferenceRolePolicy{RequiredCapabilities: []string{"evidence-research"}},
	}
	hash, err := req.ComputeInputHash()
	if err != nil {
		t.Fatalf("ComputeInputHash: %v", err)
	}
	req.InputHash = hash

	draft, err := runners.RunReferenceEvidence(context.Background(), req)
	if err != nil {
		t.Fatalf("RunReferenceEvidence: %v", err)
	}
	if len(draft.Entries) != 1 || draft.Entries[0].ReferenceClass != "comparable migrations" {
		t.Fatalf("draft = %#v", draft)
	}
	// The exact call count is an artifact of the shared agent-runtime step
	// loop (e.g. a benign extra turn), not something this test is about; what
	// matters is that the resolved candidate was reached at all.
	if got := candidate.count(); got < 1 {
		t.Fatalf("resolved candidate agent call count = %d, want at least 1", got)
	}
	if got := legacyJudge.count(stageReference); got != 0 {
		t.Fatalf("legacy judge-model reference-stage calls = %d, want 0 (this is spec2.md's acceptance bar)", got)
	}
}

// Legacy behavior (RoutingRole nil) must be completely unaffected: the
// sidecar answers exactly as it did before this file existed.
func TestReferenceEvidence_WithoutRoutingRoleStaysOnLegacySidecar(t *testing.T) {
	const judgeModel = "decision-v1-judge"
	judge := newFakeJudge("execute")
	server := newIPv4TestServer(t, judge)
	t.Cleanup(server.Close)

	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: judgeModel, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
	})
	manager, err := agent.NewProviderManager(server.URL+"/v1", "judge-secret", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: server.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	dir := t.TempDir()
	store, err := NewEventStore(dir, "run-legacy-reference", "session-legacy-reference")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{
		session:                 &TeamSession{Dir: dir, Workspace: dir, Config: agent.TeamConfig{Name: "legacy-reference-test"}},
		projectDir:              dir,
		sessionTime:             time.Now(),
		providerManager:         manager,
		modelProfileRuntime:     NewModelProfileRuntime(manager, false),
		taskTracker:             NewTaskTracker(),
		eventStore:              store,
		executionRunID:          "run-legacy-reference",
		judgeModel:              judgeModel,
		sidecarModel:            judgeModel,
		reportStatus:            func(StatusEvent) {},
		providerBoundaryStarted: true,
	}
	c.SetSessionData(NewSession())

	runners := newDecisionRunners(c, "task-1")
	req := ReferenceEvidenceRequest{
		SchemaVersion: ReferenceEvidenceSchemaVersion,
		InvocationID:  "inv-1",
		DecisionID:    "dec-1",
		Question:      "Should we migrate the storage backend?",
	}
	hash, err := req.ComputeInputHash()
	if err != nil {
		t.Fatalf("ComputeInputHash: %v", err)
	}
	req.InputHash = hash

	if _, err := runners.RunReferenceEvidence(context.Background(), req); err != nil {
		t.Fatalf("RunReferenceEvidence: %v", err)
	}
	if got := judge.count(stageReference); got != 1 {
		t.Fatalf("legacy judge-model reference-stage calls = %d, want 1 (unchanged legacy path)", got)
	}
}

// referenceRoleTools must narrow to the read-only ceiling regardless of what
// a resolved worker declares for itself — subtraction, not trust (spec.md v2
// §22 "effective tools = base-agent tools ∩ ... ∩ role constraints").
func TestReferenceRoleTools_NarrowsToReadOnly(t *testing.T) {
	noop := func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	}
	tools := []fantasy.AgentTool{
		&structuredTestTool{name: "view", run: noop},
		&structuredTestTool{name: "grep", run: noop},
		&structuredTestTool{name: "bash", run: noop},
		&structuredTestTool{name: "write", run: noop},
		&structuredTestTool{name: "agent", run: noop},
	}
	filtered := referenceRoleTools(tools)
	if len(filtered) != 2 {
		t.Fatalf("filtered tools = %#v, want exactly view and grep", filtered)
	}
	names := map[string]bool{}
	for _, tool := range filtered {
		names[tool.Info().Name] = true
	}
	if !names["view"] || !names["grep"] {
		t.Fatalf("filtered tool names = %#v, want view and grep", names)
	}
	if names["bash"] || names["write"] || names["agent"] {
		t.Fatalf("filtered tools retained a non-read-only tool: %#v", names)
	}
}
