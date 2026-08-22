package main

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/sidecar"
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

func TestRoute_ExplicitNonDefaultTeamUsesCoordinator(t *testing.T) {
	router := NewExecutionRouter(nil, nil)
	ctx := context.Background()

	origRoute := opts.routeMode
	defer func() { opts.routeMode = origRoute }()
	opts.routeMode = "auto"

	dec := router.Route(ctx, "review recent commits", "hufu-code-review")
	if dec.Route != RouteTeam {
		t.Fatalf("explicit non-default team route = %s, want %s (reasons: %v)", dec.Route, RouteTeam, dec.Reasons)
	}
	if dec.Team != "hufu-code-review" {
		t.Fatalf("explicit non-default team = %q, want hufu-code-review", dec.Team)
	}
}

func TestRoute_SelectionPreflightContextAndCloseEncloseClassifier(t *testing.T) {
	type contextKey string
	const key contextKey = "preflight"
	wantContext := context.WithValue(context.Background(), key, "builder-context")
	var calls []string

	router := NewExecutionRouter(nil, nil)
	router.sidecarBuilder = func(context.Context) *preflightSidecarHandle {
		return &preflightSidecarHandle{
			sidecar: &sidecar.Sidecar{},
			ctx:     wantContext,
			close:   func() { calls = append(calls, "close") },
		}
	}
	originalClassifier := classifyRouteWithSelectionSidecar
	classifyRouteWithSelectionSidecar = func(ctx context.Context, _ *sidecar.Sidecar, _ string) (sidecar.RouteClassification, error) {
		if ctx.Value(key) != "builder-context" {
			t.Fatalf("classifier context value = %v, want builder context", ctx.Value(key))
		}
		calls = append(calls, "classify")
		return sidecar.RouteClassification{Route: "fast", Reason: "test"}, nil
	}
	t.Cleanup(func() { classifyRouteWithSelectionSidecar = originalClassifier })

	decision := router.Route(context.Background(), "consider the implications of this request carefully before choosing the appropriate execution path for the work", "")
	if decision.Route != RouteFast {
		t.Fatalf("route = %s, want %s", decision.Route, RouteFast)
	}
	if got, want := len(calls), 2; got != want || calls[0] != "classify" || calls[1] != "close" {
		t.Fatalf("classifier/close order = %v, want [classify close]", calls)
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
