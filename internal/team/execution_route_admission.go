package team

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
)

var executionRouteNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func isCoordinatorAgentDef(def *agent.AgentDef) bool {
	role := strings.ToLower(strings.TrimSpace(def.Role))
	return role == "coordinator" || role == "orchestrator"
}

// validateExecutionRouteReferences checks, at team load and before hufu.yaml
// is consulted, that every route reference is well formed and that no agent
// names both a route and its own model.
func validateExecutionRouteReferences(session *TeamSession) error {
	if session == nil {
		return nil
	}
	if name := session.Config.ExecutionRoute; name != "" && !executionRouteNamePattern.MatchString(name) {
		return fmt.Errorf("%s: team execution-route %q is not a valid route name (lower-case letters, digits, and '-', starting with a letter)", executionRouteInvalidCode, name)
	}
	for _, def := range uniqueSessionAgentDefs(session) {
		if def.ExecutionRoute == "" {
			continue
		}
		switch {
		case !executionRouteNamePattern.MatchString(def.ExecutionRoute):
			return fmt.Errorf("%s: agent %q: execution-route %q is not a valid route name (lower-case letters, digits, and '-', starting with a letter)", executionRouteInvalidCode, def.Name, def.ExecutionRoute)
		case isCoordinatorAgentDef(def):
			return fmt.Errorf("%s: agent %q: execution-route applies only to workers", executionRouteInvalidCode, def.Name)
		case strings.TrimSpace(def.Generation.Model) != "":
			return fmt.Errorf("%s: agent %q sets both model %q and execution-route %q; a route supplies the worker's targets", executionRouteConflictCode, def.Name, def.Generation.Model, def.ExecutionRoute)
		}
	}
	return nil
}

