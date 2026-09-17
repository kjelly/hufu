package context

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupSQLiteReadOnlyCopiesCanonicalFacts(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.sqlite")
	repository, err := OpenSQLite(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repository.db.ExecContext(t.Context(), `INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES('test',NULL,'{"project_id":"legacy-project"}','{}',1)`); err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "snapshot.sqlite")
	if err = BackupSQLiteReadOnly(t.Context(), source, destination); err != nil {
		t.Fatal(err)
	}
	sourceFacts, err := InspectSQLiteSnapshot(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	destinationFacts, err := InspectSQLiteSnapshot(t.Context(), destination)
	if err != nil {
		t.Fatal(err)
	}
	if sourceFacts.Revision != destinationFacts.Revision || len(destinationFacts.ProjectIDs) != 1 || destinationFacts.ProjectIDs[0] != "legacy-project" {
		t.Fatalf("snapshot facts source=%+v destination=%+v", sourceFacts, destinationFacts)
	}
}

func TestBackupSQLiteReadOnlyHonorsCancelledContextAndDestinationExclusivity(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source.sqlite")
	repository, err := OpenSQLite(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "snapshot.sqlite")
	if err = os.WriteFile(destination, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = BackupSQLiteReadOnly(ctx, source, destination); err == nil {
		t.Fatal("backup unexpectedly replaced an existing destination")
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "owned" {
		t.Fatalf("destination changed: %q, %v", data, err)
	}
}
