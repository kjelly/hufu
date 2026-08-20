package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

type fakePreflightCoordinator struct {
	prepareErr error
	sidecar    *sidecar.Sidecar
	closeCalls int
}

func (f *fakePreflightCoordinator) PrepareContextPreflight() error {
	return f.prepareErr
}
func (f *fakePreflightCoordinator) CloseContextPreflight()    { f.closeCalls++ }
func (f *fakePreflightCoordinator) Sidecar() *sidecar.Sidecar { return f.sidecar }

func TestPreparePreflightSidecarOwnership(t *testing.T) {
	t.Run("successful handle releases exactly once", func(t *testing.T) {
		coordinator := &fakePreflightCoordinator{sidecar: &sidecar.Sidecar{}}
		handle, err := preparePreflightSidecar(coordinator)
		if err != nil {
			t.Fatal(err)
		}
		if handle.Sidecar() == nil {
			t.Fatal("successful preflight did not retain the sidecar")
		}
		handle.Close()
		handle.Close()
		if coordinator.closeCalls != 1 {
			t.Fatalf("close calls = %d, want 1", coordinator.closeCalls)
		}
	})

	t.Run("preflight error closes before returning it", func(t *testing.T) {
		want := errors.New("preflight failed")
		coordinator := &fakePreflightCoordinator{prepareErr: want}
		if _, err := preparePreflightSidecar(coordinator); !errors.Is(err, want) {
			t.Fatalf("error = %v, want %v", err, want)
		}
		if coordinator.closeCalls != 1 {
			t.Fatalf("close calls = %d, want 1", coordinator.closeCalls)
		}
	})

	t.Run("unavailable sidecar closes before fallback", func(t *testing.T) {
		coordinator := &fakePreflightCoordinator{}
		if _, err := preparePreflightSidecar(coordinator); err == nil {
			t.Fatal("expected unavailable sidecar error")
		}
		if coordinator.closeCalls != 1 {
			t.Fatalf("close calls = %d, want 1", coordinator.closeCalls)
		}
	})
}

