package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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
				// architecture is irrelevant to every test that only queries
				// decision-analysis; it exists so routing-hint tests have a
				// preferred capability that can flip cand-a from last place
				// to first.
				"judge-cand-a": {
					{Capability: "decision-analysis", Confidence: 0.5},
					{Capability: "architecture", Confidence: 0.9},
				},
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

// modelSharingJudgeRoutingHarness mirrors newJudgeRoutingHarness's shape but
// gives two candidates the *same* declared model (and hence the same
// provider, since a model ID's provider prefix determines routing) so a
// test can distinguish "prefer distinct models" reordering from plain
// rank-order round-robin (newJudgeRoutingHarness's three candidates each
// already declare a distinct model, which would make that distinction
// untestable). Ranked by decision-analysis confidence: x (0.7) > y (0.6) >
// z (0.5). x and y share model+provider "shared/model" (and hence one fake
// server, distinguishable only by AgentID, not call count); z has its own
// "distinct/model" on its own fake server.
func modelSharingJudgeRoutingHarness(t *testing.T) *judgeRoutingHarness {
	t.Helper()
	const legacyJudgeModel = "decision-v1-judge"

	legacyJudge := newFakeJudge("execute")
	legacyServer := newIPv4TestServer(t, legacyJudge)
	t.Cleanup(legacyServer.Close)

	sharedProvider := &fixedAnswerProvider{body: validJudgeResponseJSON}
	sharedServer := newIPv4TestServer(t, sharedProvider)
	t.Cleanup(sharedServer.Close)
	distinctProvider := &fixedAnswerProvider{body: validJudgeResponseJSON}
	distinctServer := newIPv4TestServer(t, distinctProvider)
	t.Cleanup(distinctServer.Close)

	for _, modelID := range []string{legacyJudgeModel, "shared/model", "distinct/model"} {
		GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
			ModelID: modelID, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
		})
	}
	manager, err := agent.NewProviderManager(legacyServer.URL+"/v1", "judge-secret", map[string]config.ProviderConfig{
		"ollama":   {ProviderURL: legacyServer.URL + "/v1"},
		"shared":   {ProviderURL: sharedServer.URL + "/v1"},
		"distinct": {ProviderURL: distinctServer.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}

	dir := t.TempDir()
	session := &TeamSession{
		Dir: dir, Workspace: dir,
		Config: agent.TeamConfig{
			Name:       "model-sharing-judge-routing-test",
			Generation: agent.GenerationParams{Model: legacyJudgeModel},
			CapabilityRegistry: map[string][]agent.DeclaredCapability{
				"judge-cand-x": {{Capability: "decision-analysis", Confidence: 0.7}},
				"judge-cand-y": {{Capability: "decision-analysis", Confidence: 0.6}},
				"judge-cand-z": {{Capability: "decision-analysis", Confidence: 0.5}},
			},
		},
		Agents: map[string]*agent.AgentDef{
			"judge-cand-x": {Name: "judge-cand-x", Role: "worker", Generation: agent.GenerationParams{Model: "shared/model"}},
			"judge-cand-y": {Name: "judge-cand-y", Role: "worker", Generation: agent.GenerationParams{Model: "shared/model"}},
			"judge-cand-z": {Name: "judge-cand-z", Role: "worker", Generation: agent.GenerationParams{Model: "distinct/model"}},
		},
	}

	store, err := NewEventStore(dir, "run-model-sharing-judge-routing", "session-model-sharing-judge-routing")
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
		executionRunID:          "run-model-sharing-judge-routing",
		judgeModel:              legacyJudgeModel,
		sidecarModel:            legacyJudgeModel,
		reportStatus:            func(StatusEvent) {},
		providerBoundaryStarted: true,
	}
	c.SetSessionData(NewSession())

	// candB is unused by these tests (x and y share sharedProvider, so
	// per-candidate call counts can't distinguish them — AgentID does that).
	return &judgeRoutingHarness{coordinator: c, legacyJudge: legacyJudge, candA: sharedProvider, candC: distinctProvider}
}

