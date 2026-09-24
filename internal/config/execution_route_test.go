package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExecutionRoutesMergeAsAWhole(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home.yaml")
	project := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(home, []byte("execution-routes:\n  coding:\n    candidates: [ollama/a, openai/b]\n    fallback-on: [rate_limited]\n  review:\n    candidates: [ollama/r]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("execution-routes:\n  coding:\n    candidates: [ollama/c]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(home)
	if got := cfg.ExecutionRoutes["coding"]; !slices.Equal(got.Candidates, []string{"ollama/a", "openai/b"}) || !slices.Equal(got.FallbackOn, []string{"rate_limited"}) {
		t.Fatalf("home coding route = %+v", got)
	}
	cfg.mergeFromFile(project)
	if len(cfg.ExecutionRoutes) != 1 || !slices.Equal(cfg.ExecutionRoutes["coding"].Candidates, []string{"ollama/c"}) {
		t.Fatalf("project routes = %+v, want the project file's routes to replace the home file's", cfg.ExecutionRoutes)
	}
}

func TestExecutionRouteDecodesStrictly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hufu.yaml")
	if err := os.WriteFile(path, []byte("model: kept-only-if-valid\nexecution-routes:\n  coding:\n    candidates: [ollama/a, openai/b]\n    fallback_on: [rate_limited]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(path)
	if len(cfg.ExecutionRoutes) != 0 {
		t.Fatalf("a misspelled fallback-on key was accepted: %+v", cfg.ExecutionRoutes)
	}
}