func TestAutoSelectTeamFallbackReleasesPreflightHandle(t *testing.T) {
	searchPath := t.TempDir()
	for name, description := range map[string]string{
		"docs":  "write documentation and manuals",
		"infra": "deploy cloud infrastructure",
	} {
		dir := filepath.Join(searchPath, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("description: "+description+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := team.NewTeamRegistry([]string{searchPath})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}

	var closeCalls int
	original := selectionSidecarBuilder
	selectionSidecarBuilder = func(context.Context) *preflightSidecarHandle {
		return &preflightSidecarHandle{close: func() { closeCalls++ }}
	}
	t.Cleanup(func() { selectionSidecarBuilder = original })

	name, method := autoSelectTeam(context.Background(), "write a manual", registry)
	if name != "docs" || method != "keyword" {
		t.Fatalf("selection = (%q, %q), want keyword docs", name, method)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestAutoSelectTeamLLMSuccessReleasesPreflightHandle(t *testing.T) {
	searchPath := t.TempDir()
	for name, description := range map[string]string{
		"docs":  "write documentation and manuals",
		"infra": "deploy cloud infrastructure",
	} {
		dir := filepath.Join(searchPath, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("description: "+description+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	registry := team.NewTeamRegistry([]string{searchPath})
	if err := registry.Discover(); err != nil {
		t.Fatal(err)
	}

	var closeCalls int
	originalBuilder, originalMatcher := selectionSidecarBuilder, matchTeamWithSelectionSidecar
	selectionSidecarBuilder = func(context.Context) *preflightSidecarHandle {
		return &preflightSidecarHandle{sidecar: &sidecar.Sidecar{}, close: func() { closeCalls++ }}
	}
	matchTeamWithSelectionSidecar = func(context.Context, *sidecar.Sidecar, string, []sidecar.TeamSummary) (string, error) {
		return "docs", nil
	}
	t.Cleanup(func() {
		selectionSidecarBuilder = originalBuilder
		matchTeamWithSelectionSidecar = originalMatcher
	})

	name, method := autoSelectTeam(context.Background(), "write a manual", registry)
	if name != "docs" || method != "llm" {
		t.Fatalf("selection = (%q, %q), want LLM docs", name, method)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestMaybeAutoSelectTeamEarlyRouteReleasesPreflightHandle(t *testing.T) {
	originalBuilder, originalOpts := selectionSidecarBuilder, opts
	var closeCalls int
	selectionSidecarBuilder = func(context.Context) *preflightSidecarHandle {
		return &preflightSidecarHandle{sidecar: &sidecar.Sidecar{}, close: func() { closeCalls++ }}
	}
	opts = runOptions{defaultTeam: true}
	t.Cleanup(func() {
		selectionSidecarBuilder = originalBuilder
		opts = originalOpts
	})

	decision := maybeAutoSelectTeam(context.Background(), "explain the coordinator", "default", nil)
	if decision.Route != RouteFast || decision.Team != "default" {
		t.Fatalf("decision = %#v, want default fast route", decision)
	}
	if closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", closeCalls)
	}
}

func TestTokenSet_FiltersStopwordsAndShort(t *testing.T) {
	got := tokenSet("Please deploy the cluster to prod")
	for _, w := range []string{"deploy", "cluster", "prod"} {
		if !got[w] {
			t.Errorf("expected token %q to be kept", w)
		}
	}
	for _, w := range []string{"please", "the", "to"} {
		if got[w] {
			t.Errorf("stopword/short token %q should be dropped", w)
		}
	}
}

func TestSingularize(t *testing.T) {
	cases := map[string]string{
		"manuals":   "manual",
		"tutorials": "tutorial",
		"logs":      "log",
		"class":     "class", // -ss preserved
		"is":        "is",    // too short
		"cd":        "cd",
	}
	for in, want := range cases {
		if got := singularize(in); got != want {
			t.Errorf("singularize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeywordBestTeam(t *testing.T) {
	candidates := []sidecar.TeamSummary{
		{Name: "doc-gen", Description: "Generates documentation, manuals and tutorials from code"},
		{Name: "infra-team", Description: "Kubernetes, CI/CD pipelines, cloud infrastructure and deployment"},
		{Name: "reviewer", Description: "Reviews code for correctness, security and quality"},
	}

	cases := []struct {
		prompt string
		want   string
	}{
		{"deploy the kubernetes cluster and set up CI/CD pipelines", "infra-team"},
		{"write a tutorial and user manual for the project", "doc-gen"},
		{"review this code for security issues", "reviewer"},
	}
	for _, tc := range cases {
		if got := keywordBestTeam(tc.prompt, candidates); got != tc.want {
			t.Errorf("keywordBestTeam(%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}

func TestKeywordBestTeam_NoSignal(t *testing.T) {
	candidates := []sidecar.TeamSummary{
		{Name: "alpha", Description: "does alpha things"},
		{Name: "beta", Description: "does beta things"},
	}
	// No overlapping words → no confident pick.
	if got := keywordBestTeam("xyzzy frobnicate", candidates); got != "" {
		t.Errorf("expected empty (no signal), got %q", got)
	}
	if got := keywordBestTeam("", candidates); got != "" {
		t.Errorf("empty prompt should yield no pick, got %q", got)
	}
}

func TestTeamDescription_FromTeamYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: x\ndescription: builds rockets\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := teamDescription(dir); got != "builds rockets" {
		t.Errorf("teamDescription = %q, want 'builds rockets'", got)
	}
}

func TestTeamDescription_FallsBackToAgents(t *testing.T) {
	dir := t.TempDir()
	// No team.yaml; two agents with descriptions.
	a := "---\nname: a\ndescription: analyzes logs\n---\nbody\n"
	b := "---\nname: b\ndescription: writes reports\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	got := teamDescription(dir)
	if got == "" {
		t.Fatal("expected fallback to agent descriptions")
	}
	// Order is directory-read dependent; just assert both are present.
	for _, want := range []string{"analyzes logs", "writes reports"} {
		if !strings.Contains(got, want) {
			t.Errorf("teamDescription %q missing %q", got, want)
		}
	}
}
