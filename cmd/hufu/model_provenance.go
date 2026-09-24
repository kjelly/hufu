package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// roleModelSource is one run role's effective model and the configuration
// layer that supplied it. Models can be set by CLI flags, team.yaml, agent
// frontmatter, and two hufu.yaml files; this names the layer that won.
type roleModelSource struct {
	Role string
	// Target is the effective model selector, or "" when the role is not
	// configured or its default is an execution route.
	Target string
	// Route and Candidates describe a team execution route that is the
	// worker default; Candidates come from hufu.yaml execution-routes.
	Route      string
	Candidates []string
	Source     string
}

// Display renders the effective value: the model, or the route with its
// ordered fallback candidates.
func (s roleModelSource) Display() string {
	if s.Route != "" {
		return "route " + s.Route + " → " + strings.Join(s.Candidates, ", ")
	}
	return s.Target
}

type modelLayer struct{ value, source string }

func pickModelLayer(layers ...modelLayer) (string, string) {
	for _, layer := range layers {
		if strings.TrimSpace(layer.value) != "" {
			return layer.value, layer.source
		}
	}
	return "", ""
}

// resolveRoleModelSources mirrors resolveExecutionRoleModels layer by layer
// and records which layer won for each role. teamCfg must already carry the
// CLI overrides but not yet the resolved worker/coordinator targets, which is
// the state loadTeamCommon holds just before resolveExecutionRoleModels.
func resolveRoleModelSources(teamCfg agent.TeamConfig, teamLabel string, cfg *config.Config, overrides ModelCLIOverrides) []roleModelSource {
	if cfg == nil {
		cfg = &config.Config{}
	}
	hufu := func(key string) string { return configSourceLabel(cfg.Source(key)) + " " + key }
	inTeam := func(key string) string { return teamLabel + " " + key }

	worker := roleModelSource{Role: "Worker"}
	switch {
	case overrides.Model != "":
		worker.Target, worker.Source = overrides.Model, "--model"
	case teamCfg.ExecutionRoute != "":
		worker.Route = teamCfg.ExecutionRoute
		worker.Candidates = cfg.ExecutionRoutes[teamCfg.ExecutionRoute].Candidates
		worker.Source = inTeam("execution-route")
		if routeFile := cfg.Source("execution-routes"); routeFile != "" {
			worker.Source += "; candidates from " + configSourceLabel(routeFile)
		}
	default:
		worker.Target, worker.Source = pickModelLayer(
			modelLayer{teamCfg.WorkerModel, inTeam("worker-model")},
			modelLayer{cfg.WorkerModel, hufu("worker-model")},
			modelLayer{teamCfg.Generation.Model, inTeam("model")},
			modelLayer{cfg.Model, hufu("model")},
		)
	}

	coordinator := roleModelSource{Role: "Coordinator"}
	coordinator.Target, coordinator.Source = pickModelLayer(
		modelLayer{overrides.CoordinatorModel, "--coordinator-model"},
		modelLayer{teamCfg.CoordinatorModel, inTeam("coordinator-model")},
		modelLayer{cfg.CoordinatorModel, hufu("coordinator-model")},
		modelLayer{teamCfg.Generation.Model, inTeam("model")},
		modelLayer{cfg.Model, hufu("model")},
	)

	sidecar := roleModelSource{Role: "Sidecar"}
	sidecar.Target, sidecar.Source = pickModelLayer(
		modelLayer{overrides.SidecarModel, "--sidecar-model"},
		modelLayer{teamCfg.SidecarModel, inTeam("sidecar-model")},
		modelLayer{cfg.SidecarModel, hufu("sidecar-model")},
	)
	auxiliary := func(role, flag, teamValue, hufuValue, key string) roleModelSource {
		source := roleModelSource{Role: role}
		source.Target, source.Source = pickModelLayer(
			modelLayer{flag, "--" + key},
			modelLayer{teamValue, inTeam(key)},
			modelLayer{hufuValue, hufu(key)},
			modelLayer{sidecar.Target, "sidecar fallback"},
		)
		return source
	}
	guard := auxiliary("Guard", overrides.GuardModel, teamCfg.GuardModel, cfg.GuardModel, "guard-model")
	judge := auxiliary("Judge", overrides.JudgeModel, teamCfg.JudgeModel, cfg.JudgeModel, "judge-model")

	planReviewer := roleModelSource{Role: "Plan reviewer"}
	planReviewer.Target, planReviewer.Source = pickModelLayer(
		modelLayer{overrides.PlanReviewerModel, "--plan-reviewer-model"},
		modelLayer{teamCfg.PlanReviewerModel, inTeam("plan-reviewer-model")},
		modelLayer{cfg.PlanReviewerModel, hufu("plan-reviewer-model")},
		modelLayer{coordinator.Target, "coordinator fallback"},
		modelLayer{cfg.Model, hufu("model")},
	)
	return []roleModelSource{worker, coordinator, sidecar, guard, judge, planReviewer}
}

// roleSourceFor returns the source recorded for role, or "".
func roleSourceFor(sources []roleModelSource, role string) string {
	for _, source := range sources {
		if strings.EqualFold(source.Role, role) {
			return source.Source
		}
	}
	return ""
}

// teamSourceLabel names the team manifest a team-level value came from.
func teamSourceLabel(session *team.TeamSession) string {
	if session != nil && session.Dir != "" {
		for _, name := range []string{"team.yml", "team.yaml"} {
			path := filepath.Join(session.Dir, name)
			if _, err := os.Stat(path); err == nil {
				return displayConfigPath(path)
			}
		}
	}
	return "team config"
}

// configSourceLabel names the hufu.yaml file a value came from.
func configSourceLabel(path string) string {
	if path == "" {
		return "hufu.yaml"
	}
	return displayConfigPath(path)
}

// displayConfigPath shortens path relative to the working directory or home.
func displayConfigPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil {
		if rel, relErr := filepath.Rel(cwd, absolute); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "." + string(filepath.Separator) + rel
		}
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
		if rel, relErr := filepath.Rel(home, absolute); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "~" + string(filepath.Separator) + rel
		}
	}
	return absolute
}

// executionRouteConfigs returns hufu.yaml's execution routes, preferring the
// copy the host already bound onto the session.
func executionRouteConfigs(session *team.TeamSession, cfg *config.Config) map[string]config.ExecutionRouteConfig {
	if session != nil && len(session.ExecutionRouteConfigs) > 0 {
		return session.ExecutionRouteConfigs
	}
	if cfg == nil {
		return nil
	}
	return cfg.ExecutionRoutes
}

// boundExecutionRoute mirrors route binding: a worker's own route wins, and
// a worker with neither a route nor a model inherits the team route.
func boundExecutionRoute(session *team.TeamSession, def *agent.AgentDef) string {
	if def == nil {
		return ""
	}
	if def.ExecutionRoute != "" {
		return def.ExecutionRoute
	}
	if session != nil && strings.TrimSpace(def.Generation.Model) == "" {
		return session.Config.ExecutionRoute
	}
	return ""
}
