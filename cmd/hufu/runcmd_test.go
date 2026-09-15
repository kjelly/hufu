package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCanonicalRunWorkspaceSemantics(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })

	exact := filepath.Join(t.TempDir(), "already-team")
	opts = runOptions{workspace: exact, workspaceMode: "exact"}
	session := &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatal(err)
	}
	if session.Workspace != exact {
		t.Fatalf("exact workspace = %q, want %q", session.Workspace, exact)
	}

	root := t.TempDir()
	opts = runOptions{workspace: root, workspaceMode: "root"}
	session = &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "review")
	if session.Workspace != want {
		t.Fatalf("root workspace = %q, want %q", session.Workspace, want)
	}
}

func TestCanonicalRunResolvesProfileBeforeRuntimeBridge(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  safe:
    no-net: "true"
    unattended: "true"
    max-duration: "60"
    max-total-tokens: "1200"
    model: "profile-model"
    output: "json"
    workspace: "/profile/workspace"
    event-format: "jsonl"
`)
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.profileName = "safe"

	command, options := newRunCommandWithOptions()
	var diagnostics bytes.Buffer
	command.SetErr(&diagnostics)
	if err := command.ParseFlags([]string{"task"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCanonicalRunOptions(command, options)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.noNet || !resolved.unattended || resolved.maxDuration != 60 || resolved.maxTotalTokens != 1200 {
		t.Fatalf("profile safety/budget options not bridged: %#v", resolved)
	}
	if resolved.modelOverride != "profile-model" || resolved.outputFormat != "json" || resolved.workspace != "/profile/workspace" || resolved.workspaceMode != "exact" {
		t.Fatalf("profile runtime options not bridged: %#v", resolved)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("JSONL profile emitted non-JSONL diagnostics: %q", diagnostics.String())
	}
}

func TestCanonicalRunExplicitZeroAndFalseOverrideProfile(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  safe:
    no-net: "true"
    max-duration: "60"
    model: "profile-model"
    output: "json"
`)
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.profileName = "safe"

	command, options := newRunCommandWithOptions()
	command.SetErr(new(bytes.Buffer))
	if err := command.ParseFlags([]string{"--no-net=false", "--max-duration=0", "--model", "cli-model", "--output", "text", "task"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveCanonicalRunOptions(command, options)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.noNet || resolved.maxDuration != 0 || resolved.modelOverride != "cli-model" || resolved.outputFormat != "text" {
		t.Fatalf("explicit CLI values did not override profile: %#v", resolved)
	}
}

func TestCanonicalRunProfileAliasConflictFailsBeforeWorkspaceMutation(t *testing.T) {
	dir := t.TempDir()
	exact := filepath.Join(dir, "must-not-exist")
	writeHufuYAML(t, dir, "profiles:\n  scoped:\n    workspace: \""+exact+"\"\n")
	defer chdir(t, dir)()
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts.profileName = "scoped"

	command, options := newRunCommandWithOptions()
	command.SetErr(new(bytes.Buffer))
	if err := command.ParseFlags([]string{"--workspace-root", filepath.Join(dir, "root"), "task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveCanonicalRunOptions(command, options); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("profile alias conflict error = %v", err)
	}
	if _, err := os.Stat(exact); !os.IsNotExist(err) {
		t.Fatalf("profile conflict mutated workspace %q: %v", exact, err)
	}
}

func TestCanonicalRunFactoryFlagsAreInstanceScoped(t *testing.T) {
	first := newRunCommand()
	second := newRunCommand()
	if err := first.ParseFlags([]string{"--team", "alpha", "--workspace", "/tmp/alpha"}); err != nil {
		t.Fatal(err)
	}
	if second.Flags().Lookup("team").Value.String() != "" || second.Flags().Lookup("workspace").Value.String() != "" {
		t.Fatal("new run command inherited values from another factory instance")
	}
}

func TestCanonicalRunExposesPresentationFlags(t *testing.T) {
	command := newRunCommand()
	for _, name := range []string{"theme", "display-preset", "display-mode", "no-spinner", "no-summary", "tui-compact", "route"} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("canonical run missing --%s", name)
		}
	}
}

func TestCanonicalRunAcceptsHyphenLeadingPromptAfterSeparator(t *testing.T) {
	command := newRunCommand()
	if err := command.ParseFlags([]string{"--team", "alpha", "--", "-audit the result"}); err != nil {
		t.Fatal(err)
	}
	args := command.Flags().Args()
	if len(args) != 1 || args[0] != "-audit the result" {
		t.Fatalf("prompt args = %#v", args)
	}
}

func TestCanonicalRunRejectsFlagConflictsBeforeExecution(t *testing.T) {
	command := newRunCommand()
	parent := t.TempDir()
	exact := filepath.Join(parent, "exact-must-not-exist")
	root := filepath.Join(parent, "root-must-not-exist")
	command.SetArgs([]string{"--workspace", exact, "--workspace-root", root, "task"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, path := range []string{exact, root} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("conflicting workspace flags mutated %q: %v", path, statErr)
		}
	}

	command = newRunCommand()
	command.SetArgs([]string{"--team", "alpha", "--agent-team", "beta", "task"})
	err = command.Execute()
	if err == nil || !strings.Contains(err.Error(), "different teams") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestValidateCanonicalRunSegments(t *testing.T) {
	segments := []team.PromptSegment{
		{Type: team.SegmentSwitchTeam, Name: "alpha"},
		{Type: team.SegmentSwitchTeam, Name: "beta"},
	}
	if err := validateCanonicalRunSegments(segments, "alpha", "root"); err == nil || !strings.Contains(err.Error(), "prompt switches") {
		t.Fatalf("explicit team conflict error = %v", err)
	}
	if err := validateCanonicalRunSegments(segments, "", "exact"); err == nil || !strings.Contains(err.Error(), "multiple teams") {
		t.Fatalf("exact multi-team error = %v", err)
	}
	if err := validateCanonicalRunSegments(segments, "", "root"); err != nil {
		t.Fatalf("root multi-team error = %v", err)
	}
}
