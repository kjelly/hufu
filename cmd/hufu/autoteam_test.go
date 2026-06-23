package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/sidecar"
)

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