// PreferDistinctModels must move a lower-ranked, model-diverse candidate
// ahead of a higher-ranked one that repeats an already-used model.
func TestJudgeRoleCapabilityRouting_PreferDistinctModelsChangesBinding(t *testing.T) {
	h := modelSharingJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		PreferDistinctModels: true,
	}

	// Without the preference, judge-1 -> cand-x, judge-2 -> cand-y (both
	// "shared/model"). With it, judge-2 should move to cand-z instead
	// ("distinct/model"), since cand-y only repeats judge-1's model.
	opinion1, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-1): %v", err)
	}
	opinion2, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-2", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-2): %v", err)
	}
	if opinion1.AgentID != "judge-cand-x" {
		t.Fatalf("judge-1 opinion.AgentID = %q, want %q", opinion1.AgentID, "judge-cand-x")
	}
	if opinion2.AgentID != "judge-cand-z" {
		t.Fatalf("judge-2 opinion.AgentID = %q, want %q (prefer-distinct-models should skip the same-model repeat)", opinion2.AgentID, "judge-cand-z")
	}
	if got := h.candC.count(); got < 1 {
		t.Fatalf("cand-z (distinct model) call count = %d, want at least 1", got)
	}
}

// Without prefer-distinct-models, ranking is completely unaffected — plain
// round-robin over the rank order, same as before this feature existed.
func TestJudgeRoleCapabilityRouting_WithoutPreferDistinctModelsIsUnaffected(t *testing.T) {
	h := modelSharingJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	opinion2, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-2", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-2): %v", err)
	}
	if opinion2.AgentID != "judge-cand-y" {
		t.Fatalf("judge-2 opinion.AgentID = %q, want %q (plain rank order, unaffected)", opinion2.AgentID, "judge-cand-y")
	}
}

func TestJudgeRoleCapabilityRouting_HardModelFloorPlansAllOrdinals(t *testing.T) {
	h := modelSharingJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctModels: 2}

	first, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-1): %v", err)
	}
	second, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-2", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-2): %v", err)
	}
	if first.AgentID != "judge-cand-x" || second.AgentID != "judge-cand-z" {
		t.Fatalf("bindings = [%q %q], want [judge-cand-x judge-cand-z]", first.AgentID, second.AgentID)
	}
	if got := judgeDiversitySummary([]DecisionOpinion{first, second}); got == nil || got.DistinctModelCount != 2 {
		t.Fatalf("judge diversity = %#v, want two models", got)
	}
}

func TestJudgeRoleCapabilityRouting_HardProviderFloorUsesCanonicalProvider(t *testing.T) {
	h := modelSharingJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}, MinDistinctProviders: 2}

	first, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-1): %v", err)
	}
	second, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-2", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-2): %v", err)
	}
	if first.AgentID != "judge-cand-x" || second.AgentID != "judge-cand-z" {
		t.Fatalf("bindings = [%q %q], want [judge-cand-x judge-cand-z]", first.AgentID, second.AgentID)
	}
	if first.Provider != "shared" || second.Provider != "distinct" {
		t.Fatalf("providers = [%q %q], want canonical [shared distinct]", first.Provider, second.Provider)
	}
}

func TestJudgeRoleCapabilityRouting_ProfileSyncFailurePrecedesProviderCall(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	configureEventStoreSyncFailureForEventType(t, h.coordinator.eventStore, string(EventModelProfileResolved), 1, fmt.Errorf("injected decision profile sync failure"))
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}
	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role)); err == nil {
		t.Fatal("expected model profile persistence failure")
	}
	if calls := h.candA.count() + h.candB.count() + h.candC.count(); calls != 0 {
		t.Fatalf("provider calls after profile sync failure = %d, want 0", calls)
	}
}

