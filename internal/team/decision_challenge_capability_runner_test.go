package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func challengeRequestFor(challengerID string, role *agent.ChallengeRolePolicy) ChallengeRequest {
	// "You are challenging a candidate decision" is the marker
	// classifyDecisionStagePrompt looks for so the legacy-sidecar fake
	// correctly attributes a call to stageChallenge.
	return ChallengeRequest{
		DecisionID:   "dec-1",
		ChallengerID: challengerID,
		Prompt:       "You are challenging a candidate decision for " + challengerID + ".",
		RoutingRole:  role,
	}
}

// spec2.md's own acceptance shape for CHALLENGE: each challenger's provider
// call must be attributed to its own distinct resolved candidate, and the
// legacy judge-model sidecar must record zero calls.
func TestChallengeRoleCapabilityRouting_RoutesEachChallengerToADistinctAgent(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.ChallengeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	challenge1, err := runners.RunChallenge(context.Background(), challengeRequestFor("challenger-1", role))
	if err != nil {
		t.Fatalf("RunChallenge(challenger-1): %v", err)
	}
	challenge2, err := runners.RunChallenge(context.Background(), challengeRequestFor("challenger-2", role))
	if err != nil {
		t.Fatalf("RunChallenge(challenger-2): %v", err)
	}

	// Ranked by declared confidence: c (0.7) > b (0.6), so challenger-1 ->
	// cand-c, challenger-2 -> cand-b.
	if challenge1.AgentID != "judge-cand-c" {
		t.Fatalf("challenger-1 challenge.AgentID = %q, want %q", challenge1.AgentID, "judge-cand-c")
	}
	if challenge2.AgentID != "judge-cand-b" {
		t.Fatalf("challenger-2 challenge.AgentID = %q, want %q", challenge2.AgentID, "judge-cand-b")
	}
	if got := h.candC.count(); got < 1 {
		t.Fatalf("challenger-1's resolved candidate (cand-c) call count = %d, want at least 1", got)
	}
	if got := h.candB.count(); got < 1 {
		t.Fatalf("challenger-2's resolved candidate (cand-b) call count = %d, want at least 1", got)
	}
	if got := h.legacyJudge.count(stageChallenge); got != 0 {
		t.Fatalf("legacy judge-model CHALLENGE-stage calls = %d, want 0 (this is spec2.md's acceptance bar)", got)
	}
}

// Legacy behavior (RoutingRole nil) must be completely unaffected.
func TestChallengeRoleCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")

	challenge, err := runners.RunChallenge(context.Background(), challengeRequestFor("challenger-1", nil))
	if err != nil {
		t.Fatalf("RunChallenge: %v", err)
	}
	if challenge.AgentID != "" {
		t.Fatalf("challenge.AgentID = %q, want empty on the legacy sidecar path", challenge.AgentID)
	}
	if got := h.legacyJudge.count(stageChallenge); got != 1 {
		t.Fatalf("legacy judge-model CHALLENGE-stage calls = %d, want 1 (unchanged legacy path)", got)
	}
	if got := h.candA.count() + h.candB.count() + h.candC.count(); got != 0 {
		t.Fatalf("capability-routed candidates were called (%d) when RoutingRole was nil", got)
	}
}

// A pool smaller than challenge-role.min-distinct-agents must fail closed.
func TestChallengeRoleCapabilityRouting_FailsClosedWhenPoolTooSmall(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.ChallengeRolePolicy{
		RequiredCapabilities: []string{"a-capability-nobody-declares"},
		MinDistinctAgents:    1,
	}
	_, err := runners.RunChallenge(context.Background(), challengeRequestFor("challenger-1", role))
	if err == nil {
		t.Fatal("want an error when the qualified pool is empty, got nil")
	}
	if got := h.legacyJudge.count(stageChallenge); got != 0 {
		t.Fatalf("legacy judge-model CHALLENGE-stage calls = %d, want 0 (must fail closed, not silently fall back)", got)
	}
}

