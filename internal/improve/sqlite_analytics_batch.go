package improve

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

const (
	fallbackSQLiteVariableLimit = 999
	defaultBatchCandidate       = 1
)

type analyticsBatchConfig struct {
	Execution int
	Skills    int
	Audit     int
	Memory    int
}

var defaultAnalyticsBatchConfig = analyticsBatchConfig{
	Execution: defaultBatchCandidate,
	Skills:    defaultBatchCandidate,
	Audit:     defaultBatchCandidate,
	Memory:    defaultBatchCandidate,
}

func sqliteMaxVariableNumber(ctx context.Context, conn *sql.Conn) (int, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA compile_options")
	if err != nil {
		return 0, fmt.Errorf("read SQLite compile options: %w", err)
	}
	defer func() { _ = rows.Close() }()
	limit := fallbackSQLiteVariableLimit
	for rows.Next() {
		var option string
		if err := rows.Scan(&option); err != nil {
			return 0, fmt.Errorf("scan SQLite compile option: %w", err)
		}
		const prefix = "MAX_VARIABLE_NUMBER="
		if !strings.HasPrefix(option, prefix) {
			continue
		}
		parsed, err := strconv.Atoi(strings.TrimPrefix(option, prefix))
		if err != nil || parsed < 1 {
			return 0, fmt.Errorf("parse SQLite %s compile option", option)
		}
		limit = parsed
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate SQLite compile options: %w", err)
	}
	return limit, nil
}

func analyticsBatchRows(maxVariableNumber, columnCount, candidate int) (int, error) {
	if maxVariableNumber < 1 {
		return 0, fmt.Errorf("SQLite variable limit must be positive")
	}
	if columnCount < 1 {
		return 0, fmt.Errorf("batch column count must be positive")
	}
	if candidate < 1 {
		return 0, fmt.Errorf("batch candidate must be positive")
	}
	maxRows := maxVariableNumber / columnCount
	if maxRows < 1 {
		return 0, fmt.Errorf("SQLite variable limit %d cannot fit %d columns", maxVariableNumber, columnCount)
	}
	return min(candidate, maxRows), nil
}

func (s *sqliteAnalyticsSession) configuredBatchRows(ctx context.Context, columnCount, candidate int) (int, error) {
	// The benchmark gate currently retains the single-row path. Avoid paying
	// for compile-option discovery unless an experiment explicitly enables a
	// multi-row candidate.
	if candidate == 1 {
		return analyticsBatchRows(fallbackSQLiteVariableLimit, columnCount, candidate)
	}
	if s.variableMax == 0 {
		limit, err := sqliteMaxVariableNumber(ctx, s.conn)
		if err != nil {
			return 0, err
		}
		s.variableMax = limit
	}
	return analyticsBatchRows(s.variableMax, columnCount, candidate)
}

type sqliteBatchInserter struct {
	ctx         context.Context
	tx          *sql.Tx
	prefix      string
	rowTemplate string
	columnCount int
	batchRows   int
	queuedRows  int
	arguments   []any
	fullBatch   *sql.Stmt
}

func newSQLiteBatchInserter(ctx context.Context, tx *sql.Tx, prefix string, columnCount, batchRows int) (*sqliteBatchInserter, error) {
	if tx == nil {
		return nil, fmt.Errorf("batch inserter requires a transaction")
	}
	if columnCount < 1 || batchRows < 1 {
		return nil, fmt.Errorf("batch inserter requires positive columns and rows")
	}
	placeholders := make([]string, columnCount)
	for i := range placeholders {
		placeholders[i] = "?"
	}
	rowTemplate := "(" + strings.Join(placeholders, ",") + ")"
	rows := make([]string, batchRows)
	for i := range rows {
		rows[i] = rowTemplate
	}
	fullBatch, err := tx.PrepareContext(ctx, prefix+strings.Join(rows, ","))
	if err != nil {
		return nil, fmt.Errorf("prepare batch insert: %w", err)
	}
	return &sqliteBatchInserter{
		ctx: ctx, tx: tx, prefix: prefix, rowTemplate: rowTemplate,
		columnCount: columnCount, batchRows: batchRows,
		arguments: make([]any, 0, columnCount*batchRows), fullBatch: fullBatch,
	}, nil
}

func (inserter *sqliteBatchInserter) Add(arguments ...any) error {
	if len(arguments) != inserter.columnCount {
		return fmt.Errorf("batch row has %d values, want %d", len(arguments), inserter.columnCount)
	}
	inserter.arguments = append(inserter.arguments, arguments...)
	inserter.queuedRows++
	if inserter.queuedRows == inserter.batchRows {
		return inserter.Flush()
	}
	return nil
}

func (inserter *sqliteBatchInserter) Flush() error {
	if inserter.queuedRows == 0 {
		return nil
	}
	if inserter.queuedRows == inserter.batchRows {
		if _, err := inserter.fullBatch.ExecContext(inserter.ctx, inserter.arguments...); err != nil {
			return err
		}
	} else {
		rows := make([]string, inserter.queuedRows)
		for i := range rows {
			rows[i] = inserter.rowTemplate
		}
		if _, err := inserter.tx.ExecContext(inserter.ctx, inserter.prefix+strings.Join(rows, ","), inserter.arguments...); err != nil {
			return err
		}
	}
	inserter.arguments = inserter.arguments[:0]
	inserter.queuedRows = 0
	return nil
}

func (inserter *sqliteBatchInserter) Close() error {
	if inserter == nil || inserter.fullBatch == nil {
		return nil
	}
	return inserter.fullBatch.Close()
}
