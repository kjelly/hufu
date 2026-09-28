package main

import (
	"context"
	"strings"

	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

// ExecutionRoute specifies the execution path chosen for a task.
type ExecutionRoute string

const (
	RouteFast ExecutionRoute = "fast"
	RouteTeam ExecutionRoute = "team"

	minimumStructuredRouteConfidence = 0.60
)

// RouteDecision contains the outcome of an ExecutionRouter classification.
type RouteDecision struct {
	Route      ExecutionRoute `json:"route"`
	Team       string         `json:"team"`
	Confidence float64        `json:"confidence"`
	Reasons    []string       `json:"reasons"`
}

// ExecutionRouter routes prompts to either a fast path (single agent execution)
// or a team path (multi-agent coordinator workflow). Only explicit CLI/team
// syntax is interpreted deterministically; natural-language intent is resolved
// by a schema-validated sidecar response.
type ExecutionRouter struct {
	sidecar        *sidecar.Sidecar
	sidecarBuilder func(context.Context) *preflightSidecarHandle
}

// NewExecutionRouter constructs a new ExecutionRouter instance.
func NewExecutionRouter(_ *team.TeamRegistry, sidecar *sidecar.Sidecar) *ExecutionRouter {
	return &ExecutionRouter{
		sidecar: sidecar,
	}
}

func (r *ExecutionRouter) selectionSidecar(ctx context.Context) (*sidecar.Sidecar, context.Context, func()) {
	if r.sidecar != nil || r.sidecarBuilder == nil {
		return r.sidecar, ctx, func() {}
	}
	handle := r.sidecarBuilder(ctx)
	if handle == nil {
		return nil, ctx, func() {}
	}
	return handle.Sidecar(), handle.Context(), handle.Close
}

var classifyRouteWithSelectionSidecar = func(ctx context.Context, s *sidecar.Sidecar, prompt string) (sidecar.RouteClassification, error) {
	return s.ClassifyRoute(ctx, prompt)
}

// Route determines the route decision (fast vs team path and target team) for a prompt.
func (r *ExecutionRouter) Route(ctx context.Context, prompt string, targetTeam string) RouteDecision {
	prompt = strings.TrimSpace(prompt)

	// Check explicit CLI flag override
	switch opts.routeMode {
	case "fast":
		teamName := targetTeam
		if teamName == "" {
			teamName = "default"
		}
		return RouteDecision{
			Route:      RouteFast,
			Team:       teamName,
			Confidence: 1.0,
			Reasons:    []string{"explicit CLI flag override (--route fast)"},
		}
	case "team":
		return RouteDecision{
			Route:      RouteTeam,
			Team:       targetTeam,
			Confidence: 1.0,
			Reasons:    []string{"explicit CLI flag override (--route team)"},
		}
	}

	// A named non-default team is an explicit request for that team's
	// coordinator and workflow. Do not silently collapse it to its primary
	// worker based on the wording of a short prompt.
	if targetTeam != "" && targetTeam != "default" {
		return RouteDecision{
			Route:      RouteTeam,
			Team:       targetTeam,
			Confidence: 1.0,
			Reasons:    []string{"explicit non-default agent team requested"},
		}
	}

	// Natural-language route selection is model-backed and schema validated.
	// Construction remains lazy: explicit non-default teams never need a
	// selection preflight.
	selectionSidecar, selectionContext, closeSelectionSidecar := r.selectionSidecar(ctx)
	defer closeSelectionSidecar()
	if selectionSidecar != nil {
		if classification, err := classifyRouteWithSelectionSidecar(sidecar.WithPurpose(selectionContext, "team_selection"), selectionSidecar, prompt); err == nil && classification.Confidence >= minimumStructuredRouteConfidence {
			route := RouteFast
			if classification.Route == "team" {
				route = RouteTeam
			}
			chosenTeam := targetTeam
			if route == RouteFast && chosenTeam == "" {
				chosenTeam = "default"
			}
			return RouteDecision{
				Route:      route,
				Team:       chosenTeam,
				Confidence: classification.Confidence,
				Reasons:    []string{"sidecar LLM classification: " + classification.Reason},
			}
		}
	}

	// An unavailable, ambiguous, or invalid resolver must not guess intent.
	// The coordinator path is the safe default because it preserves planning,
	// verification, and acceptance semantics.
	return RouteDecision{
		Route:      RouteTeam,
		Team:       targetTeam,
		Confidence: 0,
		Reasons:    []string{"safe fallback: structured route resolver unavailable or invalid"},
	}
}

// CanEscalateToTeam determines if a Fast Path execution should escalate to Team Path.
func (r *ExecutionRouter) CanEscalateToTeam(currentRoute RouteDecision, stepCount int, errorCount int, requiresMultiAgent bool) (bool, string) {
	if currentRoute.Route == RouteTeam {
		return false, ""
	}
	if requiresMultiAgent {
		return true, "agent requested multi-role collaboration"
	}
	if errorCount >= 2 {
		return true, "repeated task failures in fast path"
	}
	if stepCount > 8 {
		return true, "fast path step budget exceeded"
	}
	return false, ""
}
