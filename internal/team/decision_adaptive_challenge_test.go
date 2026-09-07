package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestCriterionDispersion(t *testing.T) {
	t.Run("higher spread criterion gets higher dispersion", func(t *testing.T) {
		opinions := []DecisionOpinion{
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 1, "data-integrity": 5}}}},
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 5, "data-integrity": 5}}}},
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 9, "data-integrity": 5}}}},
		}
		got := criterionDispersion(opinions, "execute")
		if got["data-integrity"] != 0 {
			t.Fatalf("data-integrity dispersion = %v, want 0 (identical across opinions)", got["data-integrity"])
		}
		if got["operability"] <= got["data-integrity"] {
			t.Fatalf("operability dispersion (%v) must exceed data-integrity's (%v)", got["operability"], got["data-integrity"])
		}
	})

	t.Run("criterion missing from any opinion is excluded", func(t *testing.T) {
		opinions := []DecisionOpinion{
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 1, "data-integrity": 5}}}},
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 9}}}},
		}
		got := criterionDispersion(opinions, "execute")
		if _, ok := got["data-integrity"]; ok {
			t.Fatalf("data-integrity should be excluded (missing from one opinion), got %#v", got)
		}
		if _, ok := got["operability"]; !ok {
			t.Fatalf("operability should be present (in every opinion), got %#v", got)
		}
	})

	t.Run("invalid opinions are ignored", func(t *testing.T) {
		opinions := []DecisionOpinion{
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 1}}}},
			{Valid: false, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 100}}}},
		}
		got := criterionDispersion(opinions, "execute")
		if got != nil {
			t.Fatalf("with only one valid opinion, dispersion is not meaningful, want nil, got %#v", got)
		}
	})

	t.Run("fewer than two valid opinions returns nil", func(t *testing.T) {
		opinions := []DecisionOpinion{
			{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 1}}}},
		}
		if got := criterionDispersion(opinions, "execute"); got != nil {
			t.Fatalf("got = %#v, want nil", got)
		}
	})
}

func TestMostContestedCriterion(t *testing.T) {
	if got := mostContestedCriterion(nil); got != "" {
		t.Fatalf("mostContestedCriterion(nil) = %q, want empty", got)
	}
	if got := mostContestedCriterion(map[string]float64{}); got != "" {
		t.Fatalf("mostContestedCriterion(empty) = %q, want empty", got)
	}
	if got := mostContestedCriterion(map[string]float64{"a": 0.1, "b": 0.9}); got != "b" {
		t.Fatalf("mostContestedCriterion = %q, want %q", got, "b")
	}
	// Ties broken by criterion ID ascending, deterministically.
	if got := mostContestedCriterion(map[string]float64{"b": 1.0, "a": 1.0}); got != "a" {
		t.Fatalf("mostContestedCriterion tie = %q, want %q", got, "a")
	}
}

func TestAdaptiveChallengePreferred(t *testing.T) {
	mapping := map[string][]string{"operability": {"operations", "reliability"}}

	t.Run("empty criterion ID is a no-op", func(t *testing.T) {
		got := adaptiveChallengePreferred(mapping, "", []string{"architecture"})
		if len(got) != 1 || got[0] != "architecture" {
			t.Fatalf("got = %#v, want unchanged [architecture]", got)
		}
	})

	t.Run("criterion absent from mapping is a no-op", func(t *testing.T) {
		got := adaptiveChallengePreferred(mapping, "data-integrity", []string{"architecture"})
		if len(got) != 1 || got[0] != "architecture" {
			t.Fatalf("got = %#v, want unchanged [architecture]", got)
		}
	})

	t.Run("matching criterion appends its mapped capabilities", func(t *testing.T) {
		got := adaptiveChallengePreferred(mapping, "operability", []string{"architecture"})
		if len(got) != 3 || got[0] != "architecture" || got[1] != "operations" || got[2] != "reliability" {
			t.Fatalf("got = %#v, want [architecture operations reliability]", got)
		}
	})

	t.Run("original slice is never mutated", func(t *testing.T) {
		original := []string{"architecture"}
		_ = adaptiveChallengePreferred(mapping, "operability", original)
		if len(original) != 1 || original[0] != "architecture" {
			t.Fatalf("input slice was mutated: %#v", original)
		}
	})
}

