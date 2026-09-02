package main

import (
	"os"
	"path/filepath"
	"testing"

	internalteam "github.com/kjelly/hufu/internal/team"
)

func TestScaffoldTemplatesHaveExpectedAgents(t *testing.T) {
	tests := []struct {
		name  string
		files []string
	}{
		{name: "default", files: []string{"worker.md"}},
		{name: "dev", files: []string{"developer.md", "reviewer.md", "tester.md"}},
		{name: "research", files: []string{"researcher.md", "writer.md"}},
		{name: "ops", files: []string{"operator.md", "monitor.md"}},
		{name: "minimal", files: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			template := scaffoldTemplates[tt.name]
			if len(template.agents) != len(tt.files) {
				t.Fatalf("agent count = %d, want %d", len(template.agents), len(tt.files))
			}
			for _, filename := range tt.files {
				if _, ok := template.agents[filename]; !ok {
					t.Errorf("missing %s", filename)
				}
			}
		})
	}
}

// runInitInDir chdirs into a fresh temp directory, runs runInit with the
// given model/template overrides, and restores cwd/opts on cleanup. It
// returns the created team directory.
func runInitInDir(t *testing.T, teamName, model, template string) string {
	t.Helper()
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	originalModel, originalTemplate := opts.modelOverride, opts.initTemplateName
	opts.modelOverride = model
	opts.initTemplateName = template
	t.Cleanup(func() {
		opts.modelOverride = originalModel
		opts.initTemplateName = originalTemplate
	})

	if err := runInit(nil, []string{teamName}); err != nil {
		t.Fatalf("runInit() error = %v", err)
	}
	return filepath.Join(dir, ".agent-teams", teamName)
}

func TestRunInitOmitsTeamYAMLWithoutModelOverride(t *testing.T) {
	teamDir := runInitInDir(t, "plain-team", "", "")

	if _, err := os.Stat(filepath.Join(teamDir, "team.yaml")); !os.IsNotExist(err) {
		t.Fatalf("team.yaml should not be created without --model (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(teamDir, "worker.md")); err != nil {
		t.Fatalf("worker.md was not created: %v", err)
	}

	if _, err := internalteam.LoadTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry); err != nil {
		t.Fatalf("generated team without team.yaml failed to load: %v", err)
	}
}

func TestRunInitWritesMinimalTeamYAMLWithModelOverride(t *testing.T) {
	teamDir := runInitInDir(t, "model-team", "local-model", "")

	got, err := os.ReadFile(filepath.Join(teamDir, "team.yaml"))
	if err != nil {
		t.Fatalf("read team.yaml: %v", err)
	}
	if want := "model: local-model\n"; string(got) != want {
		t.Fatalf("team.yaml = %q, want %q", got, want)
	}

	if _, err := internalteam.LoadTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry); err != nil {
		t.Fatalf("generated team with minimal team.yaml failed to load: %v", err)
	}
}
