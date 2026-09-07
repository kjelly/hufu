package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func agentsFixture() map[string]*agent.AgentDef {
	return map[string]*agent.AgentDef{
		"a": {Name: "a", Generation: agent.GenerationParams{Model: "model-x"}, ProviderURL: "https://p1"},
		"b": {Name: "b", Generation: agent.GenerationParams{Model: "model-y"}, ProviderURL: "https://p2"},
		"c": {Name: "c", Generation: agent.GenerationParams{Model: "model-y"}, ProviderURL: "https://p2"},
	}
}

func TestResolveRoleCandidateOrder(t *testing.T) {
	agents := agentsFixture()

	t.Run("no preference flags returns the pool unchanged", func(t *testing.T) {
		pool := []string{"c", "b", "a"}
		got := resolveRoleCandidateOrder(pool, agents, false, false)
		if len(got) != 3 || got[0] != "c" || got[1] != "b" || got[2] != "a" {
			t.Fatalf("got = %#v, want unchanged %#v", got, pool)
		}
	})

	t.Run("prefer distinct models moves a fresh model ahead of a repeat", func(t *testing.T) {
		// a, b share nothing; b and c share model-y. Ranked [a, b, c]: a is
		// fresh (model-x), b is fresh (model-y), c is a repeat (model-y).
		got := resolveRoleCandidateOrder([]string{"a", "b", "c"}, agents, true, false)
		if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
			t.Fatalf("got = %#v, want [a b c] (already diverse in rank order)", got)
		}

		// Ranked [b, c, a]: b fresh (model-y), c repeat (model-y), a fresh
		// (model-x) -> a should move ahead of c.
		got = resolveRoleCandidateOrder([]string{"b", "c", "a"}, agents, true, false)
		if len(got) != 3 || got[0] != "b" || got[1] != "a" || got[2] != "c" {
			t.Fatalf("got = %#v, want [b a c]", got)
		}
	})

	t.Run("prefer distinct providers behaves the same way over ProviderURL", func(t *testing.T) {
		got := resolveRoleCandidateOrder([]string{"b", "c", "a"}, agents, false, true)
		if len(got) != 3 || got[0] != "b" || got[1] != "a" || got[2] != "c" {
			t.Fatalf("got = %#v, want [b a c]", got)
		}
	})

	t.Run("unknown agent ID is treated as sharing the empty model/provider key", func(t *testing.T) {
		got := resolveRoleCandidateOrder([]string{"missing-1", "missing-2"}, agents, true, false)
		if len(got) != 2 || got[0] != "missing-1" || got[1] != "missing-2" {
			t.Fatalf("got = %#v, want [missing-1 missing-2] (first is fresh, second repeats the empty key)", got)
		}
	})
}

func TestDistinctModelAndProviderCount(t *testing.T) {
	agents := agentsFixture()
	if got := distinctModelCount([]string{"a", "b", "c"}, agents); got != 2 {
		t.Fatalf("distinctModelCount = %d, want 2", got)
	}
	if got := distinctProviderCount([]string{"a", "b", "c"}, agents); got != 2 {
		t.Fatalf("distinctProviderCount = %d, want 2", got)
	}
	if got := distinctModelCount(nil, agents); got != 0 {
		t.Fatalf("distinctModelCount(nil) = %d, want 0", got)
	}
}

func TestComputeBindingDiversitySummary(t *testing.T) {
	t.Run("all distinct", func(t *testing.T) {
		got := computeBindingDiversitySummary([]bindingIdentity{
			{AgentID: "a", Model: "m1", Provider: "p1"},
			{AgentID: "b", Model: "m2", Provider: "p2"},
		})
		if got.Count != 2 || got.DistinctAgentCount != 2 || got.DistinctModelCount != 2 || got.DistinctProviderCount != 2 || len(got.RepeatedAgentBindings) != 0 {
			t.Fatalf("got = %#v", got)
		}
	})

	t.Run("some repeated", func(t *testing.T) {
		got := computeBindingDiversitySummary([]bindingIdentity{
			{AgentID: "a", Model: "m1", Provider: "p1"},
			{AgentID: "a", Model: "m1", Provider: "p1"},
			{AgentID: "b", Model: "m1", Provider: "p1"},
		})
		if got.Count != 3 || got.DistinctAgentCount != 2 || got.DistinctModelCount != 1 || got.DistinctProviderCount != 1 {
			t.Fatalf("got = %#v", got)
		}
		if len(got.RepeatedAgentBindings) != 1 || got.RepeatedAgentBindings[0] != "a" {
			t.Fatalf("RepeatedAgentBindings = %#v, want [a]", got.RepeatedAgentBindings)
		}
	})

	t.Run("empty-AgentID entries are excluded, not counted as unrouted", func(t *testing.T) {
		got := computeBindingDiversitySummary([]bindingIdentity{{}, {}})
		if got.Count != 0 || got.DistinctAgentCount != 0 || got.DistinctModelCount != 0 || got.DistinctProviderCount != 0 || len(got.RepeatedAgentBindings) != 0 {
			t.Fatalf("got = %#v, want a zero-value summary", got)
		}
	})
}

func TestJudgeAndChallengeDiversitySummary(t *testing.T) {
	t.Run("legacy sidecar opinions (no AgentID) produce a nil summary", func(t *testing.T) {
		opinions := []DecisionOpinion{{JudgeID: "judge-1"}, {JudgeID: "judge-2"}}
		if got := judgeDiversitySummary(opinions); got != nil {
			t.Fatalf("judgeDiversitySummary = %#v, want nil", got)
		}
	})

	t.Run("routed opinions produce a populated summary", func(t *testing.T) {
		opinions := []DecisionOpinion{
			{JudgeID: "judge-1", AgentID: "cand-a", Model: "m1", Provider: "p1"},
			{JudgeID: "judge-2", AgentID: "cand-b", Model: "m2", Provider: "p2"},
		}
		got := judgeDiversitySummary(opinions)
		if got == nil || got.Count != 2 || got.DistinctAgentCount != 2 {
			t.Fatalf("judgeDiversitySummary = %#v", got)
		}
	})

	t.Run("challengeDiversitySummary mirrors judgeDiversitySummary", func(t *testing.T) {
		if got := challengeDiversitySummary(nil); got != nil {
			t.Fatalf("challengeDiversitySummary(nil) = %#v, want nil", got)
		}
		challenges := []DecisionChallenge{{AgentID: "cand-a", Model: "m1", Provider: "p1"}}
		got := challengeDiversitySummary(challenges)
		if got == nil || got.Count != 1 {
			t.Fatalf("challengeDiversitySummary = %#v", got)
		}
	})
}
