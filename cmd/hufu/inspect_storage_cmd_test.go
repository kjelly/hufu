package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestInspectStorageTextAndJSONParity(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{
		ID: "storage-item", Kind: contextstore.ContextObservation, Content: "private storage payload",
		Scope: contextstore.Scope{ProjectID: "project"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	jsonCommand := newInspectCommand()
	var jsonOutput bytes.Buffer
	jsonCommand.SetOut(&jsonOutput)
	jsonCommand.SetErr(&bytes.Buffer{})
	jsonCommand.SetArgs([]string{"--workspace", workspace, "--format", "json", "storage"})
	if err := jsonCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind        `json:"kind"`
		Data inspectpkg.StorageData `json:"data"`
	}
	if err := json.Unmarshal(jsonOutput.Bytes(), &envelope); err != nil {
		t.Fatalf("decode storage JSON: %v\n%s", err, jsonOutput.String())
	}
	if envelope.Kind != inspectpkg.KindStorage || envelope.Data.ContextRows != 1 || envelope.Data.FTSRows != 1 {
		t.Fatalf("storage envelope = %#v", envelope)
	}

	textCommand := newInspectCommand()
	var textOutput bytes.Buffer
	textCommand.SetOut(&textOutput)
	textCommand.SetErr(&bytes.Buffer{})
	textCommand.SetArgs([]string{"--workspace", workspace, "storage"})
	if err := textCommand.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		fmt.Sprintf("Page count: %d", envelope.Data.PageCount),
		fmt.Sprintf("Freelist count: %d", envelope.Data.FreelistCount),
		fmt.Sprintf("Page size: %d", envelope.Data.PageSize),
		"Journal mode: " + envelope.Data.JournalMode,
		fmt.Sprintf("WAL autocheckpoint: %d", envelope.Data.WALAutoCheckpoint),
		fmt.Sprintf("Schema version: %d", envelope.Data.SchemaVersion),
		fmt.Sprintf("Database bytes: %d", envelope.Data.DatabaseBytes),
		fmt.Sprintf("WAL bytes: %d", envelope.Data.WALBytes),
		fmt.Sprintf("FTS rows: %d", envelope.Data.FTSRows),
		fmt.Sprintf("Context rows: %d", envelope.Data.ContextRows),
	} {
		if !strings.Contains(textOutput.String(), expected) {
			t.Fatalf("text output missing JSON value %q:\n%s", expected, textOutput.String())
		}
	}
	for _, output := range []string{jsonOutput.String(), textOutput.String()} {
		if strings.Contains(output, "private storage payload") || strings.Contains(output, "storage-item") {
			t.Fatalf("storage inspection emitted context data: %s", output)
		}
	}
}

func TestInspectStorageMissingDatabaseUsesIntegrityExitAndCreatesNothing(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	command := newInspectCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", workspace, "storage"})
	err := command.Execute()
	if err == nil {
		t.Fatal("storage inspection unexpectedly opened a missing database")
	}
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitIntegrity {
		t.Fatalf("error = %v, want integrity exit %d", err, inspectpkg.ExitIntegrity)
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Fatalf("storage inspection created workspace: %v", statErr)
	}
}

func TestInspectStorageRejectsInheritedRunFilters(t *testing.T) {
	command := newInspectCommand()
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--workspace", t.TempDir(), "--branch", "main", "storage"})
	err := command.ExecuteContext(context.Background())
	var exitError interface{ ProcessExitCode() int }
	if !errors.As(err, &exitError) || exitError.ProcessExitCode() != inspectpkg.ExitUsage {
		t.Fatalf("error = %v, want usage exit %d", err, inspectpkg.ExitUsage)
	}
}