func TestDecisionRoutingDurableIdentityMatchesModelProfileTelemetry(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	judgeRole := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}
	challengeRole := &agent.ChallengeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	opinions := []DecisionOpinion{
		mustRunJudge(t, runners, "judge-1", judgeRole),
		mustRunJudge(t, runners, "judge-2", judgeRole),
	}
	challenges := []DecisionChallenge{
		mustRunChallenge(t, runners, "challenger-1", challengeRole),
		mustRunChallenge(t, runners, "challenger-2", challengeRole),
	}
	revisions := make([]DecisionRevision, 0, len(opinions))
	for _, opinion := range opinions {
		revision, err := runners.RunRevision(context.Background(), RevisionRequest{
			DecisionID: "dec-1", JudgeID: opinion.JudgeID, Original: opinion,
			Prompt:      "You are judge " + opinion.JudgeID + " revising your own judgment once (round 2).",
			RoutingRole: judgeRole,
		})
		if err != nil {
			t.Fatalf("RunRevision(%s): %v", opinion.JudgeID, err)
		}
		if revision.AgentID != opinion.AgentID || revision.Model != opinion.Model || revision.Provider != opinion.Provider {
			t.Fatalf("revision route = %#v, want original route agent=%q model=%q provider=%q", revision, opinion.AgentID, opinion.Model, opinion.Provider)
		}
		revisions = append(revisions, revision)
	}
	if len(challenges) != 2 {
		t.Fatalf("challenges = %d, want 2", len(challenges))
	}

	events, err := h.coordinator.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	profiles := 0
	for _, event := range events {
		if event.Type != string(EventModelProfileResolved) {
			continue
		}
		var projection struct {
			ModelID  string `json:"model_id"`
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal(event.Payload, &projection); err != nil {
			t.Fatalf("decode model profile event: %v", err)
		}
		if projection.Provider == "" || projection.ModelID == "" {
			t.Fatalf("incomplete model profile telemetry: %#v", projection)
		}
		profiles++
	}
	if profiles != 6 {
		t.Fatalf("model profile events = %d, want 6 for JUDGE→CHALLENGE→REVISE", profiles)
	}
}

func TestDecisionEngineRoutingDurableIdentityAndProfileOrder(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	judgeResponse := `{"option_scores":[{"option_id":"migrate","criteria":{"cost":8,"risk":8}},{"option_id":"wait","criteria":{"cost":5,"risk":5}}],"preferred_option":"migrate","success_probability":0.7,"confidence":0.8}`
	challengeResponse := `{"target_option":"migrate","strongest_countercase":"capacity risk","fragile_assumptions":["capacity holds"],"missing_evidence":[],"falsification_tests":["run capacity test"],"severity":0.4}`
	revisionResponse := `{"revised_scores":[{"option_id":"migrate","criteria":{"cost":8,"risk":8}},{"option_id":"wait","criteria":{"cost":5,"risk":5}}],"revised_probability":0.7,"changed":false,"reason":"countercase does not change the judgment"}`
	for _, provider := range []*fixedAnswerProvider{h.candA, h.candB, h.candC} {
		provider.responses = []string{judgeResponse, challengeResponse, revisionResponse}
	}

	var mu sync.Mutex
	profileCount, providerRequests := 0, 0
	var orderErr error
	h.coordinator.reportStatus = func(event StatusEvent) {
		if event.Type != string(EventModelProfileResolved) {
			return
		}
		mu.Lock()
		profileCount++
		mu.Unlock()
	}
	for _, provider := range []*fixedAnswerProvider{h.candA, h.candB, h.candC} {
		provider.onChat = func() {
			mu.Lock()
			defer mu.Unlock()
			providerRequests++
			if profileCount < providerRequests && orderErr == nil {
				orderErr = fmt.Errorf("provider request %d preceded model profile event %d", providerRequests, profileCount)
			}
		}
	}

	artifactStore, err := NewFileArtifactStore(h.coordinator.session.Workspace, h.coordinator.session.Workspace)
	if err != nil {
		t.Fatalf("NewFileArtifactStore: %v", err)
	}
	runners := newDecisionRunners(h.coordinator, "task-1")
	policy := enginePolicy(2)
	policy.MaxRounds = 2
	policy.Challenge = ChallengePolicy{Enabled: true, Count: 2}
	policy.Revision = RevisionPolicy{Enabled: true}
	policy.JudgeRole = &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}
	policy.ChallengeRole = &agent.ChallengeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}
	req := engineRequest(policy)
	req.DecisionID = "decision-routing-integration"
	req.RunID = h.coordinator.executionRunID
	engine := NewDecisionEngine(DecisionServices{
		Judges: runners, Challengers: runners, Revisions: runners,
		Journal: eventStoreJournal{store: h.coordinator.eventStore}, Store: artifactStore,
	})
	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("decision engine Run: %v", err)
	}
	if len(record.Opinions) != 2 || len(record.Challenges) != 2 || len(record.Revisions) != 2 {
		t.Fatalf("decision stages = opinions:%d challenges:%d revisions:%d, want 2/2/2", len(record.Opinions), len(record.Challenges), len(record.Revisions))
	}
	mu.Lock()
	gotProfiles, gotRequests, gotOrderErr := profileCount, providerRequests, orderErr
	mu.Unlock()
	if gotOrderErr != nil {
		t.Fatal(gotOrderErr)
	}
	if gotProfiles != gotRequests || gotRequests != 6 {
		t.Fatalf("profile/request counts = %d/%d, want 6/6", gotProfiles, gotRequests)
	}

	events, err := h.coordinator.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	profileProviders := make(map[string]string)
	for _, event := range events {
		if event.Type != string(EventModelProfileResolved) {
			continue
		}
		var profile struct {
			ModelID  string `json:"model_id"`
			Provider string `json:"provider"`
		}
		if err := json.Unmarshal(event.Payload, &profile); err != nil {
			t.Fatalf("decode profile event: %v", err)
		}
		profileProviders[profile.ModelID] = profile.Provider
	}
	for _, event := range events {
		if event.Type != string(agent.EventDecisionOpinionSubmitted) &&
			event.Type != string(agent.EventDecisionChallengeSubmitted) &&
			event.Type != string(agent.EventDecisionRevisionSubmitted) {
			continue
		}
		var durable decisionEvent
		if err := json.Unmarshal(event.Payload, &durable); err != nil {
			t.Fatalf("decode durable decision event: %v", err)
		}
		var agentID, modelID, provider string
		switch event.Type {
		case string(agent.EventDecisionOpinionSubmitted):
			agentID, modelID, provider = durable.Opinion.AgentID, durable.Opinion.Model, durable.Opinion.Provider
		case string(agent.EventDecisionChallengeSubmitted):
			agentID, modelID, provider = durable.Challenge.AgentID, durable.Challenge.Model, durable.Challenge.Provider
		case string(agent.EventDecisionRevisionSubmitted):
			agentID, modelID, provider = durable.Revision.AgentID, durable.Revision.Model, durable.Revision.Provider
		}
		if agentID == "" || modelID == "" || provider == "" || profileProviders[modelID] != provider {
			t.Fatalf("durable decision identity = agent:%q model:%q provider:%q, profiles=%v", agentID, modelID, provider, profileProviders)
		}
		if !strings.Contains(modelID, "/") {
			t.Fatalf("durable decision model %q lost its named provider prefix", modelID)
		}
	}
}

