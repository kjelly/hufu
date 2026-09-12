package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
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
