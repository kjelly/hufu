package improve

// sqliteAnalyticsSession backs the SQL-based telemetry aggregation described
// in spec.md ("Hufu 改善計畫：modernc SQLite + TEMP Analytics Tables + SQL
// Aggregation"). It is an ephemeral, in-memory analytics database: nothing it
// creates is ever written to disk, and it never mutates canonical storage
// (context.sqlite, execution-events.jsonl, event_store.jsonl).

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// sqliteAnalyticsSession pins a single SQLite connection so every TEMP
// table/view/index this package creates stays visible across the whole
// session. SQLite TEMP objects are connection-scoped: if database/sql were
// left free to hand out more than one connection from the pool, a query
// could land on a different connection than the one that created a TEMP
// table and fail with "no such table" (spec.md §5.1).
type sqliteAnalyticsSession struct {
	db   *sql.DB
	conn *sql.Conn

	// taskViewsReady is set once materializeTaskViews has populated
	// task_summary/task_skills for this session, so ensureTaskViews (used by
	// every query that reads them) only materializes once no matter how
	// many different run scopes are queried afterward.
	taskViewsReady bool
}

// openSQLiteAnalyticsSession opens a fresh in-memory SQLite database, pins a
// single connection to it, and creates the TEMP analytics schema. Every call
// to AnalyzeRecent opens (and closes) its own session — sessions are never
// shared across calls or across goroutines (spec.md §19: "不要使用 global
// analytics DB").
func openSQLiteAnalyticsSession(ctx context.Context) (*sqliteAnalyticsSession, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageOpen, fmt.Errorf("open analytics database: %w", err))
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, newAnalyticsError(AnalyticsStageOpen, fmt.Errorf("pin analytics connection: %w", err))
	}

	session := &sqliteAnalyticsSession{db: db, conn: conn}
	if err := session.createSchema(ctx); err != nil {
		_ = session.Close()
		return nil, newAnalyticsError(AnalyticsStageSchema, err)
	}
	return session, nil
}

// Close releases the pinned connection and then the database. TEMP objects
// vanish with the connection; nothing created by this session outlives the
// call. Safe to call on a session that failed to fully initialize.
func (s *sqliteAnalyticsSession) Close() error {
	if s == nil {
		return nil
	}
	var connErr, dbErr error
	if s.conn != nil {
		connErr = s.conn.Close()
	}
	if s.db != nil {
		dbErr = s.db.Close()
	}
	if connErr != nil {
		return fmt.Errorf("close analytics connection: %w", connErr)
	}
	if dbErr != nil {
		return fmt.Errorf("close analytics database: %w", dbErr)
	}
	return nil
}
