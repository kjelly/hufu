package team

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// Only the immutable bytes of an empty, migrated database are shared. Closing
// SQLite's last connection checkpoints its WAL; refuse to copy if either
// sidecar survives. Tests open only private copies, never the source database.
var migratedContextTestImage = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "hufu-context-template-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "context.sqlite")
	repo, err := contextstore.OpenSQLite(path)
	if err != nil {
		return nil, err
	}
	if err := repo.Close(); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("closed context template has sidecar %s: %v", suffix, err)
		}
	}
	return os.ReadFile(path)
})

func copyMigratedContextTestDB(path string) error {
	image, err := migratedContextTestImage()
	if err != nil {
		return err
	}
	// Existing databases belong to the caller (reopen/resume/locking tests).
	// Never overwrite one with a fresh fixture.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(image)
	return errors.Join(writeErr, file.Close())
}

// openMigratedContextTestDB is for service tests needing an empty current
// schema, not migration, empty-file, recovery or cross-process tests. Each
// caller owns its path and a real adapter with the production safety settings.
func openMigratedContextTestDB(t *testing.T, path string) *contextstore.SQLiteRepository {
	t.Helper()
	if err := copyMigratedContextTestDB(path); err != nil {
		t.Fatal(err)
	}
	repo, err := contextstore.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("close context fixture: %v", err)
		}
	})
	return repo
}

func TestMigratedContextTestDBIsolation(t *testing.T) {
	image, err := migratedContextTestImage()
	if err != nil {
		t.Fatal(err)
	}
	before := sha256.Sum256(image)
	t.Run("copies", func(t *testing.T) {
		for index := range 8 {
			t.Run(fmt.Sprint(index), func(t *testing.T) {
				t.Parallel()
				repo := openMigratedContextTestDB(t, filepath.Join(t.TempDir(), "context.sqlite"))
				scope := contextstore.Scope{ProjectID: "fixture-project", TeamID: "fixture-team"}
				items, err := repo.Query(t.Context(), contextstore.RepositoryQuery{Scope: scope})
				if err != nil || len(items) != 0 {
					t.Fatalf("new copy is not empty: %v, %v", items, err)
				}
				content := fmt.Sprintf("fixture sentinel %d", index)
				if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "same-id", Kind: contextstore.ContextPattern, Content: content, Scope: scope}); err != nil {
					t.Fatal(err)
				}
				got, err := repo.Get(t.Context(), "same-id")
				if err != nil || got.Content != content {
					t.Fatalf("copy shared mutable state: %+v, %v", got, err)
				}
			})
		}
	})
	if got := sha256.Sum256(image); got != before {
		t.Fatal("shared image was mutated")
	}
}

func TestMigratedContextTestDBAdapterContract(t *testing.T) {
	for _, copied := range []bool{false, true} {
		t.Run(fmt.Sprintf("copied=%v", copied), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "context.sqlite")
			var owned *contextstore.SQLiteRepository
			t.Run("lifetime", func(t *testing.T) {
				if copied {
					owned = openMigratedContextTestDB(t, path)
				} else {
					var err error
					owned, err = contextstore.OpenSQLite(path)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := owned.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				scope := contextstore.Scope{ProjectID: "fixture-project", TeamID: "fixture-team"}
				if err := owned.Append(t.Context(), contextstore.ContextItem{ID: "sentinel", Kind: contextstore.ContextPattern, Content: "fixture sentinel", Scope: scope}); err != nil {
					t.Fatal(err)
				}
				results, err := owned.SearchLexical(t.Context(), contextstore.SearchRequest{Query: "sentinel", Scope: scope})
				if err != nil || len(results) != 1 || results[0].Item.ID != "sentinel" {
					t.Fatalf("FTS contract: %+v, %v", results, err)
				}
				scope.TeamID = "other-team"
				results, err = owned.SearchLexical(t.Context(), contextstore.SearchRequest{Query: "sentinel", Scope: scope})
				if err != nil || len(results) != 0 {
					t.Fatalf("scope isolation: %+v, %v", results, err)
				}
			})
			if _, err := owned.Get(t.Context(), "sentinel"); err == nil {
				t.Fatal("fixture handle survived cleanup")
			}
			if _, err := os.Stat("/proc/self/fd"); err == nil {
				if open := openFileDescriptorsUnder(t, dir); len(open) != 0 {
					t.Fatalf("fixture leaked descriptors: %v", open)
				}
			}
			reopened, err := contextstore.OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			got, err := reopened.Get(t.Context(), "sentinel")
			if err != nil || got.Content != "fixture sentinel" {
				t.Fatalf("reopen lost persisted data: %+v, %v", got, err)
			}
		})
	}
}

func TestMigratedContextTestDBDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	const existing = "existing database belongs to its caller"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyMigratedContextTestDB(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("copy over existing DB: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != existing {
		t.Fatalf("existing DB changed: %q, %v", got, err)
	}
}