func TestAdaptiveChallengeRole(t *testing.T) {
	role := &agent.ChallengeRolePolicy{
		RequiredCapabilities: []string{"adversarial-analysis"},
		AdaptiveCapabilities: map[string][]string{"operability": {"operations"}},
	}
	opinions := []DecisionOpinion{
		{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 1}}}},
		{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 9}}}},
	}
	aggregate := DecisionAggregate{PreferredOption: "execute"}

	augmented := adaptiveChallengeRole(role, opinions, aggregate)
	if augmented == role {
		t.Fatal("adaptiveChallengeRole must return a new value when it applies, not the original pointer")
	}
	if len(augmented.PreferredCapabilities) != 1 || augmented.PreferredCapabilities[0] != "operations" {
		t.Fatalf("augmented.PreferredCapabilities = %#v", augmented.PreferredCapabilities)
	}
	if len(role.PreferredCapabilities) != 0 {
		t.Fatalf("original role was mutated: %#v", role.PreferredCapabilities)
	}

	t.Run("nil role returns nil", func(t *testing.T) {
		if adaptiveChallengeRole(nil, opinions, aggregate) != nil {
			t.Fatal("adaptiveChallengeRole(nil, ...) must return nil")
		}
	})

	t.Run("no adaptive-capabilities configured is a no-op", func(t *testing.T) {
		plain := &agent.ChallengeRolePolicy{RequiredCapabilities: []string{"adversarial-analysis"}}
		if got := adaptiveChallengeRole(plain, opinions, aggregate); got != plain {
			t.Fatal("with no AdaptiveCapabilities configured, role must be returned unchanged")
		}
	})

	t.Run("no criterion to act on is a no-op", func(t *testing.T) {
		if got := adaptiveChallengeRole(role, nil, aggregate); got != role {
			t.Fatal("with no opinions to compute dispersion from, role must be returned unchanged")
		}
	})
}

// Integration proof: when JUDGE round 1's opinions disagree most on a
// criterion the team maps to a capability the top-ranked default candidate
// doesn't have, CHALLENGE binds to a different, better-matching candidate
// instead — purely from the aggregate's own dispersion, never from which
// agent produced which opinion.
func TestChallengeRoleCapabilityRouting_AdaptiveCapabilitiesChangeBinding(t *testing.T) {
	h := newJudgeRoutingHarness(t)
	runners := newDecisionRunners(h.coordinator, "task-1")

	baseRole := &agent.ChallengeRolePolicy{
		RequiredCapabilities: []string{"decision-analysis"},
		AdaptiveCapabilities: map[string][]string{"operability": {"architecture"}},
	}
	opinions := []DecisionOpinion{
		{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 1}}}},
		{Valid: true, OptionScores: []OptionScore{{OptionID: "execute", Criteria: map[string]float64{"operability": 9}}}},
	}
	aggregate := DecisionAggregate{PreferredOption: "execute"}

	// Without the aggregate's dispersion signal, ranking is c (0.7) > b (0.6)
	// > a (0.5): challenger-1 would bind to cand-c.
	routed := adaptiveChallengeRole(baseRole, opinions, aggregate)
	req := challengeRequestFor("challenger-1", routed)
	challenge, err := runners.RunChallenge(context.Background(), req)
	if err != nil {
		t.Fatalf("RunChallenge: %v", err)
	}
	// cand-a declares architecture:0.9 (decision-config 0.5 + preferred 0.9*0.5
	// = 0.95), which now outranks cand-c's plain 0.7.
	if challenge.AgentID != "judge-cand-a" {
		t.Fatalf("challenge.AgentID = %q, want %q (adaptive capability should have flipped ranking)", challenge.AgentID, "judge-cand-a")
	}
	if got := h.candA.count(); got < 1 {
		t.Fatalf("cand-a call count = %d, want at least 1", got)
	}
	if got := h.candC.count(); got != 0 {
		t.Fatalf("cand-c call count = %d, want 0 (adaptive routing should have moved binding away from the default top rank)", got)
	}
}
