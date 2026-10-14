package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestOpenCreatesAPrivateMigratedStore(t *testing.T) {
	workspace := t.TempDir()
	store, err := Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	info, err := os.Stat(Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("runtime.sqlite mode = %v, want 0600", info.Mode().Perm())
	}
	var version int
	if err := store.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrations[len(migrations)-1].Version {
		t.Fatalf("schema version = %d, want %d", version, migrations[len(migrations)-1].Version)
	}
}

func TestOpenSharesOneConnectionPerWorkspace(t *testing.T) {
	workspace := t.TempDir()
	first, err := Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if first.DB() != second.DB() {
		t.Fatal("two writable opens of one workspace did not share a connection")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// The other user keeps the shared connection usable.
	if err := second.PutDocument(context.Background(), DocumentSession, []byte(`{}`)); err != nil {
		t.Fatalf("shared connection closed with its first user: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	if third.DB() == second.DB() {
		t.Fatal("a closed shared connection was reused")
	}
}

func TestOpenRefusesFileBasedWorkspaces(t *testing.T) {
	tests := []struct {
		name   string
		marker string
	}{
		{name: "event log", marker: filepath.Join("logs", "event_store.jsonl")},
		{name: "session checkpoint", marker: "session.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, tt.marker)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(context.Background(), workspace); !errors.Is(err, ErrLegacyWorkspace) {
				t.Fatalf("Open error = %v, want ErrLegacyWorkspace", err)
			}
			if _, err := OpenReadOnly(context.Background(), workspace); !errors.Is(err, ErrLegacyWorkspace) {
				t.Fatalf("OpenReadOnly error = %v, want ErrLegacyWorkspace", err)
			}
			if Exists(workspace) {
				t.Fatal("a refused open created runtime.sqlite")
			}
		})
	}
}

func TestOpenReadOnlyNeverWrites(t *testing.T) {
	workspace := t.TempDir()
	if _, err := OpenReadOnly(context.Background(), workspace); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenReadOnly of an empty workspace = %v, want ErrNotFound", err)
	}
	if names := dirNames(t, workspace); len(names) != 0 {
		t.Fatalf("read-only open created %v", names)
	}

	writable, err := Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := writable.PutDocument(context.Background(), DocumentSession, []byte(`{"rounds":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	before := dirNames(t, workspace)
	reader, err := OpenReadOnly(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	body, ok, err := reader.GetDocument(context.Background(), DocumentSession)
	if err != nil || !ok || string(body) != `{"rounds":1}` {
		t.Fatalf("read-only document = %q, %v, %v", body, ok, err)
	}
	if err := reader.PutDocument(context.Background(), DocumentSession, []byte(`{}`)); err == nil {
		t.Fatal("read-only store accepted a write")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if after := dirNames(t, workspace); !slices.Equal(before, after) {
		t.Fatalf("read-only open changed the workspace: %v -> %v", before, after)
	}
}

func TestOpenRejectsAnUnknownSchemaVersion(t *testing.T) {
	workspace := t.TempDir()
	store, err := Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`INSERT INTO schema_migrations(version, name, applied_at, checksum) VALUES(99, 'future', 0, 'x')`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), workspace); err == nil {
		t.Fatal("Open accepted a database migrated by a newer hufu")
	}
	if _, err := OpenReadOnly(context.Background(), workspace); err == nil {
		t.Fatal("OpenReadOnly accepted a database migrated by a newer hufu")
	}
}

func TestRuntimeDocuments(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	tests := []struct {
		name   string
		act    func() error
		want   string
		exists bool
	}{
		{name: "missing", act: func() error { return nil }},
		{name: "insert", act: func() error { return store.PutDocument(ctx, DocumentSession, []byte(`{"a":1}`)) }, want: `{"a":1}`, exists: true},
		{name: "replace", act: func() error { return store.PutDocument(ctx, DocumentSession, []byte(`{"a":2}`)) }, want: `{"a":2}`, exists: true},
		{name: "delete", act: func() error { return store.DeleteDocument(ctx, DocumentSession) }},
		{name: "delete again", act: func() error { return store.DeleteDocument(ctx, DocumentSession) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.act(); err != nil {
				t.Fatal(err)
			}
			body, ok, err := store.GetDocument(ctx, DocumentSession)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.exists || string(body) != tt.want {
				t.Fatalf("document = %q (exists %v), want %q (exists %v)", body, ok, tt.want, tt.exists)
			}
		})
	}
}

func TestWithTxRollsBackEveryWriteOnError(t *testing.T) {
	store, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.PutDocument(ctx, DocumentSession, []byte(`before`)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected")
	err = store.WithTx(ctx, func(tx *sql.Tx) error {
		if err := PutDocument(ctx, tx, DocumentSession, []byte(`after`)); err != nil {
			return err
		}
		if err := PutDocument(ctx, tx, DocumentSessionHistory, []byte(`history`)); err != nil {
			return err
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("WithTx error = %v, want the injected failure", err)
	}
	if body, _, _ := store.GetDocument(ctx, DocumentSession); string(body) != "before" {
		t.Fatalf("session after rollback = %q, want before", body)
	}
	if _, ok, _ := store.GetDocument(ctx, DocumentSessionHistory); ok {
		t.Fatal("a rolled-back transaction left a document")
	}
}
