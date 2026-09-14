package main

import (
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
	for _, name := range []string{"theme", "display-preset", "display-mode", "no-spinner", "no-summary", "tui-compact"} {
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
	command.SetArgs([]string{"--workspace", "/tmp/exact", "--workspace-root", "/tmp/root", "task"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Execute() error = %v", err)
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
