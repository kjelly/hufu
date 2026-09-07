package team

import (
	"context"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

// A pin (spec.md v2 §34) forces every ordinal of a role to the same named
// agent, but only after that agent still clears authorization and required-
// capability checks — it can never bypass them.

func TestJudgeRoleCapabilityRouting_PinForcesEveryOrdinalToTheSameAgent(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		Pin:                  &agent.RoutingPin{Agent: "judge-cand-a", Reason: "regulatory requirement"},
	}

	// Without the pin, judge-1 would rank to cand-c (highest confidence) and
	// judge-2 to cand-b. With the pin, both must land on cand-a instead.
	opinion1, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-1): %v", err)
	}
	opinion2, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-2", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-2): %v", err)
	}
	for _, opinion := range []DecisionOpinion{opinion1, opinion2} {
		if opinion.AgentID != "judge-cand-a" {
			t.Fatalf("opinion.AgentID = %q, want %q (pinned)", opinion.AgentID, "judge-cand-a")
		}
		if !opinion.Pinned {
			t.Fatal("opinion.Pinned = false, want true")
		}
		if opinion.BindingReason != "regulatory requirement" {
			t.Fatalf("opinion.BindingReason = %q, want %q", opinion.BindingReason, "regulatory requirement")
		}
	}
	if got := h.candB.count() + h.candC.count(); got != 0 {
		t.Fatalf("non-pinned candidates were called (%d calls), want 0", got)
	}
	if got := h.candA.count(); got < 2 {
		t.Fatalf("cand-a call count = %d, want at least 2 (both judges)", got)
	}
}

func TestJudgeRoleCapabilityRouting_PinFailsClosedWhenAgentMissingRequiredCapability(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"adversarial-analysis"}, // cand-a does not declare this
		Pin:                  &agent.RoutingPin{Agent: "judge-cand-a", Reason: "regulatory requirement"},
	}
	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role)); err == nil {
		t.Fatal("want an error when the pinned agent does not satisfy the required capability")
	}
	if got := h.candA.count(); got != 0 {
		t.Fatalf("pinned agent should never be invoked when disqualified, got %d calls", got)
	}
}

func TestJudgeRoleCapabilityRouting_PinFailsClosedWhenAgentNotAuthorized(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	h.coordinator.session.Config.Delegation.AllowedWorkers = []string{"judge-cand-b", "judge-cand-c"}
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		Pin:                  &agent.RoutingPin{Agent: "judge-cand-a", Reason: "regulatory requirement"},
	}
	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role)); err == nil {
		t.Fatal("want an error when the pinned agent is not in delegation.allowed-workers")
	}
	if got := h.candA.count(); got != 0 {
		t.Fatalf("unauthorized pinned agent should never be invoked, got %d calls", got)
	}
}

func TestJudgeRoleCapabilityRouting_PinFailsClosedWhenAgentNotConfigured(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		Pin:                  &agent.RoutingPin{Agent: "no-such-agent", Reason: "regulatory requirement"},
	}
	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role)); err == nil {
		t.Fatal("want an error when the pinned agent does not exist")
	}
}

// REVISE reusing a pinned original binding must still resolve to the pinned
// agent and carry the pinned/reason markers forward.
func TestRevisionCapabilityRouting_ReusesPinnedOriginalBinding(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		Pin:                  &agent.RoutingPin{Agent: "judge-cand-a", Reason: "regulatory requirement"},
	}
	opinion, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role))
	if err != nil {
		t.Fatalf("RunJudge: %v", err)
	}

	revisionReq := RevisionRequest{
		DecisionID: "dec-1", JudgeID: "judge-1", Original: opinion,
		Prompt: "You are judge judge-1 revising your own judgment once (round 2).", RoutingRole: role,
	}
	revision, err := runners.RunRevision(context.Background(), revisionReq)
	if err != nil {
		t.Fatalf("RunRevision: %v", err)
	}
	if revision.AgentID != "judge-cand-a" {
		t.Fatalf("revision.AgentID = %q, want %q", revision.AgentID, "judge-cand-a")
	}
	if !revision.Pinned || revision.BindingReason != "regulatory requirement" {
		t.Fatalf("revision.Pinned=%v BindingReason=%q, want true / %q", revision.Pinned, revision.BindingReason, "regulatory requirement")
	}
}

