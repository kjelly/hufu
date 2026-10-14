package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Document names of the runtime state kept in runtime_documents.
const (
	DocumentSession                     = "session"
	DocumentConversationCompactionState = "conversation_compaction_state"
	DocumentSessionHistory              = "session_history"
	DocumentTerminalSessions            = "terminal_sessions"
	DocumentReliabilityEval             = "reliability_eval"
)

// Queryer is the statement surface shared by *sql.DB and *sql.Tx, so runtime
// state helpers can run alone or inside a caller's transaction.
type Queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// GetDocument returns the body of name and whether it exists.
func (s *Store) GetDocument(ctx context.Context, name string) ([]byte, bool, error) {
	return GetDocument(ctx, s.db, name)
}

// PutDocument replaces the body of name.
func (s *Store) PutDocument(ctx context.Context, name string, body []byte) error {
	if err := s.checkWritable(); err != nil {
		return fmt.Errorf("write runtime document %q: %w", name, err)
	}
	return PutDocument(ctx, s.db, name, body)
}

// DeleteDocument removes name. A missing document is not an error.
func (s *Store) DeleteDocument(ctx context.Context, name string) error {
	if err := s.checkWritable(); err != nil {
		return fmt.Errorf("delete runtime document %q: %w", name, err)
	}
	return DeleteDocument(ctx, s.db, name)
}

// Exec runs one write statement outside a caller's transaction.
func (s *Store) Exec(ctx context.Context, query string, args ...any) error {
	if err := s.checkWritable(); err != nil {
		return fmt.Errorf("runtime store write: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("runtime store write: %w", err)
	}
	return nil
}

// checkWritable refuses writes through a read-only store and through a
// connection whose file the path no longer names.
func (s *Store) checkWritable() error {
	if s.readOnly {
		return fmt.Errorf("store is read-only")
	}
	return s.CheckCurrent()
}

// GetDocument reads name through q.
func GetDocument(ctx context.Context, q Queryer, name string) ([]byte, bool, error) {
	var body []byte
	err := q.QueryRowContext(ctx, `SELECT body FROM runtime_documents WHERE name = ?`, name).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read runtime document %q: %w", name, err)
	}
	return body, true, nil
}

// PutDocument upserts name through q.
func PutDocument(ctx context.Context, q Queryer, name string, body []byte) error {
	if body == nil {
		body = []byte{}
	}
	_, err := q.ExecContext(ctx, `INSERT INTO runtime_documents(name, body, updated_at) VALUES(?, ?, ?)
ON CONFLICT(name) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at`,
		name, body, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("write runtime document %q: %w", name, err)
	}
	return nil
}

// DeleteDocument deletes name through q.
func DeleteDocument(ctx context.Context, q Queryer, name string) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM runtime_documents WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete runtime document %q: %w", name, err)
	}
	return nil
}

// WithTx runs fn in one IMMEDIATE transaction and commits when fn returns
// nil. Any error rolls back every write fn made.
func (s *Store) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := s.checkWritable(); err != nil {
		return fmt.Errorf("runtime store transaction: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin runtime store transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, rollback(tx))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit runtime store transaction: %w", err)
	}
	return nil
}

func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("roll back runtime store transaction: %w", err)
	}
	return nil
}

// IsStorageFault reports whether err is SQLite failing to reach or write the
// database file (cannot open, I/O error, full disk, permissions, read-only
// media, or a lock held past busy_timeout): faults an operator can fix and
// retry. Corruption is not one: it means the stored evidence is invalid.
func IsStorageFault(err error) bool {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_CANTOPEN, sqlite3.SQLITE_IOERR, sqlite3.SQLITE_FULL, sqlite3.SQLITE_PERM,
		sqlite3.SQLITE_READONLY, sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED, sqlite3.SQLITE_NOMEM:
		return true
	}
	return false
}

// IsBusy reports whether err is SQLite refusing a lock another connection
// holds past busy_timeout.
func IsBusy(err error) bool {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}
