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

func TestTeamFlagAvailableForEveryAgentTeamCommand(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	var check func(*cobra.Command)
	check = func(command *cobra.Command) {
		if command.Flag("agent-team") != nil && command.Flag("team") == nil {
			t.Errorf("%s accepts --agent-team but not --team", command.CommandPath())
		}
		for _, child := range command.Commands() {
			check(child)
		}
	}
	check(newRootCommand())
}

func TestTeamSelectionAliasesOverrideProfile(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  canonical:
    team: profile-team
  legacy:
    agent-team: profile-team
`)
	t.Chdir(dir)
	root := newRootCommand()
	for _, entry := range []struct {
		command *cobra.Command
		target  *string
	}{
		{root, &opts.agentTeamName},
		{replCmd, &opts.agentTeamName},
		{resumeCmd, &resumeTeamName},
		{retryCmd, &targetedRecoveryTeamName},
		{reconcileCmd, &targetedRecoveryTeamName},
	} {
		for _, spelling := range []string{"team", "agent-team"} {
			for _, profile := range []string{"canonical", "legacy"} {
				t.Run(entry.command.Name()+"/"+spelling+"/"+profile, func(t *testing.T) {
					for _, name := range []string{"team", "agent-team"} {
						flag := entry.command.Flags().Lookup(name)
						if flag == nil {
							t.Fatalf("missing --%s", name)
						}
						value, changed := flag.Value.String(), flag.Changed
						t.Cleanup(func() {
							_ = flag.Value.Set(value)
							flag.Changed = changed
						})
						flag.Changed = false
					}
					if err := entry.command.ParseFlags([]string{"--" + spelling, "review-team"}); err != nil {
						t.Fatal(err)
					}
					if got := *entry.target; got != "review-team" {
						t.Fatalf("command selected %q, want review-team", got)
					}
					entry.command.SetErr(&bytes.Buffer{})
					t.Cleanup(func() { entry.command.SetErr(nil) })
					if err := applyNamedProfile(entry.command, profile); err != nil {
						t.Fatal(err)
					}
					if got := *entry.target; got != "review-team" {
						t.Fatalf("profile replaced explicit team with %q", got)
					}
				})
			}
		}
	}
}

func TestTeamSelectionRoutesRootAndResultCommands(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	for _, entry := range []struct {
		name string
		path []string
		args []string
	}{
		{
			name: "root prompt",
			args: []string{"--team", "review-team", "--model", "local-model", "--auto-approve", "--report", "--new", "review latest commit"},
		},
		{
			name: "team before result",
			path: []string{"result", "final"},
			args: []string{"--team", "review-team", "result", "final"},
		},
		{
			name: "team after result",
			path: []string{"result", "final"},
			args: []string{"result", "final", "--team=review-team"},
		},
	} {
		t.Run(entry.name, func(t *testing.T) {
			root := newRootCommand()
			root.PersistentPreRun = func(*cobra.Command, []string) {}
			command, _, err := root.Find(entry.path)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			command.RunE = func(cmd *cobra.Command, _ []string) error {
				called = true
				if got := cmd.Flag("team").Value.String(); got != "review-team" {
					t.Errorf("selected team = %q, want review-team", got)
				}
				return nil
			}
			root.SetArgs(entry.args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("selected command was not executed")
			}
		})
	}
}