func mustRunJudge(t *testing.T, runners *coordinatorDecisionRunners, judgeID string, role *agent.JudgeRolePolicy) DecisionOpinion {
	t.Helper()
	opinion, err := runners.RunJudge(context.Background(), judgeRequestFor(judgeID, role))
	if err != nil {
		t.Fatalf("RunJudge(%s): %v", judgeID, err)
	}
	return opinion
}

func mustRunChallenge(t *testing.T, runners *coordinatorDecisionRunners, challengerID string, role *agent.ChallengeRolePolicy) DecisionChallenge {
	t.Helper()
	challenge, err := runners.RunChallenge(context.Background(), challengeRequestFor(challengerID, role))
	if err != nil {
		t.Fatalf("RunChallenge(%s): %v", challengerID, err)
	}
	return challenge
}

// min-distinct-models must fail closed when the qualified pool cannot reach
// the configured floor.
func TestJudgeRoleCapabilityRouting_FailsClosedWhenPoolLacksModelDiversity(t *testing.T) {
	h := modelSharingJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	// Only cand-x and cand-y qualify, both on the same model.
	role := &agent.JudgeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		MinDistinctModels:    2,
	}
	h.coordinator.session.Config.Delegation.AllowedWorkers = []string{"judge-cand-x", "judge-cand-y"}
	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", role)); err == nil {
		t.Fatal("want an error when the qualified pool cannot reach min-distinct-models")
	}
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

	// Ranked by declared confidence: c (0.7) > b (0.6) > a (0.5), so
	// judge-1 -> cand-c, judge-2 -> cand-b, judge-3 -> cand-a.
	wantAgent := map[string]string{"judge-1": "judge-cand-c", "judge-2": "judge-cand-b", "judge-3": "judge-cand-a"}
	for judgeID := range 3 {
		id := fmtJudgeID(judgeID + 1)
		opinion, err := runners.RunJudge(context.Background(), judgeRequestFor(id, role))
		if err != nil {
			t.Fatalf("RunJudge(%s): %v", id, err)
		}
		if opinion.AgentID != wantAgent[id] {
			t.Fatalf("%s opinion.AgentID = %q, want %q (durable AgentBinding must record the resolved worker)", id, opinion.AgentID, wantAgent[id])
		}
	}

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

	opinion, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", nil))
	if err != nil {
		t.Fatalf("RunJudge: %v", err)
	}
	if opinion.AgentID != "" {
		t.Fatalf("opinion.AgentID = %q, want empty on the legacy sidecar path", opinion.AgentID)
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

// Integration proof that a matching routing hint changes which candidate
// judge-1 binds to, and that a routed revision for the same JudgeID still
// lands on that same hint-influenced candidate — hints don't break "reuse
// the original binding" (spec2.md §8), because the engine applies the same
// deterministic augmentation independently at both the JUDGE and REVISE
// construction sites (decision_engine.go / decision_engine_stages.go).
func TestJudgeRoleCapabilityRouting_HintsChangeBindingAndRevisionReusesIt(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	baseRole := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}
	hints := []agent.RoutingHint{
		{WhenGoalContains: "storage", PreferredCapabilities: []string{"architecture"}},
	}
	const question = "Should we change the storage backend?"

	// Without the hint applying (a non-matching question), judge-1 ranks to
	// cand-c exactly as the non-hinted tests already prove.
	unhintedRole := hintedJudgeRole(baseRole, hints, "an unrelated question")
	if _, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", unhintedRole)); err != nil {
		t.Fatalf("RunJudge(judge-1, unhinted): %v", err)
	}
	if got := h.candC.count(); got < 1 {
		t.Fatalf("unhinted judge-1 should still resolve to cand-c, got cand-c calls = %d", got)
	}
	if got := h.candA.count(); got != 0 {
		t.Fatalf("unhinted judge-1 must not reach cand-a, got %d calls", got)
	}

	// With the hint matching, cand-a's declared "architecture" preference
	// (0.9 confidence) outscores cand-c's plain required match (0.7), so
	// judge-1 now binds to cand-a instead.
	beforeB, beforeC := h.candB.count(), h.candC.count()
	hintedRole := hintedJudgeRole(baseRole, hints, question)
	hintedOpinion, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-1", hintedRole))
	if err != nil {
		t.Fatalf("RunJudge(judge-1, hinted): %v", err)
	}
	if got := h.candA.count(); got < 1 {
		t.Fatalf("hinted judge-1 should resolve to cand-a, got cand-a calls = %d", got)
	}
	if hintedOpinion.AgentID != "judge-cand-a" {
		t.Fatalf("hinted opinion.AgentID = %q, want %q", hintedOpinion.AgentID, "judge-cand-a")
	}
	beforeA := h.candA.count()

	// REVISE for judge-1, applying the identical hint independently, must
	// land on the same candidate (cand-a) the hinted JUDGE round did.
	revisionReq := RevisionRequest{
		DecisionID:  "dec-1",
		JudgeID:     "judge-1",
		Original:    DecisionOpinion{JudgeID: "judge-1", PreferredOption: "execute"},
		Prompt:      "You are judge judge-1 revising your own judgment once (round 2).",
		RoutingRole: hintedJudgeRole(baseRole, hints, question),
	}
	revision, err := runners.RunRevision(context.Background(), revisionReq)
	if err != nil {
		t.Fatalf("RunRevision(judge-1, hinted): %v", err)
	}
	if revision.AgentID != hintedOpinion.AgentID {
		t.Fatalf("revision.AgentID = %q, want it to reuse the JUDGE round's binding %q", revision.AgentID, hintedOpinion.AgentID)
	}
	if got := h.candA.count(); got <= beforeA {
		t.Fatalf("hinted revision did not reach cand-a: before=%d after=%d", beforeA, got)
	}
	if got := h.candB.count(); got != beforeB {
		t.Fatalf("hinted judge/revision must not reach cand-b: before=%d after=%d", beforeB, got)
	}
	if got := h.candC.count(); got != beforeC {
		t.Fatalf("hinted judge/revision must not reach cand-c: before=%d after=%d", beforeC, got)
	}
}
