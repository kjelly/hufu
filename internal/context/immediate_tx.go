package context

import (
	"context"
	"database/sql"
	"fmt"
)

// execQueryer is the read and write surface shared by *sql.Tx and the
// *sql.Conn that withImmediateTx holds inside an explicit transaction.
type execQueryer interface {
	queryer
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// withImmediateTx runs fn inside a BEGIN IMMEDIATE transaction and commits
// when fn succeeds. Use it for transactions that read before they write.
// A deferred transaction that has already read cannot become a writer while
// another connection writes: SQLite returns SQLITE_BUSY at once instead of
// applying busy_timeout. Taking the write lock first lets busy_timeout wait
// for the other writer, and every read then sees its committed state.
func (r *SQLiteRepository) withImmediateTx(ctx context.Context, fn func(tx execQueryer) error) error {
	return r.withBusyRetry(ctx, func() error {
		conn, err := r.db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire connection: %w", err)
		}
		defer func() { _ = conn.Close() }()
		if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			return fmt.Errorf("begin immediate transaction: %w", err)
		}
		committed := false
		defer func() {
			if !committed {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			}
		}()
		if err = fn(conn); err != nil {
			return err
		}
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return fmt.Errorf("commit immediate transaction: %w", err)
		}
		committed = true
		return nil
	})
}
