package improve

import (
	"context"
	"database/sql"
)

// analyticsQueryExecutor is the package-private SQL seam used by analytics
// query stages. Tests wrap it to prove aggregation query counts do not grow
// with the number of selected runs.
type analyticsQueryExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
