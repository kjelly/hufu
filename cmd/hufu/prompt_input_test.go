package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
)

func TestResolveInitialSegmentsWithRedirectedStreams(t *testing.T) {
	stdin, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdout, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousStdin, previousStdout, previousOpts := os.Stdin, os.Stdout, opts
	os.Stdin, os.Stdout = stdin, stdout
	opts = runOptions{}
	t.Cleanup(func() {
		os.Stdin, os.Stdout, opts = previousStdin, previousStdout, previousOpts
		stdin.Close()
		stdinWriter.Close()
		stdout.Close()
		stdoutReader.Close()
	})

	searchPath := t.TempDir()
	makeRegistry := func(names ...string) *team.TeamRegistry {
		t.Helper()
		for _, name := range names {
			dir := filepath.Join(searchPath, name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		registry := team.NewTeamRegistry([]string{searchPath})
		if err := registry.Discover(); err != nil {
			t.Fatal(err)
		}
		return registry
	}

	registry := makeRegistry("alpha")
	if _, err := resolveInitialSegments("review the change", "", registry, nil); err != nil {
		t.Fatalf("single team should be selected: %v", err)
	}

	registry = makeRegistry("zeta")
	_, err = resolveInitialSegments("review the change", "", registry, nil)
	if err == nil || !strings.Contains(err.Error(), "alpha, zeta") || !strings.Contains(err.Error(), "--team <name>") {
		t.Fatalf("ambiguous team error = %v", err)
	}

	opts.autoTeam = true
	_, err = resolveInitialSegments("review the change", "", registry, nil)
	if err == nil || !strings.Contains(err.Error(), "--auto-team could not resolve") {
		t.Fatalf("unresolved auto-team error = %v", err)
	}

	if _, err := resolveInitialSegments("review the change", "zeta", registry, nil); err != nil {
		t.Fatalf("explicit team should still work: %v", err)
	}
}

func TestTeamSelectionWithNullInput(t *testing.T) {
	for _, name := range tools.CIEnvVars {
		t.Setenv(name, "")
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	previousStdin, previousOpts := os.Stdin, opts
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin, opts = previousStdin, previousOpts
		_ = stdin.Close()
	})

	for _, tc := range []struct {
		name    string
		teams   []string
		auto    bool
		want    string
		wantErr string
	}{
		{name: "single team", teams: []string{"alpha"}, want: "alpha"},
		{name: "ambiguous teams", teams: []string{"zeta", "alpha"}, wantErr: "multiple teams available: alpha, zeta"},
		{name: "unresolved auto-team", teams: []string{"zeta", "alpha"}, auto: true, wantErr: "--auto-team could not resolve a team"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts = runOptions{autoTeam: tc.auto}
			got, err := askUserForTeamWithPromptUI(tc.teams)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "--team <name>") {
					t.Fatalf("selection = %q, error = %v; want %q and selector guidance", got, err, tc.wantErr)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("selection = %q, error = %v; want %q", got, err, tc.want)
			}
		})
	}
}
