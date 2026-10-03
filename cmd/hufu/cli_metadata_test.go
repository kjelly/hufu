package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestHelpAllShowsCanonicalGroupsAndSafetyFlags(t *testing.T) {
	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"help", "--all"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, required := range []string{"Execution:", "Teams and readiness:", "Progress and recovery:", "Advanced and diagnostics:", "run", "team", "session", "inspect"} {
		if !strings.Contains(text, required) {
			t.Fatalf("help --all missing %q:\n%s", required, text)
		}
	}

	run, _, err := root.Find([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"no-net", "force-mcp", "unattended", "rbash", "allow-path", "auto-approve", "max-duration", "max-total-tokens", "graceful-wrap-up-timeout", "workspace", "workspace-root"} {
		if run.Flags().Lookup(flag) == nil {
			t.Fatalf("canonical run help lost safety flag --%s", flag)
		}
	}
}

func TestHelpShowsCanonicalRunAndKeepsFullFlagListAvailable(t *testing.T) {
	helping := func(args ...string) string {
		t.Helper()
		root := newRootCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}

	rootHelp := helping("--help")
	if !strings.Contains(rootHelp, "hufu run --team <name>") || !strings.Contains(rootHelp, "compatibility syntax") || strings.Contains(rootHelp, "Team selection: --team / --agent-team") {
		t.Fatalf("root help does not distinguish canonical run from compatibility syntax:\n%s", rootHelp)
	}

	runHelp := helping("run", "--help")
	for _, required := range []string{"hufu run --profile coding", "interactive picker", "--no-net", "--force-mcp", "--unattended", "hufu help-flags", "hufu help --all run"} {
		if !strings.Contains(runHelp, required) {
			t.Fatalf("run help missing %q:\n%s", required, runHelp)
		}
	}
	if strings.Contains(runHelp, "--verify-timeout") {
		t.Fatalf("run help was not condensed:\n%s", runHelp)
	}

	fullHelp := helping("help", "--all", "run")
	if !strings.Contains(fullHelp, "--verify-timeout") || !strings.Contains(fullHelp, "--profile") {
		t.Fatalf("full run help unavailable:\n%s", fullHelp)
	}
}

func TestTeamListFactoryDoesNotShareFlagValues(t *testing.T) {
	first := newTeamListCommand()
	second := newTeamListCommand()
	if err := first.ParseFlags([]string{"--output", "json", "--agent-team-search-path", "/tmp/teams"}); err != nil {
		t.Fatal(err)
	}
	if second.Flags().Lookup("output").Value.String() != "text" || second.Flags().Lookup("agent-team-search-path").Value.String() != "" {
		t.Fatal("team list factory leaked flag values")
	}
}

func TestExactSessionFacadeRegistersTypedScopeFlags(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{{"session", "status"}, {"session", "resume"}, {"session", "retry"}, {"session", "reconcile"}} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, flag := range []string{"workspace", "team", "run", "branch"} {
			if command.Flag(flag) == nil {
				t.Fatalf("%v missing --%s", path, flag)
			}
		}
		if path[1] == "retry" || path[1] == "reconcile" {
			for _, flag := range []string{"task", "attempt"} {
				if command.Flag(flag) == nil {
					t.Fatalf("%v missing --%s", path, flag)
				}
			}
		}
	}
}

func TestCoordinatorModelShortFlag(t *testing.T) {
	originalOpts := opts
	t.Cleanup(func() { opts = originalOpts })

	tests := []struct {
		name    string
		command *cobra.Command
	}{
		{name: "root", command: newRootCommand()},
		{name: "run", command: newRunCommand()},
		{name: "chat", command: replCmd},
		{name: "resume", command: resumeCmd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flag := tt.command.Flags().Lookup("coordinator-model")
			if flag == nil {
				t.Fatal("missing --coordinator-model")
			}
			if flag.Shorthand != "c" {
				t.Fatalf("--coordinator-model shorthand = %q, want %q", flag.Shorthand, "c")
			}
		})
	}
}
