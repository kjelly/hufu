package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// loadLayeredConfig writes a home and a project hufu.yaml and loads them the
// way a run does, from a working directory of <root>/project.
func loadLayeredConfig(t *testing.T, homeYAML, projectYAML string) *config.Config {
	t.Helper()
	root := t.TempDir()
	homeConfig := filepath.Join(root, "home", ".config", "hufu")
	project := filepath.Join(root, "project")
	for _, dir := range []string{homeConfig, project} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(homeConfig, "hufu.yaml"), []byte(homeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "hufu.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Chdir(project)
	return config.LoadConfig()
}

func TestResolveRoleModelSourcesMatchesRoleResolution(t *testing.T) {
	const (
		homeFile    = "~/.config/hufu/hufu.yaml"
		projectFile = "./hufu.yaml"
		reviewRoute = "execution-routes:\n  review:\n    candidates: [ollama/a, ollama/b]\n"
	)
	tests := []struct {
		name        string
		homeYAML    string
		projectYAML string
		team        agent.TeamConfig
		overrides   ModelCLIOverrides
		want        []roleModelSource
	}{
		{
			name:        "team route is the worker default",
			homeYAML:    "model: ollama/home-model\nsidecar-model: ollama/home-sidecar\n",
			projectYAML: reviewRoute,
			team:        agent.TeamConfig{ExecutionRoute: "review", CoordinatorModel: "ollama/team-coordinator"},
			want: []roleModelSource{
				{Role: "Worker", Route: "review", Candidates: []string{"ollama/a", "ollama/b"}, Source: "team.yaml execution-route; candidates from " + projectFile},
				{Role: "Coordinator", Target: "ollama/team-coordinator", Source: "team.yaml coordinator-model"},
				{Role: "Sidecar", Target: "ollama/home-sidecar", Source: homeFile + " sidecar-model"},
				{Role: "Guard", Target: "ollama/home-sidecar", Source: "sidecar fallback"},
				{Role: "Judge", Target: "ollama/home-sidecar", Source: "sidecar fallback"},
				{Role: "Plan reviewer", Target: "ollama/team-coordinator", Source: "coordinator fallback"},
			},
		},
		{
			name:        "project hufu.yaml overrides home per key",
			homeYAML:    "model: ollama/home-model\nsidecar-model: ollama/home-sidecar\n",
			projectYAML: "sidecar-model: ollama/project-sidecar\njudge-model: ollama/project-judge\n",
			want: []roleModelSource{
				{Role: "Worker", Target: "ollama/home-model", Source: homeFile + " model"},
				{Role: "Coordinator", Target: "ollama/home-model", Source: homeFile + " model"},
				{Role: "Sidecar", Target: "ollama/project-sidecar", Source: projectFile + " sidecar-model"},
				{Role: "Guard", Target: "ollama/project-sidecar", Source: "sidecar fallback"},
				{Role: "Judge", Target: "ollama/project-judge", Source: projectFile + " judge-model"},
				{Role: "Plan reviewer", Target: "ollama/home-model", Source: "coordinator fallback"},
			},
		},
		{
			name:        "flags beat team and hufu.yaml and --model replaces the route",
			homeYAML:    "model: ollama/home-model\n",
			projectYAML: reviewRoute,
			team:        agent.TeamConfig{ExecutionRoute: "review", CoordinatorModel: "ollama/team-coordinator", SidecarModel: "ollama/team-sidecar"},
			overrides:   ModelCLIOverrides{Model: "ollama/cli-worker", SidecarModel: "ollama/cli-sidecar", GuardModel: "ollama/cli-guard"},
			want: []roleModelSource{
				{Role: "Worker", Target: "ollama/cli-worker", Source: "--model"},
				{Role: "Coordinator", Target: "ollama/team-coordinator", Source: "team.yaml coordinator-model"},
				{Role: "Sidecar", Target: "ollama/cli-sidecar", Source: "--sidecar-model"},
				{Role: "Guard", Target: "ollama/cli-guard", Source: "--guard-model"},
				{Role: "Judge", Target: "ollama/cli-sidecar", Source: "sidecar fallback"},
				{Role: "Plan reviewer", Target: "ollama/team-coordinator", Source: "coordinator fallback"},
			},
		},
		{
			name:     "legacy team model",
			homeYAML: "worker-model: ollama/home-worker\n",
			team:     agent.TeamConfig{Generation: agent.GenerationParams{Model: "ollama/legacy"}, PlanReviewerModel: "ollama/team-plan"},
			want: []roleModelSource{
				{Role: "Worker", Target: "ollama/home-worker", Source: homeFile + " worker-model"},
				{Role: "Coordinator", Target: "ollama/legacy", Source: "team.yaml model"},
				{Role: "Sidecar"},
				{Role: "Guard"},
				{Role: "Judge"},
				{Role: "Plan reviewer", Target: "ollama/team-plan", Source: "team.yaml plan-reviewer-model"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := loadLayeredConfig(t, tt.homeYAML, tt.projectYAML)
			session := &team.TeamSession{Config: tt.team}
			applyCLIModelOverrides(&session.Config, tt.overrides)

			got := resolveRoleModelSources(session.Config, "team.yaml", cfg, tt.overrides)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("resolveRoleModelSources() =\n%+v\nwant\n%+v", got, tt.want)
			}

			roles, err := resolveExecutionRoleModels(session, cfg)
			if err != nil {
				t.Fatalf("resolveExecutionRoleModels() error = %v", err)
			}
			wantWorker := got[0].Target
			if got[0].Route != "" {
				wantWorker = "" // the route is the default; no hufu.yaml target leaks in
			}
			resolved := []string{session.Config.WorkerModel, session.Config.CoordinatorModel, roles.Sidecar, roles.Guard, roles.Judge, roles.PlanReviewer}
			reported := []string{wantWorker, got[1].Target, got[2].Target, got[3].Target, got[4].Target, got[5].Target}
			if !reflect.DeepEqual(resolved, reported) {
				t.Fatalf("resolved targets %q, provenance reported %q", resolved, reported)
			}
		})
	}
}

func TestApplyCLIModelOverridesModelClearsTeamRoute(t *testing.T) {
	tests := []struct {
		name      string
		overrides ModelCLIOverrides
		wantRoute string
	}{
		{name: "--model replaces the route", overrides: ModelCLIOverrides{Model: "ollama/cli"}, wantRoute: ""},
		{name: "--sidecar-model keeps the route", overrides: ModelCLIOverrides{SidecarModel: "ollama/cli"}, wantRoute: "review"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := agent.TeamConfig{ExecutionRoute: "review"}
			applyCLIModelOverrides(&cfg, tt.overrides)
			if cfg.ExecutionRoute != tt.wantRoute {
				t.Errorf("ExecutionRoute = %q, want %q", cfg.ExecutionRoute, tt.wantRoute)
			}
		})
	}
}
