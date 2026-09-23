package context

import "context"

// Schema versions that readers must check before touching columns or tables a
// not-yet-upgraded store does not have. Writable opens always migrate to the
// latest version; OpenSQLiteReadOnly never migrates.
const (
	schemaVersionPromotionEditTracking = 10
	schemaVersionPairJudgments         = 11
)

// latestSchemaVersion is the newest migration this build knows.
func latestSchemaVersion() int {
	latest := 0
	for _, m := range migrations {
		latest = max(latest, m.version)
	}
	return latest
}

// readAppliedSchemaVersion returns the newest recorded migration. A store
// without schema_migrations reports 0 so readers degrade instead of failing.
func readAppliedSchemaVersion(ctx context.Context, r *SQLiteRepository) int {
	var version int
	if err := r.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return 0
	}
	return version
}

// schemaAtLeast reports whether the opened store has migration version applied.
func (r *SQLiteRepository) schemaAtLeast(version int) bool {
	return r.schemaVersion >= version
}