// bindExecutionRoutes compiles the hufu.yaml routes the team references and
// binds each worker to its route. It runs once, after CLI overrides were
// applied and before the execution policy snapshot is built. Precedence per
// worker: its own execution-route, then its own model, then the team's
// execution-route; coordinators and auxiliary roles never bind a route.
func (c *Coordinator) bindExecutionRoutes() error {
	if c == nil || c.session == nil {
		return nil
	}
	session := c.session
	compiled := make(map[string]*ExecutionRouteDefinition)
	compile := func(name string) (*ExecutionRouteDefinition, error) {
		if route, ok := compiled[name]; ok {
			return route, nil
		}
		route, err := c.compileExecutionRoute(name, session.ExecutionRouteConfigs)
		if err != nil {
			return nil, err
		}
		compiled[name] = route
		return route, nil
	}
	if name := session.Config.ExecutionRoute; name != "" {
		if _, err := compile(name); err != nil {
			return fmt.Errorf("team execution-route: %w", err)
		}
	}
	bindings := make(map[string]string)
	decisionRoles := decisionRoleEligibleAgents(session)
	multiCandidate := false
	for _, def := range uniqueSessionAgentDefs(session) {
		if isCoordinatorAgentDef(def) {
			continue
		}
		name := def.ExecutionRoute
		if name == "" && strings.TrimSpace(def.Generation.Model) == "" {
			name = session.Config.ExecutionRoute
		}
		if name == "" {
			continue
		}
		route, err := compile(name)
		if err != nil {
			return fmt.Errorf("agent %q: %w", def.Name, err)
		}
		if len(route.Candidates) > 1 {
			multiCandidate = true
			if len(def.ExtraModels) > 0 {
				return fmt.Errorf("%s: agent %q combines extra-models with the multi-candidate execution route %q", executionRouteConflictCode, def.Name, name)
			}
			if reason, ok := decisionRoles[strings.ToLower(def.Name)]; ok {
				return fmt.Errorf("%s: agent %q can be bound as a %s and cannot use the multi-candidate execution route %q", executionRouteConflictCode, def.Name, reason, name)
			}
		}
		bindings[strings.ToLower(def.Name)] = name
	}
	if multiCandidate && session.Config.EscalateOnRetry {
		return fmt.Errorf("%s: escalate-on-retry cannot be combined with a multi-candidate execution route", executionRouteConflictCode)
	}
	// Interim gate: until fallback lands, a route may only have one target.
	// Every other check above runs first, so a later fallback build fails
	// on the same configurations this one does.
	for _, name := range sortedKeys(bindings) {
		if route := compiled[bindings[name]]; len(route.Candidates) > 1 {
			return fmt.Errorf("%s: agent %q is bound to execution route %q, which has %d candidates; this build runs only single-candidate routes", executionRouteFallbackUnsupportedCode, name, route.Name, len(route.Candidates))
		}
	}
	session.ExecutionRoutes = compiled
	session.AgentExecutionRoutes = bindings
	return nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func (c *Coordinator) compileExecutionRoute(name string, routes map[string]config.ExecutionRouteConfig) (*ExecutionRouteDefinition, error) {
	raw, ok := routes[name]
	if !ok {
		return nil, fmt.Errorf("%s: execution route %q is not defined in hufu.yaml execution-routes", executionRouteInvalidCode, name)
	}
	if !executionRouteNamePattern.MatchString(name) {
		return nil, fmt.Errorf("%s: execution route name %q is not valid", executionRouteInvalidCode, name)
	}
	if len(raw.Candidates) == 0 || len(raw.Candidates) > executionRouteMaxCandidates {
		return nil, fmt.Errorf("%s: execution route %q must list 1 to %d candidates, not %d", executionRouteInvalidCode, name, executionRouteMaxCandidates, len(raw.Candidates))
	}
	route := &ExecutionRouteDefinition{Name: name}
	for _, candidate := range raw.Candidates {
		target, err := c.executionRouteCandidate(candidate)
		if err != nil {
			return nil, fmt.Errorf("%s: execution route %q candidate %q: %w", executionRouteInvalidCode, name, candidate, err)
		}
		for _, existing := range route.Candidates {
			if execution.TargetsEqual(existing, target) {
				return nil, fmt.Errorf("%s: execution route %q lists candidate %q more than once", executionRouteInvalidCode, name, target)
			}
		}
		route.Candidates = append(route.Candidates, target)
	}
	for _, value := range raw.FallbackOn {
		class := ProviderFailureClass(strings.TrimSpace(value))
		switch {
		case slices.Contains(route.FallbackOn, class):
			return nil, fmt.Errorf("%s: execution route %q lists fallback-on %q more than once", executionRouteInvalidCode, name, class)
		case slices.Contains(fallbackEligibleProviderFailures, class):
			route.FallbackOn = append(route.FallbackOn, class)
		case slices.Contains(knownProviderFailureClasses, class):
			return nil, fmt.Errorf("%s: execution route %q: %q never triggers a fallback (allowed: %s)", executionRouteInvalidCode, name, class, providerFailureClassList(fallbackEligibleProviderFailures))
		default:
			return nil, fmt.Errorf("%s: execution route %q: unknown fallback-on value %q (allowed: %s)", executionRouteInvalidCode, name, value, providerFailureClassList(fallbackEligibleProviderFailures))
		}
	}
	if len(route.Candidates) > 1 && len(route.FallbackOn) == 0 {
		return nil, fmt.Errorf("%s: execution route %q has several candidates but no fallback-on", executionRouteInvalidCode, name)
	}
	route.Digest = executionRouteDigest(route.Name, route.Candidates, route.FallbackOn)
	return route, nil
}

// executionRouteCandidate resolves one candidate to a canonical target. It
// must name its backend, and the backend must be a language-model backend.
func (c *Coordinator) executionRouteCandidate(raw string) (execution.ExecutionTarget, error) {
	selector, err := execution.ParseExecutionSelector(strings.TrimSpace(raw))
	if err != nil {
		return execution.ExecutionTarget{}, err
	}
	if selector.Backend == "" {
		return execution.ExecutionTarget{}, fmt.Errorf("a candidate must name its backend (for example ollama/%s)", selector.Model)
	}
	target := execution.ExecutionTarget{Backend: selector.Backend, Model: selector.Model}
	if err := target.Validate(); err != nil {
		return execution.ExecutionTarget{}, err
	}
	backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
	if err != nil {
		return execution.ExecutionTarget{}, err
	}
	if backend.Kind() != execution.BackendKindLLM {
		return execution.ExecutionTarget{}, fmt.Errorf("backend %q is an agent backend; route candidates must be language-model backends", target.Backend)
	}
	return target, nil
}

func providerFailureClassList(classes []ProviderFailureClass) string {
	names := make([]string, 0, len(classes))
	for _, class := range classes {
		names = append(names, string(class))
	}
	return strings.Join(names, ", ")
}

// admittedExecutionRoute freezes a route-bound worker's route into its
// occurrence. The occurrence's target must be the route's primary. A task
// that selected another model for a multi-candidate route agent, or asked to
// escalate it, is refused: the route is that agent's only routing policy.
// A single-candidate route behaves like the agent's own model, so an
// explicit per-task model simply admits a plain target.
func (c *Coordinator) admittedExecutionRoute(task TaskDef, def *agent.AgentDef) (*ExecutionRouteBinding, error) {
	route := c.executionRouteFor(def)
	if route == nil || task.ResolvedExecutionTarget.IsZero() {
		return nil, nil
	}
	multi := len(route.Candidates) > 1
	if multi && task.Escalate {
		return nil, fmt.Errorf("%s: agent %q uses the multi-candidate execution route %q, which already defines its fallback; escalate does not apply", executionRouteConflictCode, def.Name, route.Name)
	}
	if execution.TargetsEqual(task.ResolvedExecutionTarget, route.Candidates[0]) {
		return route.binding(), nil
	}
	if multi {
		return nil, fmt.Errorf("%s: agent %q uses the execution route %q; a task cannot select model %q for it", executionRouteConflictCode, def.Name, route.Name, task.ResolvedExecutionTarget)
	}
	return nil, nil
}
