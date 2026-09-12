package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestMigrateInspectExecutionCommandUsesRootWorkspaceAndStableJSON(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "session.json"), []byte(`{"tasks":[{"id":"legacy","model":"qwen3:8b","subagent_provider":"hufu-local"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previousOpts := opts
	previousBranch := migrateInspectExecutionBranch
	previousJSON := migrateInspectExecutionJSON
	t.Cleanup(func() {
		opts = previousOpts
		migrateInspectExecutionBranch = previousBranch
		migrateInspectExecutionJSON = previousJSON
	})
	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--workspace", workspace, "migrate", "inspect-execution", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute inspect command: %v", err)
	}
	got := output.String()
	if !bytes.Contains([]byte(got), []byte(`"schema_version":1`)) || !bytes.Contains([]byte(got), []byte(`"migratable_tasks":1`)) {
		t.Fatalf("JSON output = %s", got)
	}
}

func TestMigrateCommandIsRegistered(t *testing.T) {
	root := newRootCommand()
	command, _, err := root.Find([]string{"migrate", "inspect-execution"})
	if err != nil {
		t.Fatal(err)
	}
	if command == nil || command.Name() != "inspect-execution" {
		t.Fatalf("migrate inspect command = %#v", command)
	}
}

func TestMigrateApplyExecutionRequiresExplicitApply(t *testing.T) {
	previousOpts := opts
	previousApply := migrateApplyExecutionApply
	previousBranch := migrateApplyExecutionBranch
	previousJSON := migrateApplyExecutionJSON
	t.Cleanup(func() {
		opts = previousOpts
		migrateApplyExecutionApply = previousApply
		migrateApplyExecutionBranch = previousBranch
		migrateApplyExecutionJSON = previousJSON
	})
	root := newRootCommand()
	root.SetArgs([]string{"migrate", "apply-execution"})
	if err := root.Execute(); err == nil {
		t.Fatal("apply-execution accepted a missing --apply confirmation")
	}
}

func TestMigrateApplyExecutionCommandIsRegistered(t *testing.T) {
	root := newRootCommand()
	command, _, err := root.Find([]string{"migrate", "apply-execution"})
	if err != nil {
		t.Fatal(err)
	}
	if command == nil || command.Name() != "apply-execution" {
		t.Fatalf("migrate apply command = %#v", command)
	}
}

func TestInspectorDoesNotEmitCompatibilityObservation(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "session.json"), []byte(`{"tasks":[{"id":"legacy","model":"qwen3:8b","subagent_provider":"hufu-local"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	previousOpts := opts
	previousBranch := migrateInspectExecutionBranch
	previousJSON := migrateInspectExecutionJSON
	t.Cleanup(func() {
		opts = previousOpts
		migrateInspectExecutionBranch = previousBranch
		migrateInspectExecutionJSON = previousJSON
	})
	root := newRootCommand()
	root.SetArgs([]string{"--workspace", workspace, "migrate", "inspect-execution"})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute inspect command: %v", err)
	}
	store, err = team.OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == string(team.EventExecutionCompatibilityObserved) {
			t.Fatal("inspector emitted an execution compatibility observation")
		}
	}
}
