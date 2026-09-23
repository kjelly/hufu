// Package sqlmigrate applies ordered, checksum-verified SQLite schema
// migrations. Every applied migration is recorded in schema_migrations with a
// SHA-256 of its SQL; a renamed, unknown, or edited migration fails closed
// instead of silently diverging from the schema the code expects.
package sqlmigrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// Migration is one forward-only schema step. Versions must be unique and
// strictly increasing; SQL is executed inside a single transaction together
// with its schema_migrations record.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Checksum returns the recorded identity of a migration statement.
func Checksum(statement string) string {
	sum := sha256.Sum256([]byte(statement))
	return hex.EncodeToString(sum[:])
}

// Validate rejects a migration list that cannot be applied deterministically.
func Validate(migrations []Migration) error {
	previous := 0
	for _, migration := range migrations {
		if migration.Version <= previous {
			return fmt.Errorf("migration versions must be unique and increasing: %d follows %d", migration.Version, previous)
		}
		if migration.Name == "" || migration.SQL == "" {
			return fmt.Errorf("migration %d must have a name and SQL", migration.Version)
		}
		previous = migration.Version
	}
	return nil
}

// Apply creates schema_migrations when missing, verifies every already
// applied row against migrations, applies the missing ones in order, and
// verifies the final state. subject names the database in error messages
// (for example "registry").
func Apply(ctx context.Context, db *sql.DB, subject string, migrations []Migration, now func() time.Time) error {
	if err := Validate(migrations); err != nil {
		return fmt.Errorf("%s migrations: %w", subject, err)
	}
	if now == nil {
		now = time.Now
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
version INTEGER PRIMARY KEY,
name TEXT NOT NULL,
applied_at INTEGER NOT NULL,
checksum TEXT NOT NULL
)`); err != nil {
		return fmt.Errorf("create migration registry: %w", err)
	}

	applied := make(map[int]string)
	rows, err := db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("read %s migrations: %w", subject, err)
	}
	for rows.Next() {
		var version int
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan %s migration: %w", subject, err)
		}
		migration, ok := migrationByVersion(migrations, version)
		if !ok || migration.Name != name {
			_ = rows.Close()
			return fmt.Errorf("unknown or renamed %s migration version %d (%s)", subject, version, name)
		}
		applied[version] = checksum
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read %s migrations: %w", subject, err)
	}
	if err = rows.Close(); err != nil {
		return fmt.Errorf("close %s migrations query: %w", subject, err)
	}

	for _, migration := range migrations {
		checksum := Checksum(migration.SQL)
		if existing, ok := applied[migration.Version]; ok {
			if existing != checksum {
				return fmt.Errorf("%s migration checksum mismatch for version %d (%s)", subject, migration.Version, migration.Name)
			}
			continue
		}
		if err = applyOne(ctx, db, subject, migration, checksum, now); err != nil {
			// A concurrent opener may have applied the same migration first;
			// the schema is acceptable exactly when it now verifies.
			if verifyErr := Verify(ctx, db, subject, migrations); verifyErr == nil {
				continue
			}
			return err
		}
	}
	return Verify(ctx, db, subject, migrations)
}

// Verify checks that schema_migrations records exactly migrations, with
// matching names and checksums.
func Verify(ctx context.Context, db *sql.DB, subject string, migrations []Migration) error {
	rows, err := db.QueryContext(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("verify %s migrations: %w", subject, err)
	}
	defer func() { _ = rows.Close() }()
	known := make(map[int]Migration, len(migrations))
	for _, migration := range migrations {
		known[migration.Version] = migration
	}
	seen := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err = rows.Scan(&version, &name, &checksum); err != nil {
			return fmt.Errorf("verify %s migration row: %w", subject, err)
		}
		migration, ok := known[version]
		if !ok || migration.Name != name || Checksum(migration.SQL) != checksum {
			return fmt.Errorf("%s migration checksum mismatch for version %d (%s)", subject, version, name)
		}
		seen++
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("verify %s migrations: %w", subject, err)
	}
	if seen != len(migrations) {
		return fmt.Errorf("%s schema is incomplete: found %d of %d migrations", subject, seen, len(migrations))
	}
	return nil
}

func migrationByVersion(migrations []Migration, version int) (Migration, bool) {
	for _, migration := range migrations {
		if migration.Version == version {
			return migration, true
		}
	}
	return Migration{}, false
}

func applyOne(ctx context.Context, db *sql.DB, subject string, migration Migration, checksum string, now func() time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin %s migration %d: %w", subject, migration.Version, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, migration.SQL); err != nil {
		return fmt.Errorf("apply %s migration %d (%s): %w", subject, migration.Version, migration.Name, err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,applied_at,checksum) VALUES(?,?,?,?)", migration.Version, migration.Name, now().UnixMilli(), checksum); err != nil {
		return fmt.Errorf("record %s migration %d: %w", subject, migration.Version, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit %s migration %d: %w", subject, migration.Version, err)
	}
	return nil
}
