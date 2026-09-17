package context

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	backupPages       = 128
	backupRetryDelay  = 50 * time.Millisecond
	backupRetryWindow = 5 * time.Second
)

type sqliteBackuper interface {
	NewBackup(string) (*modernsqlite.Backup, error)
}

// SQLiteSnapshotFacts are content-free facts used to verify a migrated store.
type SQLiteSnapshotFacts struct {
	ProjectIDs  []string         `json:"project_ids"`
	TableRows   map[string]int64 `json:"table_rows"`
	Revision    int64            `json:"revision"`
	SchemaLevel int64            `json:"schema_level"`
}

// BackupSQLiteReadOnly creates a transactionally consistent SQLite snapshot
// using the driver's online-backup API. It never checkpoints, migrates, or
// otherwise writes the source database.
func BackupSQLiteReadOnly(ctx context.Context, source, destination string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("backup SQLite destination already exists: %q", destination)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect SQLite backup destination: %w", err)
	}
	dsn, err := readOnlySQLiteDSN(source)
	if err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open SQLite backup source: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open SQLite backup connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(backupRetryWindow)
	err = conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(sqliteBackuper)
		if !ok {
			return fmt.Errorf("SQLite driver does not support online backup")
		}
		backup, backupErr := backuper.NewBackup(destination)
		if backupErr != nil {
			return backupErr
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			more, stepErr := backup.Step(backupPages)
			if stepErr == nil {
				if !more {
					finished = true
					return backup.Finish()
				}
				continue
			}
			if !sqliteBusyOrLocked(stepErr) || time.Now().Add(backupRetryDelay).After(deadline) {
				return stepErr
			}
			timer := time.NewTimer(backupRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	})
	if err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("online SQLite backup: %w", err)
	}
	return nil
}

func sqliteBusyOrLocked(err error) bool {
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

// InspectSQLiteSnapshot validates quick_check and committed migration
// checksums, then returns canonical row counts, revision, and legacy scope IDs.
func InspectSQLiteSnapshot(ctx context.Context, path string) (SQLiteSnapshotFacts, error) {
	dsn, err := readOnlySQLiteDSN(path)
	if err != nil {
		return SQLiteSnapshotFacts{}, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return SQLiteSnapshotFacts{}, err
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	var quickCheck string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil || quickCheck != "ok" {
		if err == nil {
			err = fmt.Errorf("quick_check returned %q", quickCheck)
		}
		return SQLiteSnapshotFacts{}, fmt.Errorf("context quick check failed: %w", err)
	}
	if err = verifySnapshotMigrations(ctx, db); err != nil {
		return SQLiteSnapshotFacts{}, err
	}
	tables, err := applicationTables(ctx, db)
	if err != nil {
		return SQLiteSnapshotFacts{}, err
	}
	facts := SQLiteSnapshotFacts{TableRows: make(map[string]int64), ProjectIDs: []string{}}
	projectIDs := make(map[string]struct{})
	for _, table := range tables {
		quoted := quoteSQLiteIdentifier(table)
		var count int64
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoted).Scan(&count); err != nil {
			return SQLiteSnapshotFacts{}, fmt.Errorf("count context table %s: %w", table, err)
		}
		facts.TableRows[table] = count
		hasProjectID, columnErr := tableHasColumn(ctx, db, table, "project_id")
		if columnErr != nil {
			return SQLiteSnapshotFacts{}, columnErr
		}
		if hasProjectID {
			rows, queryErr := db.QueryContext(ctx, "SELECT DISTINCT project_id FROM "+quoted+" WHERE project_id IS NOT NULL AND trim(project_id)<>''")
			if queryErr != nil {
				return SQLiteSnapshotFacts{}, fmt.Errorf("inspect project_id in %s: %w", table, queryErr)
			}
			for rows.Next() {
				var value string
				if scanErr := rows.Scan(&value); scanErr != nil {
					_ = rows.Close()
					return SQLiteSnapshotFacts{}, scanErr
				}
				projectIDs[value] = struct{}{}
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				_ = rows.Close()
				return SQLiteSnapshotFacts{}, rowsErr
			}
			_ = rows.Close()
		}
	}
	if _, ok := facts.TableRows["context_events"]; ok {
		rows, queryErr := db.QueryContext(ctx, "SELECT scope_json FROM context_events WHERE scope_json IS NOT NULL AND trim(scope_json)<>''")
		if queryErr != nil {
			return SQLiteSnapshotFacts{}, queryErr
		}
		for rows.Next() {
			var raw string
			if scanErr := rows.Scan(&raw); scanErr != nil {
				_ = rows.Close()
				return SQLiteSnapshotFacts{}, scanErr
			}
			var scope struct {
				ProjectID string `json:"project_id"`
			}
			if json.Unmarshal([]byte(raw), &scope) == nil && strings.TrimSpace(scope.ProjectID) != "" {
				projectIDs[scope.ProjectID] = struct{}{}
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return SQLiteSnapshotFacts{}, rowsErr
		}
		_ = rows.Close()
		if err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM context_events").Scan(&facts.Revision); err != nil {
			return SQLiteSnapshotFacts{}, err
		}
	}
	if err = db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&facts.SchemaLevel); err != nil {
		return SQLiteSnapshotFacts{}, err
	}
	for value := range projectIDs {
		facts.ProjectIDs = append(facts.ProjectIDs, value)
	}
	sort.Strings(facts.ProjectIDs)
	return facts, nil
}

func readOnlySQLiteDSN(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect SQLite source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("SQLite source %q is not a regular file", path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	uri := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := uri.Query()
	query.Set("mode", "ro")
	if _, statErr := os.Stat(path + "-wal"); errors.Is(statErr, os.ErrNotExist) {
		query.Set("immutable", "1")
	}
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	uri.RawQuery = query.Encode()
	return uri.String(), nil
}

func verifySnapshotMigrations(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT version, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("migration checksum verification: %w", err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var version int
		var checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			return err
		}
		if version != seen+1 || version > len(migrations) || checksum != migrationChecksum(migrations[version-1].sql) {
			return fmt.Errorf("migration checksum mismatch at version %d", version)
		}
		seen++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if seen == 0 {
		return errors.New("migration checksum mismatch: no committed migrations")
	}
	return nil
}

func applicationTables(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'schema_migrations' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func tableHasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+quoteSQLiteIdentifier(table)+")")
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err = rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func quoteSQLiteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
