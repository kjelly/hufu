package context

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteInMemoryIsolationAndLifetime(t *testing.T) {
	dir := t.TempDir()
	for index := range 4 {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			t.Parallel()
			repo, err := OpenSQLiteInMemory(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			scope := Scope{ProjectID: "project", TeamID: "team"}
			items, err := repo.Query(t.Context(), RepositoryQuery{Scope: scope})
			if err != nil || len(items) != 0 {
				t.Fatalf("new repository: %+v, %v", items, err)
			}
			content := "isolated sentinel " + t.Name()
			if err := repo.Append(t.Context(), ContextItem{ID: "same-id", Kind: ContextPattern, Content: content, Scope: scope}); err != nil {
				t.Fatal(err)
			}
			got, err := repo.Get(t.Context(), "same-id")
			if err != nil || got.Content != content {
				t.Fatalf("isolated item: %+v, %v", got, err)
			}
			if err := repo.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.Get(t.Context(), "same-id"); err == nil {
				t.Fatal("closed repository remains usable")
			}
			reopened, err := OpenSQLiteInMemory(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			items, err = reopened.Query(t.Context(), RepositoryQuery{Scope: scope})
			if err != nil || len(items) != 0 {
				t.Fatalf("ephemeral data survived close: %+v, %v", items, err)
			}
		})
	}
}

func TestSQLiteBackendAdapterContract(t *testing.T) {
	for _, memory := range []bool{false, true} {
		name := "file"
		if memory {
			name = "memory"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			var repo *SQLiteRepository
			var err error
			if memory {
				repo, err = OpenSQLiteInMemory(dir)
			} else {
				repo, err = OpenSQLite(filepath.Join(dir, "context.sqlite"))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			if got := repo.db.Stats().MaxOpenConnections; got != 1 {
				t.Fatalf("max connections = %d", got)
			}
			var foreignKeys int
			if err := repo.db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
				t.Fatalf("foreign keys = %d, %v", foreignKeys, err)
			}
			if _, err := repo.db.ExecContext(t.Context(), "INSERT INTO context_activation(context_item_id) VALUES ('missing-parent')"); err == nil {
				t.Fatal("foreign key constraint was not enforced")
			}
			scope := Scope{ProjectID: "project", TeamID: "team", SessionID: "session"}
			if err := repo.Append(t.Context(), ContextItem{ID: "sentinel", Kind: ContextProgress, Content: "searchable sentinel", Scope: scope}); err != nil {
				t.Fatal(err)
			}
			found, err := repo.SearchLexical(t.Context(), SearchRequest{Query: "sentinel", Scope: scope})
			if err != nil || len(found) != 1 || found[0].Item.ID != "sentinel" {
				t.Fatalf("FTS lookup: %+v, %v", found, err)
			}
			other := scope
			other.TeamID = "other-team"
			found, err = repo.SearchLexical(t.Context(), SearchRequest{Query: "sentinel", Scope: other})
			if err != nil || len(found) != 0 {
				t.Fatalf("scope isolation: %+v, %v", found, err)
			}
			diagnostics, err := repo.StorageDiagnostics(t.Context())
			mode := "wal"
			if memory {
				mode = "memory"
			}
			if err != nil || diagnostics.JournalMode != mode || diagnostics.SchemaVersion != int64(latestSchemaVersion()) {
				t.Fatalf("diagnostics: %+v, %v", diagnostics, err)
			}
			if err := repo.RebuildProjection(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "context-stm.md")); err != nil {
				t.Fatal(err)
			}
			if memory {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					if _, err := os.Stat(filepath.Join(dir, "context.sqlite") + suffix); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("memory backend created database file %q: %v", suffix, err)
					}
				}
			}
		})
	}
}

func TestSQLiteInMemoryWithoutProjectionDirectory(t *testing.T) {
	repo, err := OpenSQLiteInMemory("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.RebuildProjection(t.Context(), Scope{ProjectID: "project"}); err == nil {
		t.Fatal("projection without an explicit directory was allowed")
	}
}
