package team

import (
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// Service fixtures use real SQLite with a private in-memory database. Tests of
// restart, cross-process, locking and snapshot behavior open durable files.
func openMemoryContextTestDB(t *testing.T, projectionDir string) *contextstore.SQLiteRepository {
	t.Helper()
	repo, err := contextstore.OpenSQLiteInMemory(projectionDir)
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
