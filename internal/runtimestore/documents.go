package runtimestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
	if s.readOnly {
		return fmt.Errorf("write runtime document %q: store is read-only", name)
	}
	return PutDocument(ctx, s.db, name, body)
}

// DeleteDocument removes name. A missing document is not an error.
func (s *Store) DeleteDocument(ctx context.Context, name string) error {
	if s.readOnly {
		return fmt.Errorf("delete runtime document %q: store is read-only", name)
	}
	return DeleteDocument(ctx, s.db, name)
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
	if s.readOnly {
		return fmt.Errorf("runtime store transaction: store is read-only")
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
