package team

import (
	"context"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

const validJudgeResponseJSON = `{"option_scores":[{"option_id":"execute","criteria":{},"overall":8}],"preferred_option":"execute","success_probability":0.6,"confidence":0.7}`

// judgeRoutingHarness builds a Coordinator wired for judge-role capability
// routing tests: three candidate agents with distinct declared confidence
// (so ranking is deterministic: c > b > a) each on their own fake provider,
// plus the legacy judge-model sidecar on a separate fake so a test can assert
// it was never reached.
type judgeRoutingHarness struct {
	coordinator *Coordinator
	legacyJudge *fakeJudge
	candA       *fixedAnswerProvider
	candB       *fixedAnswerProvider
	candC       *fixedAnswerProvider
}

func newJudgeRoutingHarness(t *testing.T) *judgeRoutingHarness {
	t.Helper()
	const legacyJudgeModel = "decision-v1-judge"

	legacyJudge := newFakeJudge("execute")
	legacyServer := newIPv4TestServer(t, legacyJudge)
	t.Cleanup(legacyServer.Close)

	candA := &fixedAnswerProvider{body: validJudgeResponseJSON}
	candAServer := newIPv4TestServer(t, candA)
	t.Cleanup(candAServer.Close)
	candB := &fixedAnswerProvider{body: validJudgeResponseJSON}
	candBServer := newIPv4TestServer(t, candB)
	t.Cleanup(candBServer.Close)
	candC := &fixedAnswerProvider{body: validJudgeResponseJSON}
	candCServer := newIPv4TestServer(t, candC)
	t.Cleanup(candCServer.Close)

	for _, modelID := range []string{legacyJudgeModel, "cand-a/model", "cand-b/model", "cand-c/model"} {
		GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
			ModelID: modelID, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
		})
	}

	manager, err := agent.NewProviderManager(legacyServer.URL+"/v1", "judge-secret", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: legacyServer.URL + "/v1"},
		"cand-a": {ProviderURL: candAServer.URL + "/v1"},
		"cand-b": {ProviderURL: candBServer.URL + "/v1"},
		"cand-c": {ProviderURL: candCServer.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}

	dir := t.TempDir()
	session := &TeamSession{
		Dir: dir, Workspace: dir,
		Config: agent.TeamConfig{
			Name:       "judge-routing-test",
			Generation: agent.GenerationParams{Model: legacyJudgeModel},
			CapabilityRegistry: map[string][]agent.DeclaredCapability{
				"judge-cand-a": {{Capability: "decision-analysis", Confidence: 0.5}},
				"judge-cand-b": {{Capability: "decision-analysis", Confidence: 0.6}},
				"judge-cand-c": {{Capability: "decision-analysis", Confidence: 0.7}},
			},
		},
		Agents: map[string]*agent.AgentDef{
			"judge-cand-a": {Name: "judge-cand-a", Role: "worker", Tools: "view,bash", Generation: agent.GenerationParams{Model: "cand-a/model"}},
			"judge-cand-b": {Name: "judge-cand-b", Role: "worker", Generation: agent.GenerationParams{Model: "cand-b/model"}},
			"judge-cand-c": {Name: "judge-cand-c", Role: "worker", Generation: agent.GenerationParams{Model: "cand-c/model"}},
		},
	}

	store, err := NewEventStore(dir, "run-judge-routing", "session-judge-routing")
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
		executionRunID:          "run-judge-routing",
		judgeModel:              legacyJudgeModel,
		sidecarModel:            legacyJudgeModel,
		reportStatus:            func(StatusEvent) {},
		providerBoundaryStarted: true,
	}
	c.SetSessionData(NewSession())

	return &judgeRoutingHarness{coordinator: c, legacyJudge: legacyJudge, candA: candA, candB: candB, candC: candC}
}

func judgeRequestFor(judgeID string, role *agent.JudgeRolePolicy) JudgeRequest {
	// The prompt text must contain the marker classifyDecisionStagePrompt
	// (decision_fake_judge_test.go) looks for, so the legacy-sidecar fake
	// correctly attributes a call to stageJudge when this test exercises the
	// unrouted path.
	return JudgeRequest{
		DecisionID:  "dec-1",
		Round:       1,
		JudgeID:     judgeID,
		Context:     JudgeContext{Prompt: "You are forming an independent judgment for " + judgeID + "."},
		RoutingRole: role,
	}
}