func TestChallengeRoleCapabilityRouting_PinForcesEveryOrdinalToTheSameAgent(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.ChallengeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		Pin:                  &agent.RoutingPin{Agent: "judge-cand-b", Reason: "licensed specialist"},
	}
	challenge1, err := runners.RunChallenge(context.Background(), challengeRequestFor("challenger-1", role))
	if err != nil {
		t.Fatalf("RunChallenge(challenger-1): %v", err)
	}
	challenge2, err := runners.RunChallenge(context.Background(), challengeRequestFor("challenger-2", role))
	if err != nil {
		t.Fatalf("RunChallenge(challenger-2): %v", err)
	}
	for _, challenge := range []DecisionChallenge{challenge1, challenge2} {
		if challenge.AgentID != "judge-cand-b" {
			t.Fatalf("challenge.AgentID = %q, want %q (pinned)", challenge.AgentID, "judge-cand-b")
		}
		if !challenge.Pinned || challenge.BindingReason != "licensed specialist" {
			t.Fatalf("challenge.Pinned=%v BindingReason=%q, want true / %q", challenge.Pinned, challenge.BindingReason, "licensed specialist")
		}
	}
	if got := h.candA.count() + h.candC.count(); got != 0 {
		t.Fatalf("non-pinned candidates were called (%d calls), want 0", got)
	}
}

// The REFERENCE role's pin resolution shares resolvePinnedCandidate with
// judge/challenge, but has its own call site (no ordinal, no round-robin) —
// prove it independently, against a higher-ranked non-pinned alternative.
func TestReferenceRoleCapabilityRouting_PinOverridesRanking(t *testing.T) {
	const legacyJudgeModel = "decision-v1-judge"
	legacyJudge := newFakeJudge("execute")
	legacyServer := newIPv4TestServer(t, legacyJudge)
	t.Cleanup(legacyServer.Close)

	draftJSON := `{"schema_version":1,"entries":[{"reference_class":"comparable migrations","metric":"success_rate","sample_size":42,"distribution":{"mean":0.8,"median":0.8,"p10":0.6,"p90":0.95},"limitations":["small sample"],"source":{"id":"src-1","name":"internal survey"}}]}`
	pinnedAgent := &fixedAnswerProvider{body: draftJSON}
	pinnedServer := newIPv4TestServer(t, pinnedAgent)
	t.Cleanup(pinnedServer.Close)
	topRanked := &fixedAnswerProvider{body: draftJSON}
	topRankedServer := newIPv4TestServer(t, topRanked)
	t.Cleanup(topRankedServer.Close)

	for _, modelID := range []string{legacyJudgeModel, "pinned/model", "top-ranked/model"} {
		GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
			ModelID: modelID, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
		})
	}
	manager, err := agent.NewProviderManager(legacyServer.URL+"/v1", "judge-secret", map[string]config.ProviderConfig{
		"ollama":     {ProviderURL: legacyServer.URL + "/v1"},
		"pinned":     {ProviderURL: pinnedServer.URL + "/v1"},
		"top-ranked": {ProviderURL: topRankedServer.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}

	dir := t.TempDir()
	session := &TeamSession{
		Dir: dir, Workspace: dir,
		Config: agent.TeamConfig{
			Name:       "reference-pin-test",
			Generation: agent.GenerationParams{Model: legacyJudgeModel},
			CapabilityRegistry: map[string][]agent.DeclaredCapability{
				"pinned-worker":     {{Capability: "evidence-research", Confidence: 0.5}},
				"top-ranked-worker": {{Capability: "evidence-research", Confidence: 0.9}},
			},
		},
		Agents: map[string]*agent.AgentDef{
			"pinned-worker":     {Name: "pinned-worker", Role: "worker", Tools: "view,grep", Generation: agent.GenerationParams{Model: "pinned/model"}},
			"top-ranked-worker": {Name: "top-ranked-worker", Role: "worker", Tools: "view,grep", Generation: agent.GenerationParams{Model: "top-ranked/model"}},
		},
	}
	store, err := NewEventStore(dir, "run-reference-pin", "session-reference-pin")
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
		executionRunID:          "run-reference-pin",
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
		RoutingRole: &agent.ReferenceRolePolicy{
			RequiredCapabilities: []string{"evidence-research"},
			Pin:                  &agent.RoutingPin{Agent: "pinned-worker", Reason: "user explicitly requested named agent"},
		},
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
	if draft.AgentID != "pinned-worker" {
		t.Fatalf("draft.AgentID = %q, want %q (pin must override the higher-ranked candidate)", draft.AgentID, "pinned-worker")
	}
	if !draft.Pinned || draft.BindingReason != "user explicitly requested named agent" {
		t.Fatalf("draft.Pinned=%v BindingReason=%q, want true / %q", draft.Pinned, draft.BindingReason, "user explicitly requested named agent")
	}
	if got := topRanked.count(); got != 0 {
		t.Fatalf("higher-ranked non-pinned candidate was called (%d calls), want 0", got)
	}
	if got := pinnedAgent.count(); got < 1 {
		t.Fatalf("pinned candidate call count = %d, want at least 1", got)
	}
}
