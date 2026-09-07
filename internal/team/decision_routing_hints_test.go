package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestApplyRoutingHints(t *testing.T) {
	hints := []agent.RoutingHint{
		{WhenGoalContains: "kubernetes", PreferredCapabilities: []string{"kubernetes", "platform-engineering"}},
		{WhenGoalContains: "security", PreferredCapabilities: []string{"security"}},
	}

	t.Run("no matching hint is a no-op", func(t *testing.T) {
		got := applyRoutingHints(hints, "should we migrate the database", []string{"architecture"})
		if len(got) != 1 || got[0] != "architecture" {
			t.Fatalf("got = %#v, want unchanged [architecture]", got)
		}
	})

	t.Run("one matching hint appends", func(t *testing.T) {
		got := applyRoutingHints(hints, "should we migrate to kubernetes", []string{"architecture"})
		if len(got) != 3 || got[0] != "architecture" || got[1] != "kubernetes" || got[2] != "platform-engineering" {
			t.Fatalf("got = %#v, want [architecture kubernetes platform-engineering]", got)
		}
	})

	t.Run("multiple matching hints all apply", func(t *testing.T) {
		got := applyRoutingHints(hints, "kubernetes security review", nil)
		if len(got) != 3 {
			t.Fatalf("got = %#v, want 3 preferred capabilities from both matching hints", got)
		}
	})

	t.Run("original slice is never mutated", func(t *testing.T) {
		original := []string{"architecture"}
		_ = applyRoutingHints(hints, "kubernetes", original)
		if len(original) != 1 || original[0] != "architecture" {
			t.Fatalf("input slice was mutated: %#v", original)
		}
	})

	t.Run("no hints at all returns preferred unchanged", func(t *testing.T) {
		got := applyRoutingHints(nil, "anything", []string{"architecture"})
		if len(got) != 1 || got[0] != "architecture" {
			t.Fatalf("got = %#v, want unchanged [architecture]", got)
		}
	})
}

func TestHintedJudgeRole(t *testing.T) {
	hints := []agent.RoutingHint{
		{WhenGoalContains: "storage", PreferredCapabilities: []string{"architecture"}},
	}
	role := &agent.JudgeRolePolicy{RequiredCapabilities: []string{"decision-analysis"}}

	augmented := hintedJudgeRole(role, hints, "should we change the storage backend")
	if augmented == role {
		t.Fatal("hintedJudgeRole must return a new value when a hint matches, not the original pointer")
	}
	if len(augmented.PreferredCapabilities) != 1 || augmented.PreferredCapabilities[0] != "architecture" {
		t.Fatalf("augmented.PreferredCapabilities = %#v", augmented.PreferredCapabilities)
	}
	if len(role.PreferredCapabilities) != 0 {
		t.Fatalf("original role was mutated: %#v", role.PreferredCapabilities)
	}

	// A non-matching question still returns a value equivalent to role (no
	// preferred capabilities added) — it need not be the same pointer, since
	// applyRoutingHints always returns a fresh copy once hints is non-empty.
	unchanged := hintedJudgeRole(role, hints, "should we change the network config")
	if len(unchanged.PreferredCapabilities) != 0 {
		t.Fatalf("non-matching question must not add preferred capabilities, got %#v", unchanged.PreferredCapabilities)
	}
	if unchanged.RequiredCapabilities[0] != role.RequiredCapabilities[0] {
		t.Fatalf("RequiredCapabilities must be preserved unchanged, got %#v", unchanged.RequiredCapabilities)
	}

	if hintedJudgeRole(nil, hints, "storage") != nil {
		t.Fatal("hintedJudgeRole(nil, ...) must return nil")
	}
}
