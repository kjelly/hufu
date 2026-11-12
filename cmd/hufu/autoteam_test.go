package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
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
	if name != "" || method != "" {
		t.Fatalf("selection = (%q, %q), want unresolved selection", name, method)
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

func TestMaybeAutoSelectTeamExplicitNonDefaultRouteSkipsSelectionPreflight(t *testing.T) {
	originalBuilder, originalOpts := selectionSidecarBuilder, opts
	var buildCalls int
	var closeCalls int
	selectionSidecarBuilder = func(context.Context) *preflightSidecarHandle {
		buildCalls++
		return &preflightSidecarHandle{sidecar: &sidecar.Sidecar{}, close: func() { closeCalls++ }}
	}
	opts = runOptions{routeMode: "auto"}
	t.Cleanup(func() {
		selectionSidecarBuilder = originalBuilder
		opts = originalOpts
	})

	decision := maybeAutoSelectTeam(context.Background(), "review recent commits", "hufu-code-review", nil)
	if decision.Route != RouteTeam || decision.Team != "hufu-code-review" {
		t.Fatalf("decision = %#v, want explicit named team route", decision)
	}
	if closeCalls != 0 {
		t.Fatalf("selection preflight close calls = %d, want 0", closeCalls)
	}
	if buildCalls != 0 {
		t.Fatalf("selection preflight builds = %d, want 0", buildCalls)
	}
}

func TestMaybeAutoSelectTeamClassifierFallbackBuildsSelectionPreflightLazily(t *testing.T) {
	originalBuilder, originalOpts := selectionSidecarBuilder, opts
	var buildCalls int
	selectionSidecarBuilder = func(context.Context) *preflightSidecarHandle {
		buildCalls++
		return &preflightSidecarHandle{close: func() {}}
	}
	opts = runOptions{routeMode: "auto", autoTeam: true}
	t.Cleanup(func() {
		selectionSidecarBuilder = originalBuilder
		opts = originalOpts
	})

	decision := maybeAutoSelectTeam(context.Background(), strings.Repeat("tell me something ", 5), "", nil)
	if decision.Route != RouteTeam || decision.Team != "" {
		t.Fatalf("decision = %#v, want safe unresolved team route", decision)
	}
	if buildCalls != 1 {
		t.Fatalf("selection preflight builds = %d, want 1 for classifier fallback", buildCalls)
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

func TestSelectionPreflightDoesNotFreezeTargetWorkspace(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		name := "run"
		if dryRun {
			name = "dry run"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			original := opts
			t.Cleanup(func() { opts = original })
			subject := t.TempDir()
			t.Chdir(subject)
			workspace := filepath.Join(t.TempDir(), "target")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/chat/completions" {
					t.Error("selection construction generated a model response")
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"selection-test","max_context_window":32768}],"model_info":{"general.architecture":"llama","llama.context_length":32768},"parameters":"num_ctx 32768","capabilities":["completion","tools"]}`))
			}))
			defer server.Close()
			opts = runOptions{workspace: workspace, startDir: subject, subjectRoot: subject, providerURL: server.URL + "/v1", sidecarModelOverride: "ollama/selection-test", dryRun: dryRun}
			handle := buildSelectionSidecar(t.Context())
			if dryRun {
				if handle != nil {
					handle.Close()
					t.Fatal("dry run created a selection invocation")
				}
				if _, err := os.Stat(workspace); !os.IsNotExist(err) {
					t.Fatalf("dry run touched target workspace: %v", err)
				}
				return
			}
			if handle == nil {
				t.Fatal("selection preflight unavailable")
			}
			handle.Close()
			if _, err := os.Stat(filepath.Join(workspace, "runtime.sqlite")); !os.IsNotExist(err) {
				t.Fatalf("selection preflight wrote the target event store: %v", err)
			}
			entries, err := os.ReadDir(workspace)
			if err != nil || len(entries) != 1 || !entries[0].IsDir() {
				t.Fatalf("selection did not retain an isolated control directory: %v %v", entries, err)
			}
			if _, err := os.Stat(filepath.Join(workspace, entries[0].Name(), "runtime.sqlite")); err != nil {
				t.Fatalf("selection audit store was discarded: %v", err)
			}
			session := &team.TeamSession{Dir: subject, Workspace: workspace, Config: agent.TeamConfig{Name: "actual-team"}}
			if err := session.SetCompatibilityWorkspaceScope(subject); err != nil {
				t.Fatal(err)
			}
			coordinator, err := team.NewCoordinator(session, server.URL+"/v1", "", nil, nil, team.RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			if err := freezeStartupExecutionPolicy(coordinator); err != nil {
				t.Fatalf("selection policy poisoned actual team startup: %v", err)
			}
			opts.sidecarModelOverride = "ollama/selection-next"
			next := buildSelectionSidecar(t.Context())
			if next == nil {
				t.Fatal("new auxiliary policy could not open its own selection scope")
			}
			next.Close()
			if err := freezeStartupExecutionPolicy(coordinator); err != nil {
				t.Fatalf("later selection changed the actual team's frozen policy: %v", err)
			}
		})
	}
}
