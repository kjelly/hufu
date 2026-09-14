package context

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestOpenSQLiteReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "context.sqlite")
	if _, err := OpenSQLiteReadOnly(path); err == nil {
		t.Fatal("open read-only context database succeeded for a missing file")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read-only open created parent directory: %v", err)
	}
}

func TestOpenSQLiteReadOnlyDoesNotMigrateOrMutateDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "context.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE sentinel (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeNames := directoryNames(t, dir)
	repository, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	concrete := repository.(*SQLiteRepository)
	if err := concrete.Append(t.Context(), ContextItem{}); err == nil {
		t.Fatal("read-only SQLite repository accepted a mutation")
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only open changed database bytes")
	}
	afterNames := directoryNames(t, dir)
	if !slices.Equal(beforeNames, afterNames) {
		t.Fatalf("read-only open changed directory entries: before=%v after=%v", beforeNames, afterNames)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var migrations int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != 0 {
		t.Fatal("read-only open applied context migrations")
	}
}

func TestOpenSQLiteReadOnlyQueriesExistingContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	writable, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	item := ContextItem{
		ID:         "ctx-read-only",
		Kind:       ContextObservation,
		Content:    "persisted observation",
		Scope:      Scope{ProjectID: "project"},
		Authority:  AuthorityTool,
		TrustLevel: TrustInternal,
	}
	if err := writable.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	got, err := readOnly.GetScoped(t.Context(), item.ID, ScopedReadOptions{
		Scope: Scope{ProjectID: "project"}, IncludeContent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != item.ID || got.Content != item.Content {
		t.Fatalf("read-only Get = %#v, want %q", got, item.ID)
	}
}

func TestOpenSQLiteReadOnlyQueriesCommittedWALContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	writable, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	item := ContextItem{
		ID: "ctx-in-wal", Kind: ContextObservation, Content: "committed WAL observation",
		Scope: Scope{ProjectID: "project"}, Authority: AuthorityTool, TrustLevel: TrustInternal,
	}
	if err := writable.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("fixture WAL: %v", err)
	}

	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	got, err := readOnly.GetScoped(t.Context(), item.ID, ScopedReadOptions{
		Scope: Scope{ProjectID: "project"}, IncludeContent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != item.ID || got.Content != item.Content {
		t.Fatalf("read-only WAL Get = %#v, want %q", got, item.ID)
	}
}

func TestSQLiteReadOnlyGetScopedAuthorizesBeforeContentHydration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	writable, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	item := ContextItem{
		ID: "ctx-private", Kind: ContextObservation, Content: "private unredacted detail",
		Scope: Scope{ProjectID: "project", TeamID: "team", AgentID: "worker"},
	}
	if err := writable.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()

	for name, options := range map[string]ScopedReadOptions{
		"wrong project": {Scope: Scope{ProjectID: "other", TeamID: "team", AgentID: "worker"}},
		"wrong team":    {Scope: Scope{ProjectID: "project", TeamID: "other", AgentID: "worker"}},
		"missing agent": {Scope: Scope{ProjectID: "project", TeamID: "team"}},
		"wrong agent":   {Scope: Scope{ProjectID: "project", TeamID: "team", AgentID: "other"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readOnly.GetScoped(t.Context(), item.ID, options); !errors.Is(err, ErrReadScopeDenied) {
				t.Fatalf("GetScoped error = %v, want ErrReadScopeDenied", err)
			}
		})
	}

	metadataOnly, err := readOnly.GetScoped(t.Context(), item.ID, ScopedReadOptions{
		Scope: Scope{ProjectID: "project", TeamID: "team", AgentID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadataOnly.Content != "" {
		t.Fatalf("metadata-only read hydrated content %q", metadataOnly.Content)
	}

	withContent, err := readOnly.GetScoped(t.Context(), item.ID, ScopedReadOptions{
		Scope: Scope{ProjectID: "project", TeamID: "team", AgentID: "worker"}, IncludeContent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withContent.Content != item.Content {
		t.Fatalf("content read = %q, want %q", withContent.Content, item.Content)
	}

	allAgents, err := readOnly.GetScoped(t.Context(), item.ID, ScopedReadOptions{
		Scope: Scope{ProjectID: "project", TeamID: "team"}, AllAgents: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if allAgents.Content != "" {
		t.Fatalf("all-agents metadata read hydrated content %q", allAgents.Content)
	}
}

func directoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}
