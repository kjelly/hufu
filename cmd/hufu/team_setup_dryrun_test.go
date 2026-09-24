package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

// resolverPreviewTeamYAML declares a typed input whose value comes from a
// deterministic resolver, the shape hufu-code-review uses for review.scope.
// The resolver reports "3" at bytes 12-13 of "review last 3 commits".
const resolverPreviewTeamYAML = `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: resolver-preview
spec:
  worker-model: ollama/fixture-worker
  coordinator-model: ollama/fixture-coordinator
  sidecar-model: ollama/fixture-sidecar
  judge-model: ollama/fixture-judge
  inputs:
    count:
      type: integer
      required: true
      resolver:
        id: count-v1
        capability: resolve-count
        type: resolve_count
        source: invocation_prompt
        side_effect: none
        timeout: 5
  action-providers:
    resolve-count:
      command: [/bin/sh, -c, 'cat >/dev/null; printf "{\"status\":\"matched\",\"value\":3,\"evidence\":[{\"source\":\"prompt\",\"start\":12,\"end\":13,\"kind\":\"count\"}],\"resolver_version\":\"1\"}"']
`

// loadDryRunFixtureTeam loads a one-worker team through the CLI dry-run
// path. HOME is isolated from the developer's ~/.config/hufu/hufu.yaml;
// homeYAML and projectYAML, when set, become the two hufu.yaml layers.
func loadDryRunFixtureTeam(t *testing.T, name, teamYAML, homeYAML, projectYAML string) (tc *teamContext, stateRoot string) {
	t.Helper()
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })

	root := t.TempDir()
	project := filepath.Join(root, "project")
	homeConfig := filepath.Join(root, "home", ".config", "hufu")
	teamSearch := filepath.Join(root, "teams")
	teamDir := filepath.Join(teamSearch, name)
	for _, dir := range []string{filepath.Join(project, ".git"), homeConfig, teamDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(teamDir, "team.yaml"):    teamYAML,
		filepath.Join(teamDir, "analyst.md"):   "---\nname: analyst\nrole: worker\n---\nPreview tasks.\n",
		filepath.Join(homeConfig, "hufu.yaml"): homeYAML,
		filepath.Join(project, "hufu.yaml"):    projectYAML,
	}
	for path, content := range files {
		if content == "" {
			continue
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stateRoot = filepath.Join(root, "state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Chdir(project)
	opts = runOptions{dryRun: true, canonicalRun: true}
	registry := team.NewTeamRegistry([]string{teamSearch})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}
	tc, err := loadTeamByName(t.Context(), name, registry, "", "", nil, nil, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tc.coordinator.Close() })
	return tc, stateRoot
}

func TestLoadTeamDryRunResolvesRunInputsWithFrozenPolicy(t *testing.T) {
	tc, stateRoot := loadDryRunFixtureTeam(t, "resolver-preview", resolverPreviewTeamYAML, "", "")
	result, err := tc.coordinator.DryRun(t.Context(), "review last 3 commits")
	if err != nil {
		t.Fatalf("DryRun() error = %v", err)
	}

	inputs := result.ResolvedRunInputs
	if inputs == nil || len(inputs.Inputs) != 1 {
		t.Fatalf("resolved run inputs = %#v, want one resolved input", inputs)
	}
	if got := inputs.Inputs[0]; got.Name != "count" || string(got.CanonicalValue) != "3" || got.Source != team.RunInputSourceResolver {
		t.Fatalf("resolved input = %+v, want count=3 from resolver", got)
	}

	tests := []struct {
		role string
		got  string
		want string
	}{
		{role: "worker", got: result.WorkerTarget, want: "ollama/fixture-worker"},
		{role: "coordinator", got: result.CoordinatorTarget, want: "ollama/fixture-coordinator"},
		{role: "sidecar", got: result.SidecarTarget, want: "ollama/fixture-sidecar"},
		{role: "guard falls back to sidecar", got: result.GuardTarget, want: "ollama/fixture-sidecar"},
		{role: "judge", got: result.JudgeTarget, want: "ollama/fixture-judge"},
		{role: "plan reviewer falls back to coordinator", got: result.PlanReviewerTarget, want: "ollama/fixture-coordinator"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s target = %q, want %q", tt.role, tt.got, tt.want)
		}
	}
	if _, statErr := os.Stat(stateRoot); !os.IsNotExist(statErr) {
		t.Fatalf("dry-run created state root: %v", statErr)
	}
}

func TestLoadTeamDryRunShowsTeamRouteAsWorkerDefault(t *testing.T) {
	const teamYAML = "name: route-preview\ncoordinator-model: ollama/fixture-coordinator\nexecution-route: review\n"
	// The home model names a backend nothing defines. The team route makes it
	// an unused default, so it must neither fail preflight nor show up.
	tc, _ := loadDryRunFixtureTeam(t, "route-preview", teamYAML,
		"model: bogus/unused\nsidecar-model: ollama/home-sidecar\n",
		"execution-routes:\n  review:\n    candidates: [ollama/a, ollama/b]\n    fallback-on: [rate_limited]\n")
	result, err := tc.coordinator.DryRun(t.Context(), "preview")
	if err != nil {
		t.Fatalf("DryRun() error = %v", err)
	}
	if result.WorkerTarget != "" || result.WorkerRoute != "review" || !slices.Equal(result.WorkerRouteCandidates, []string{"ollama/a", "ollama/b"}) {
		t.Fatalf("worker target = %q route = %q candidates = %v, want route review with no worker target", result.WorkerTarget, result.WorkerRoute, result.WorkerRouteCandidates)
	}
	for _, info := range result.Agents {
		if info.Role == "worker" && info.ExecutionTarget != "ollama/a" {
			t.Errorf("worker %s target = %q, want the route's first candidate", info.Name, info.ExecutionTarget)
		}
	}
	output := formatDryRun(result, tc.roleSources)
	for _, want := range []string{
		"route review → ollama/a, ollama/b",
		"execution-route; candidates from ./hufu.yaml)",
		"ollama/home-sidecar",
		"(~/.config/hufu/hufu.yaml sidecar-model)",
		"(coordinator fallback)",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "bogus/unused") {
		t.Errorf("dry-run output shows the unused hufu.yaml model:\n%s", output)
	}
}

func TestFormatDryRunListsEachRoleTargetOnce(t *testing.T) {
	output := formatDryRun(&team.DryRunResult{
		TeamName:           "preview",
		WorkerTarget:       "ollama/worker",
		CoordinatorTarget:  "ollama/coordinator",
		SidecarTarget:      "ollama/sidecar",
		GuardTarget:        "ollama/guard",
		JudgeTarget:        "ollama/judge",
		PlanReviewerTarget: "ollama/plan-reviewer",
	}, nil)
	tests := []struct {
		label  string
		target string
	}{
		{label: "Worker:", target: "ollama/worker"},
		{label: "Coordinator:", target: "ollama/coordinator"},
		{label: "Sidecar:", target: "ollama/sidecar"},
		{label: "Guard:", target: "ollama/guard"},
		{label: "Judge:", target: "ollama/judge"},
		{label: "Plan reviewer:", target: "ollama/plan-reviewer"},
	}
	for _, tt := range tests {
		lines := 0
		for line := range strings.SplitSeq(output, "\n") {
			if strings.Contains(line, tt.label) && strings.HasSuffix(strings.TrimSpace(line), tt.target) {
				lines++
			}
		}
		if lines != 1 {
			t.Errorf("%s %s appears on %d lines, want 1:\n%s", tt.label, tt.target, lines, output)
		}
	}
}