// This is spec2.md's own acceptance test for JUDGE: each judge ID's provider
// call must be attributed to its own distinct resolved candidate (ranked by
// declared capability), and the legacy judge-model sidecar must record zero
// calls.
func TestJudgeRoleCapabilityRouting_RoutesEachJudgeToADistinctAgent(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	for judgeID := range 3 {
		_, err := runners.RunJudge(context.Background(), judgeRequestFor(fmtJudgeID(judgeID+1), role))
		if err != nil {
			t.Fatalf("RunJudge(%s): %v", fmtJudgeID(judgeID+1), err)
		}
	}

	// Ranked by declared confidence: c (0.7) > b (0.6) > a (0.5), so
	// judge-1 -> cand-c, judge-2 -> cand-b, judge-3 -> cand-a.
	if got := h.candC.count(); got < 1 {
		t.Fatalf("judge-1's resolved candidate (cand-c) call count = %d, want at least 1", got)
	}
	if got := h.candB.count(); got < 1 {
		t.Fatalf("judge-2's resolved candidate (cand-b) call count = %d, want at least 1", got)
	}
	if got := h.candA.count(); got < 1 {
		t.Fatalf("judge-3's resolved candidate (cand-a) call count = %d, want at least 1", got)
	}
	if got := h.legacyJudge.count(stageJudge); got != 0 {
		t.Fatalf("legacy judge-model JUDGE-stage calls = %d, want 0 (this is spec2.md's acceptance bar)", got)
	}
}

func fmtJudgeID(n int) string {
	switch n {
	case 1:
		return "judge-1"
	case 2:
		return "judge-2"
	case 3:
		return "judge-3"
	default:
		panic("unsupported judge ordinal in test helper")
	}
}

// Legacy behavior (RoutingRole nil) must be completely unaffected.
func TestJudgeRoleCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")

	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", nil)); err != nil {
		t.Fatalf("RunJudge: %v", err)
	}
	if got := h.legacyJudge.count(stageJudge); got != 1 {
		t.Fatalf("legacy judge-model JUDGE-stage calls = %d, want 1 (unchanged legacy path)", got)
	}
	if got := h.candA.count() + h.candB.count() + h.candC.count(); got != 0 {
		t.Fatalf("capability-routed candidates were called (%d) when RoutingRole was nil", got)
	}
}

// A pool smaller than judge-role.min-distinct-agents must fail closed before
// any judge is dispatched, rather than silently repeating one agent.
func TestJudgeRoleCapabilityRouting_FailsClosedWhenPoolTooSmall(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"a-capability-nobody-declares"},
		MinDistinctAgents:    1,
	}
	_, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role))
	if err == nil {
		t.Fatal("want an error when the qualified pool is empty, got nil")
	}
	if got := h.legacyJudge.count(stageJudge); got != 0 {
		t.Fatalf("legacy judge-model JUDGE-stage calls = %d, want 0 (must fail closed, not silently fall back)", got)
	}
}

// judgeRoleZeroTools must stay empty regardless of what a resolved worker
// declares for itself: a judge reasons only from the sealed evidence packet.
func TestJudgeRoleZeroTools_IsAlwaysEmpty(t *testing.T) {
	if len(judgeRoleZeroTools) != 0 {
		t.Fatalf("judgeRoleZeroTools = %#v, want empty", judgeRoleZeroTools)
	}
}

// judgeOrdinal must parse the well-formed "judge-N" shape decision_engine.go
// actually produces, and reject anything else rather than silently defaulting
// to some ordinal.
func TestJudgeOrdinal(t *testing.T) {
	cases := []struct {
		id      string
		want    int
		wantErr bool
	}{
		{id: "judge-1", want: 1},
		{id: "judge-42", want: 42},
		{id: "judge-0", wantErr: true},
		{id: "judge--1", wantErr: true},
		{id: "not-a-judge-id", wantErr: true},
		{id: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got, err := judgeOrdinal(tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("judgeOrdinal(%q) = %d, want error", tc.id, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("judgeOrdinal(%q) error = %v", tc.id, err)
			}
			if got != tc.want {
				t.Fatalf("judgeOrdinal(%q) = %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}
