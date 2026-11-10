package runtimestore

import (
	"database/sql"
	"testing"

	"github.com/kjelly/hufu/internal/sqlmigrate"
)

func openMemoryTestStore(t *testing.T) (*Store, error) {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlmigrate.Apply(t.Context(), db, "runtime store", migrations, nil); err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}
