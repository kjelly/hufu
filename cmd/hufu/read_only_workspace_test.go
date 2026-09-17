package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHistoryMissingWorkspaceDoesNotCreateState(t *testing.T) {
	previousWorkspace := opts.workspace
	t.Cleanup(func() { opts.workspace = previousWorkspace })
	missing := filepath.Join(t.TempDir(), "missing")
	opts.workspace = missing
	if err := historyCmd.RunE(historyCmd, []string{"query"}); err == nil {
		t.Fatal("history on a missing workspace succeeded")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("history created missing workspace: %v", err)
	}
}

func TestSessionReadCommandsDoNotCreateEventStore(t *testing.T) {
	previousWorkspace := sessionWorkspace
	t.Cleanup(func() { sessionWorkspace = previousWorkspace })
	workspace := t.TempDir()
	sessionWorkspace = workspace

	for _, command := range []struct {
		name string
		run  func() error
	}{
		{name: "list", run: func() error { return sessionListCmd.RunE(sessionListCmd, nil) }},
		{name: "tree", run: func() error { return sessionTreeCmd.RunE(sessionTreeCmd, nil) }},
		{name: "diff", run: func() error { return sessionDiffCmd.RunE(sessionDiffCmd, []string{"main", "main"}) }},
	} {
		if err := command.run(); err != nil {
			t.Fatalf("session %s: %v", command.name, err)
		}
		if _, err := os.Stat(filepath.Join(workspace, "logs")); !os.IsNotExist(err) {
			t.Fatalf("session %s created logs directory: %v", command.name, err)
		}
	}
}

func TestReadOnlyCommandMatrixDoesNotCreateMissingWorkspace(t *testing.T) {
	previousContextWorkspace, previousContextProject := contextWorkspace, contextProject
	previousStatusWorkspace := statusWorkspace
	previousTerminalWorkspace := terminalWorkspace
	t.Cleanup(func() {
		contextWorkspace, contextProject = previousContextWorkspace, previousContextProject
		statusWorkspace = previousStatusWorkspace
		terminalWorkspace = previousTerminalWorkspace
	})

	for _, command := range []struct {
		name string
		run  func(string) error
	}{
		{name: "context-list", run: func(path string) error {
			contextWorkspace, contextProject = path, "project"
			return runContextList(contextListCmd, nil)
		}},
		{name: "status", run: func(path string) error {
			statusWorkspace = path
			return runStatus(statusCmd, nil)
		}},
		{name: "terminal-list", run: func(path string) error {
			terminalWorkspace = path
			return listTerminalSessions(io.Discard, path, false)
		}},
		{name: "improve", run: func(path string) error {
			_, err := resolveImproveWorkspace(path)
			return err
		}},
	} {
		missing := filepath.Join(t.TempDir(), "missing")
		_ = command.run(missing)
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("%s created missing workspace: %v", command.name, err)
		}
	}
}
