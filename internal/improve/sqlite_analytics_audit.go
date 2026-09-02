package improve

// Streaming ingestion and SQL aggregation for audit-*.jsonl telemetry. The
// analytics representation deliberately contains only the four fields needed
// by collectAuditMetrics; arbitrary audit payload is decoded and discarded.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type auditLoadStats struct {
	FilesSeen            int64
	LinesRead            int64
	RowsLoaded           int64
	MalformedRows        int64
	InvalidTimestampRows int64
}

const insertAuditEventSQL = `
INSERT INTO audit_events (
    event_seq, timestamp_raw, timestamp_unix_ns, team, agent, event
) VALUES (?, ?, ?, ?, ?, ?)`

// loadAuditEvents streams all audit-*.jsonl files in filepath.Glob order into
// the session's TEMP audit_events table. Valid JSON rows are retained even
// when their timestamp, team, or event type cannot contribute to metrics;
// SQL applies those legacy filters later. Malformed JSON rows are skipped.
func (s *sqliteAnalyticsSession) loadAuditEvents(ctx context.Context, dir string) (auditLoadStats, error) {
	var stats auditLoadStats
	if s.taskViewsReady {
		return stats, fmt.Errorf("cannot ingest audit events after task projections materialize")
	}

	files, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if err != nil {
		return stats, fmt.Errorf("find audit events: %w", err)
	}
	if len(files) == 0 {
		return stats, nil
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin audit events transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	insertEvent, err := tx.PrepareContext(ctx, insertAuditEventSQL)
	if err != nil {
		return stats, fmt.Errorf("prepare audit event insert: %w", err)
	}
	defer func() { _ = insertEvent.Close() }()

	var eventSeq int64
	for _, filename := range files {
		stats.FilesSeen++
		f, err := os.Open(filename)
		if err != nil {
			// collectAuditMetrics skips files it cannot open; retain that
			// tolerant source semantics for audit telemetry.
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		for scanner.Scan() {
			stats.LinesRead++
			var event auditEvent
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				stats.MalformedRows++
				continue
			}
			eventSeq++
			var timestampUnixNS any
			if timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
				timestampUnixNS = timestamp.UnixNano()
			} else {
				stats.InvalidTimestampRows++
			}
			if _, err := insertEvent.ExecContext(ctx, eventSeq, event.Timestamp, timestampUnixNS, event.Team, event.Agent, event.Event); err != nil {
				_ = f.Close()
				return stats, fmt.Errorf("insert audit event %d: %w", eventSeq, err)
			}
			stats.RowsLoaded++
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return stats, fmt.Errorf("read audit events %q: %w", filename, err)
		}
		_ = f.Close()
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("commit audit events: %w", err)
	}
	committed = true
	return stats, nil
}

// sqlCollectAuditMetrics applies collectAuditMetrics' team, inclusive time,
// and event-type semantics using only the minimal TEMP audit projection.
func (s *sqliteAnalyticsSession) sqlCollectAuditMetrics(ctx context.Context, teamName string, start, end time.Time, metrics *Metrics) error {
	if metrics == nil {
		return fmt.Errorf("collect audit metrics: nil metrics")
	}

	const totalsQuery = `
SELECT
    COALESCE(SUM(CASE WHEN event = 'tool_call' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN event = 'tool_error' THEN 1 ELSE 0 END), 0)
FROM audit_events
WHERE team = ? AND timestamp_unix_ns >= ? AND timestamp_unix_ns <= ?`
	var toolCalls, toolErrors int
	if err := s.conn.QueryRowContext(ctx, totalsQuery, teamName, start.UnixNano(), end.UnixNano()).Scan(&toolCalls, &toolErrors); err != nil {
		return fmt.Errorf("query audit totals: %w", err)
	}

	const byAgentQuery = `
SELECT agent,
       SUM(CASE WHEN event = 'tool_call' THEN 1 ELSE 0 END),
       SUM(CASE WHEN event = 'tool_error' THEN 1 ELSE 0 END)
FROM audit_events
WHERE team = ? AND timestamp_unix_ns >= ? AND timestamp_unix_ns <= ?
  AND event IN ('tool_call', 'tool_error')
GROUP BY agent
ORDER BY agent ASC`
	rows, err := s.conn.QueryContext(ctx, byAgentQuery, teamName, start.UnixNano(), end.UnixNano())
	if err != nil {
		return fmt.Errorf("query audit metrics by agent: %w", err)
	}
	defer func() { _ = rows.Close() }()
	toolCallsByAgent := make(map[string]int)
	toolErrorsByAgent := make(map[string]int)
	for rows.Next() {
		var agent string
		var calls, errors int
		if err := rows.Scan(&agent, &calls, &errors); err != nil {
			return fmt.Errorf("scan audit metrics by agent: %w", err)
		}
		if calls > 0 {
			toolCallsByAgent[agent] = calls
		}
		if errors > 0 {
			toolErrorsByAgent[agent] = errors
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate audit metrics by agent: %w", err)
	}

	if metrics.ToolCallsByAgent == nil {
		metrics.ToolCallsByAgent = make(map[string]int)
	}
	if metrics.ToolErrorsByAgent == nil {
		metrics.ToolErrorsByAgent = make(map[string]int)
	}
	metrics.ToolCalls += toolCalls
	metrics.ToolErrors += toolErrors
	for agent, calls := range toolCallsByAgent {
		metrics.ToolCallsByAgent[agent] += calls
	}
	for agent, errors := range toolErrorsByAgent {
		metrics.ToolErrorsByAgent[agent] += errors
	}
	return nil
}
