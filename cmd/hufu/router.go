package main

import (
	"context"
	"regexp"
	"strings"

	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

// ExecutionRoute specifies the execution path chosen for a task.
type ExecutionRoute string

const (
	RouteFast ExecutionRoute = "fast"
	RouteTeam ExecutionRoute = "team"
)

// RouteDecision contains the outcome of an ExecutionRouter classification.
type RouteDecision struct {
	Route      ExecutionRoute `json:"route"`
	Team       string         `json:"team"`
	Confidence float64        `json:"confidence"`
	Reasons    []string       `json:"reasons"`
}

// ExecutionRouter routes prompts to either a fast path (single agent execution)
// or a team path (multi-agent coordinator workflow), prioritizing deterministic signals.
type ExecutionRouter struct {
	registry *team.TeamRegistry
	sidecar  *sidecar.Sidecar
}

// NewExecutionRouter constructs a new ExecutionRouter instance.
func NewExecutionRouter(registry *team.TeamRegistry, sidecar *sidecar.Sidecar) *ExecutionRouter {
	return &ExecutionRouter{
		registry: registry,
		sidecar:  sidecar,
	}
}

// Deterministic signal patterns
var (
	multiRoleRe     = regexp.MustCompile(`(?i)\b(research\s+and\s+implement|design\s+and\s+build|review\s+and\s+fix|plan\s+and\s+execute|audit\s+and\s+refactor|coordinator|multi-agent|multi-role|workers|team)\b`)
	infraDeployRe   = regexp.MustCompile(`(?i)\b(deploy|kubernetes|k8s|ci/cd|pipeline|infra|infrastructure|cloud|security\s+audit|database\s+migration)\b`)
	refactorScopeRe = regexp.MustCompile(`(?i)\b(refactor\s+entire|across\s+packages|architecture\s+redesign|system\s+migration|full\s+codebase)\b`)
	verifyRe        = regexp.MustCompile(`(?i)\b(acceptance\s+criteria|evidence\s+chain|verify\s+with\s+tests|full\s+test\s+suite|adversarial|debate|judge|skeptic)\b`)
	multipleAtRe    = regexp.MustCompile(`@[a-zA-Z0-9_-]+.*@[a-zA-Z0-9_-]+`)

	simpleQueryRe = regexp.MustCompile(`(?i)^\s*(what\s+is|explain|how\s+to|show|list|check\s+status|print|display|summary\s+of)\b`)
	simpleFixRe   = regexp.MustCompile(`(?i)\b(fix\s+typo|add\s+comment|rename\s+variable|update\s+line|fix\s+bug\s+in\s+[a-zA-Z0-9_.-]+|run\s+test\s+[a-zA-Z0-9_.-]+)\b`)
)

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
		teamName := targetTeam
		if teamName == "" && r.registry != nil {
			teamName, _ = autoSelectTeam(ctx, prompt, r.registry)
		}
		return RouteDecision{
			Route:      RouteTeam,
			Team:       teamName,
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

	// 1. Explicit default team requested (--default)
	if opts.defaultTeam || targetTeam == "default" {
		teamSignals, tReasons := analyzeTeamSignals(prompt)
		if teamSignals >= 2 {
			reasons := append([]string{"explicit default requested but prompt has multi-agent complexity"}, tReasons...)
			return RouteDecision{
				Route:      RouteTeam,
				Team:       "default",
				Confidence: 0.70,
				Reasons:    reasons,
			}
		}
		return RouteDecision{
			Route:      RouteFast,
			Team:       "default",
			Confidence: 0.95,
			Reasons:    []string{"explicit default fast-path requested (--default)"},
		}
	}

	// 2. Deterministic Signal Analysis
	teamSignals, tReasons := analyzeTeamSignals(prompt)
	fastSignals, fReasons := analyzeFastSignals(prompt, teamSignals)

	if teamSignals > 0 && teamSignals > fastSignals {
		chosenTeam := targetTeam
		if chosenTeam == "" && r.registry != nil {
			chosenTeam, _ = autoSelectTeam(ctx, prompt, r.registry)
		}
		conf := 0.70 + 0.10*float64(teamSignals)
		if conf > 0.95 {
			conf = 0.95
		}
		return RouteDecision{
			Route:      RouteTeam,
			Team:       chosenTeam,
			Confidence: conf,
			Reasons:    tReasons,
		}
	}

	if fastSignals > 0 && fastSignals > teamSignals {
		chosenTeam := targetTeam
		if chosenTeam == "" {
			chosenTeam = "default"
		}
		conf := 0.75 + 0.08*float64(fastSignals)
		if conf > 0.95 {
			conf = 0.95
		}
		return RouteDecision{
			Route:      RouteFast,
			Team:       chosenTeam,
			Confidence: conf,
			Reasons:    fReasons,
		}
	}

	// 3. LLM Sidecar Classifier Fallback
	if r.sidecar != nil {
		if classification, err := r.sidecar.ClassifyRoute(sidecar.WithPurpose(ctx, "team_selection"), prompt); err == nil {
			route := RouteFast
			if classification.Route == "team" {
				route = RouteTeam
			}
			chosenTeam := targetTeam
			if route == RouteTeam && chosenTeam == "" && r.registry != nil {
				chosenTeam, _ = autoSelectTeam(ctx, prompt, r.registry)
			} else if route == RouteFast && chosenTeam == "" {
				chosenTeam = "default"
			}
			return RouteDecision{
				Route:      route,
				Team:       chosenTeam,
				Confidence: 0.85,
				Reasons:    []string{"sidecar LLM classification: " + classification.Reason},
			}
		}
	}

	// 4. Default Fallback
	chosenTeam := targetTeam
	if len(prompt) > 120 || strings.Contains(prompt, "\n") {
		if chosenTeam == "" && r.registry != nil {
			chosenTeam, _ = autoSelectTeam(ctx, prompt, r.registry)
		}
		return RouteDecision{
			Route:      RouteTeam,
			Team:       chosenTeam,
			Confidence: 0.60,
			Reasons:    []string{"fallback heuristic: complex or lengthy prompt"},
		}
	}

	if chosenTeam == "" {
		chosenTeam = "default"
	}
	return RouteDecision{
		Route:      RouteFast,
		Team:       chosenTeam,
		Confidence: 0.65,
		Reasons:    []string{"fallback heuristic: concise prompt without complex workflow demand"},
	}
}

func analyzeTeamSignals(prompt string) (int, []string) {
	var count int
	var reasons []string

	if multipleAtRe.MatchString(prompt) {
		count += 2
		reasons = append(reasons, "multiple agent/team references (@name)")
	}
	if multiRoleRe.MatchString(prompt) {
		count++
		reasons = append(reasons, "multi-role workflow requested (research/design/review/plan)")
	}
	if infraDeployRe.MatchString(prompt) {
		count++
		reasons = append(reasons, "infrastructure / deployment operations requested")
	}
	if refactorScopeRe.MatchString(prompt) {
		count++
		reasons = append(reasons, "wide refactor / architecture scope")
	}
	if verifyRe.MatchString(prompt) {
		count++
		reasons = append(reasons, "explicit verification / acceptance criteria")
	}
	return count, reasons
}

func analyzeFastSignals(prompt string, teamSignals int) (int, []string) {
	var count int
	var reasons []string

	if simpleQueryRe.MatchString(prompt) {
		count++
		reasons = append(reasons, "simple query or explanation prompt")
	}
	if simpleFixRe.MatchString(prompt) {
		count++
		reasons = append(reasons, "localized edit / single fix / test execution")
	}
	if teamSignals == 0 && len(prompt) < 80 && !strings.Contains(prompt, "\n") && !team.HasAtName(prompt) {
		count++
		reasons = append(reasons, "short single-sentence prompt")
	}
	return count, reasons
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