func TestChallengerOrdinal(t *testing.T) {
	cases := []struct {
		id      string
		want    int
		wantErr bool
	}{
		{id: "challenger-1", want: 1},
		{id: "challenger-2", want: 2},
		{id: "challenger-0", wantErr: true},
		{id: "not-a-challenger-id", wantErr: true},
		{id: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got, err := challengerOrdinal(tc.id)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("challengerOrdinal(%q) = %d, want error", tc.id, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("challengerOrdinal(%q) error = %v", tc.id, err)
			}
			if got != tc.want {
				t.Fatalf("challengerOrdinal(%q) = %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}

// This is the concrete proof of spec2.md §8: a revision dispatch for the
// same JudgeID as an earlier judge dispatch must land on the exact same
// resolved candidate, not a freshly re-resolved one.
func TestRevisionCapabilityRouting_ReusesOriginalJudgeBinding(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	// judge-2 ranks to cand-b (c=0.7 is judge-1, b=0.6 is judge-2).
	opinion, err := runners.RunJudge(context.Background(), judgeRequestFor("judge-2", role))
	if err != nil {
		t.Fatalf("RunJudge(judge-2): %v", err)
	}
	if opinion.AgentID != "judge-cand-b" {
		t.Fatalf("judge-2 opinion.AgentID = %q, want %q", opinion.AgentID, "judge-cand-b")
	}
	if got := h.candB.count(); got < 1 {
		t.Fatalf("judge-2's original binding (cand-b) call count = %d, want at least 1", got)
	}

	revisionReq := RevisionRequest{
		DecisionID:  "dec-1",
		JudgeID:     "judge-2",
		Original:    opinion,
		Prompt:      "You are judge judge-2 revising your own judgment once (round 2).",
		RoutingRole: role,
	}
	beforeB := h.candB.count()
	revision, err := runners.RunRevision(context.Background(), revisionReq)
	if err != nil {
		t.Fatalf("RunRevision(judge-2): %v", err)
	}
	if revision.AgentID != opinion.AgentID {
		t.Fatalf("revision.AgentID = %q, want it to durably record the same binding as the original opinion %q", revision.AgentID, opinion.AgentID)
	}

	if got := h.candB.count(); got <= beforeB {
		t.Fatalf("revision did not reach judge-2's original binding (cand-b): before=%d after=%d", beforeB, got)
	}
	if got := h.candA.count(); got != 0 {
		t.Fatalf("revision reached cand-a (%d calls), which was never judge-2's binding", got)
	}
	if got := h.candC.count(); got != 0 {
		t.Fatalf("revision reached cand-c (%d calls), which was never judge-2's binding", got)
	}
	if got := h.legacyJudge.count(stageRevision); got != 0 {
		t.Fatalf("legacy judge-model REVISION-stage calls = %d, want 0", got)
	}
}

// The original opinion (with its durable AgentID set) is now the mechanism
// REVISE reuses, not a re-ranked recompute — so a fallback opinion that
// *would* re-rank to a different candidate must still land on the original.
func TestRevisionCapabilityRouting_UsesOriginalAgentIDNotFreshRanking(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	// If REVISE re-ranked from scratch for judge-2 (ordinal 2), it would
	// land on cand-b (rank 2). Force the original opinion's AgentID to
	// cand-a instead (rank 3) to prove the durable value, not a fresh
	// recompute, drives the outcome.
	original := DecisionOpinion{JudgeID: "judge-2", PreferredOption: "execute", AgentID: "judge-cand-a"}
	revisionReq := RevisionRequest{
		DecisionID: "dec-1", JudgeID: "judge-2", Original: original,
		Prompt: "You are judge judge-2 revising your own judgment once (round 2).", RoutingRole: role,
	}
	revision, err := runners.RunRevision(context.Background(), revisionReq)
	if err != nil {
		t.Fatalf("RunRevision: %v", err)
	}
	if revision.AgentID != "judge-cand-a" {
		t.Fatalf("revision.AgentID = %q, want %q (the original opinion's durable binding, not a re-ranked candidate)", revision.AgentID, "judge-cand-a")
	}
	if got := h.candA.count(); got < 1 {
		t.Fatalf("cand-a call count = %d, want at least 1", got)
	}
	if got := h.candB.count(); got != 0 {
		t.Fatalf("cand-b call count = %d, want 0 (a fresh re-rank would have reached it, the durable original must not)", got)
	}
}

// A capability change between JUDGE round 1 and REVISE (the original
// binding no longer satisfies the required capability) must fail closed,
// not silently fall through to a different candidate.
func TestRevisionCapabilityRouting_FailsClosedWhenOriginalBindingCapabilityInvalidated(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"a-capability-cand-a-does-not-have"}}

	original := DecisionOpinion{JudgeID: "judge-1", PreferredOption: "execute", AgentID: "judge-cand-a"}
	revisionReq := RevisionRequest{
		DecisionID: "dec-1", JudgeID: "judge-1", Original: original,
		Prompt: "You are judge judge-1 revising your own judgment once (round 2).", RoutingRole: role,
	}
	if _, err := runners.RunRevision(context.Background(), revisionReq); err == nil {
		t.Fatal("want an error when the original binding no longer satisfies the required capability")
	}
	if got := h.candA.count() + h.candB.count() + h.candC.count() + h.legacyJudge.count(stageRevision); got != 0 {
		t.Fatalf("no candidate should have been invoked after failing closed, got %d calls", got)
	}
}

// A binding that is no longer authorized (removed from allowed-workers)
// must also fail closed.
func TestRevisionCapabilityRouting_FailsClosedWhenOriginalBindingNoLongerAuthorized(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	h.coordinator.session.Config.Delegation.AllowedWorkers = []string{"judge-cand-b", "judge-cand-c"}
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	original := DecisionOpinion{JudgeID: "judge-1", PreferredOption: "execute", AgentID: "judge-cand-a"}
	revisionReq := RevisionRequest{
		DecisionID: "dec-1", JudgeID: "judge-1", Original: original,
		Prompt: "You are judge judge-1 revising your own judgment once (round 2).", RoutingRole: role,
	}
	if _, err := runners.RunRevision(context.Background(), revisionReq); err == nil {
		t.Fatal("want an error when the original binding is no longer in delegation.allowed-workers")
	}
	if got := h.candA.count(); got != 0 {
		t.Fatalf("unauthorized cand-a should never be invoked, got %d calls", got)
	}
}

// When the original opinion carries no AgentID (e.g. it came from the
// legacy sidecar), REVISE falls back to a fresh resolution rather than
// erroring.
func TestRevisionCapabilityRouting_FallsBackToResolutionWhenOriginalAgentIDEmpty(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	original := DecisionOpinion{JudgeID: "judge-1", PreferredOption: "execute"}
	revisionReq := RevisionRequest{
		DecisionID: "dec-1", JudgeID: "judge-1", Original: original,
		Prompt: "You are judge judge-1 revising your own judgment once (round 2).", RoutingRole: role,
	}
	revision, err := runners.RunRevision(context.Background(), revisionReq)
	if err != nil {
		t.Fatalf("RunRevision: %v", err)
	}
	// judge-1 re-ranks to cand-c (top rank), the same as a fresh JUDGE call.
	if revision.AgentID != "judge-cand-c" {
		t.Fatalf("revision.AgentID = %q, want %q (fresh resolution fallback)", revision.AgentID, "judge-cand-c")
	}
}

// Legacy revision behavior (RoutingRole nil) must be completely unaffected.
func TestRevisionCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")

	revisionReq := RevisionRequest{
		DecisionID: "dec-1",
		JudgeID:    "judge-1",
		Original:   DecisionOpinion{JudgeID: "judge-1", PreferredOption: "execute"},
		Prompt:     "You are judge judge-1 revising your own judgment once (round 2).",
	}
	revision, err := runners.RunRevision(context.Background(), revisionReq)
	if err != nil {
		t.Fatalf("RunRevision: %v", err)
	}
	if revision.AgentID != "" {
		t.Fatalf("revision.AgentID = %q, want empty on the legacy sidecar path", revision.AgentID)
	}
	if got := h.legacyJudge.count(stageRevision); got != 1 {
		t.Fatalf("legacy judge-model REVISION-stage calls = %d, want 1 (unchanged legacy path)", got)
	}
	if got := h.candA.count() + h.candB.count() + h.candC.count(); got != 0 {
		t.Fatalf("capability-routed candidates were called (%d) when RoutingRole was nil", got)
	}
}
