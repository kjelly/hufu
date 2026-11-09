package context

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// OpenSQLite opens a durable context database and applies its migrations.
func OpenSQLite(path string) (*SQLiteRepository, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("open context database: resolve path: %w", err)
	}
	dsn := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	return openSQLite(dsn, path, false)
}

func openSQLite(dsn *url.URL, path string, inMemory bool) (*SQLiteRepository, error) {
	query := dsn.Query()
	// Most writable transactions read before they write. In a deferred transaction
	// that holds a read snapshot, SQLite returns SQLITE_BUSY at once when
	// another connection holds the write lock, without applying busy_timeout.
	// IMMEDIATE takes the write lock at BEGIN, so busy_timeout waits for the
	// other writer and the transaction reads its committed state. The driver
	// keeps read-only transactions (sql.TxOptions{ReadOnly: true}) deferred.
	query.Set("_txlock", "immediate")
	dsn.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite supports concurrent processes; serialize this handle's writers.
	r := &SQLiteRepository{db: db, path: path, inMemory: inMemory}
	if _, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err = r.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	r.schemaVersion = latestSchemaVersion()
	return r, nil
}

// OpenSQLiteInMemory opens a private SQLite database that disappears on Close.
// It uses the same migrations, FTS, constraints and transaction settings as
// OpenSQLite. Separate calls never share database state. projectionDir is used
// only for optional Markdown projections; no SQLite files are created there.
// An empty projectionDir disables file projections.
func OpenSQLiteInMemory(projectionDir string) (*SQLiteRepository, error) {
	path := ""
	if projectionDir != "" {
		absolute, err := filepath.Abs(projectionDir)
		if err != nil {
			return nil, fmt.Errorf("resolve context projection directory: %w", err)
		}
		path = filepath.Join(absolute, "context.sqlite")
	}
	return openSQLite(&url.URL{Scheme: "file", Opaque: ":memory:"}, path, true)
}

func (r *SQLiteRepository) journalMode(ctx context.Context) (string, error) {
	if !r.inMemory {
		return sqliteJournalModeFromHeader(r.path)
	}
	var mode string
	err := r.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode)
	return mode, err
}
