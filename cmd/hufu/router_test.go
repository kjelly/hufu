package main

import (
	"context"
	"testing"
)

func TestRoute_DeterministicFastSignals(t *testing.T) {
	router := NewExecutionRouter(nil, nil)
	ctx := context.Background()

	cases := []struct {
		prompt string
		want   ExecutionRoute
	}{
		{"explain how the coordinator works", RouteFast},
		{"what is the default model context window?", RouteFast},
		{"fix typo in main.go", RouteFast},
		{"run test TestRoute", RouteFast},
		{"list available teams", RouteFast},
	}

	for _, tc := range cases {
		dec := router.Route(ctx, tc.prompt, "")
		if dec.Route != tc.want {
			t.Errorf("Route(%q) = %s, want %s (reasons: %v)", tc.prompt, dec.Route, tc.want, dec.Reasons)
		}
		if len(dec.Reasons) == 0 {
			t.Errorf("Route(%q) should have explainable reasons", tc.prompt)
		}
		if dec.Confidence <= 0 {
			t.Errorf("Route(%q) confidence = %f, want > 0", tc.prompt, dec.Confidence)
		}
	}
}

func TestRoute_DeterministicTeamSignals(t *testing.T) {
	router := NewExecutionRouter(nil, nil)
	ctx := context.Background()

	cases := []struct {
		prompt string
		want   ExecutionRoute
	}{
		{"@researcher research the bug and @coder fix it", RouteTeam},
		{"research and implement a new memory system with debate and skeptic review", RouteTeam},
		{"deploy kubernetes cluster to production with ci/cd pipeline", RouteTeam},
		{"refactor entire codebase across packages for new architecture", RouteTeam},
		{"run full test suite and verify with acceptance criteria", RouteTeam},
	}

	for _, tc := range cases {
		dec := router.Route(ctx, tc.prompt, "")
		if dec.Route != tc.want {
			t.Errorf("Route(%q) = %s, want %s (reasons: %v)", tc.prompt, dec.Route, tc.want, dec.Reasons)
		}
		if len(dec.Reasons) == 0 {
			t.Errorf("Route(%q) should have explainable reasons", tc.prompt)
		}
	}
}

func TestRoute_ExplicitFlagOverrides(t *testing.T) {
	router := NewExecutionRouter(nil, nil)
	ctx := context.Background()

	origRoute := opts.routeMode
	defer func() { opts.routeMode = origRoute }()

	opts.routeMode = "fast"
	decFast := router.Route(ctx, "research and implement a complex multi-agent framework across packages", "")
	if decFast.Route != RouteFast {
		t.Errorf("expected RouteFast with --route fast override, got %s", decFast.Route)
	}

	opts.routeMode = "team"
	decTeam := router.Route(ctx, "what is hufu?", "")
	if decTeam.Route != RouteTeam {
		t.Errorf("expected RouteTeam with --route team override, got %s", decTeam.Route)
	}
}

func TestRoute_DefaultTeamHandling(t *testing.T) {
	router := NewExecutionRouter(nil, nil)
	ctx := context.Background()

	origDefault := opts.defaultTeam
	defer func() { opts.defaultTeam = origDefault }()

	opts.defaultTeam = true
	dec := router.Route(ctx, "fix typo in doc", "default")
	if dec.Route != RouteFast {
		t.Errorf("expected RouteFast for --default with simple prompt, got %s", dec.Route)
	}
	if dec.Team != "default" {
		t.Errorf("expected team 'default', got %q", dec.Team)
	}
}

func TestRoute_CanEscalateToTeam(t *testing.T) {
	router := NewExecutionRouter(nil, nil)
	fastDec := RouteDecision{Route: RouteFast, Team: "default"}
	teamDec := RouteDecision{Route: RouteTeam, Team: "dev-team"}

	// Fast route escalation scenarios
	esc, reason := router.CanEscalateToTeam(fastDec, 2, 0, true)
	if !esc || reason == "" {
		t.Errorf("expected escalation when multi-agent is required")
	}

	esc, reason = router.CanEscalateToTeam(fastDec, 3, 2, false)
	if !esc || reason == "" {
		t.Errorf("expected escalation on repeated errors")
	}

	esc, reason = router.CanEscalateToTeam(fastDec, 10, 0, false)
	if !esc || reason == "" {
		t.Errorf("expected escalation when step budget exceeded")
	}

	esc, _ = router.CanEscalateToTeam(fastDec, 2, 0, false)
	if esc {
		t.Errorf("expected no escalation for simple fast execution")
	}

	// Team route never escalates
	esc, _ = router.CanEscalateToTeam(teamDec, 10, 5, true)
	if esc {
		t.Errorf("already on RouteTeam, should not escalate")
	}
}
