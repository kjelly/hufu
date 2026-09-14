package improve

import (
	"context"
	"database/sql"
	"fmt"
)

var analyticsPragmaStatements []string

// configureAnalyticsSQLite applies only the benchmark-approved settings for
// the disposable in-memory analytics connection. Canonical databases never
// call this function.
func configureAnalyticsSQLite(ctx context.Context, conn *sql.Conn) error {
	return configureAnalyticsSQLiteFrom(ctx, conn, analyticsPragmaStatements)
}

func configureAnalyticsSQLiteFrom(ctx context.Context, conn *sql.Conn, statements []string) error {
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure analytics SQLite: %w", err)
		}
	}
	return nil
}
