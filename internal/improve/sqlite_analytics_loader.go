package improve

// Streaming ingestion of execution-events.jsonl into the TEMP analytics
// schema (spec.md §8). Events are decoded and inserted one line at a time
// inside a single transaction — the file is never materialized into a
// []team.ExecutionEvent slice, so peak memory stays proportional to one
// event, not to the whole telemetry history (spec.md G4).

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// loadStats reports what happened during ingestion, for tests and
// diagnostics only — spec.md §8.1 explicitly keeps this out of Report.
type loadStats struct {
	LinesRead        int64
	RowsLoaded       int64
	MalformedRows    int64
	MissingRunIDRows int64
}

const insertExecutionEventSQL = `
INSERT INTO execution_events (
    event_seq, version, timestamp_raw, timestamp_unix_ns, run_id, team,
	 task_id, agent, attempt, status, model, task_type, team_revision,
	 skills_reported,
	 duration_ms, input_tokens, output_tokens, total_tokens, progress_tokens,
    outcome, stop_reason, acceptance_state, repair_attempts, phase,
    provider, failure_signature
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const insertExecutionEventSkillSQL = `
INSERT OR IGNORE INTO execution_event_skills (event_seq, run_id, task_id, skill)
VALUES (?, ?, ?, ?)`

// loadExecutionEvents streams path (execution-events.jsonl) into TEMP
// execution_events / execution_event_skills. It preserves readEvents'
// tolerant semantics exactly: a missing file is not an error, a malformed
// JSON line is skipped, and a line with an empty run_id is skipped
// (parity_test.go TestReadEvents_SkipsMalformedAndMissingRunID locks this
// for the legacy path).
func (s *sqliteAnalyticsSession) loadExecutionEvents(ctx context.Context, path string) (loadStats, error) {
	var stats loadStats
	if s.taskViewsReady {
		return stats, fmt.Errorf("cannot ingest execution events after task projections materialize")
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return stats, nil
		}
		return stats, fmt.Errorf("open execution events: %w", err)
	}
	defer func() { _ = f.Close() }()

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin execution events transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	insertEvent, err := tx.PrepareContext(ctx, insertExecutionEventSQL)
	if err != nil {
		return stats, fmt.Errorf("prepare execution event insert: %w", err)
	}
	defer func() { _ = insertEvent.Close() }()

	insertSkill, err := tx.PrepareContext(ctx, insertExecutionEventSkillSQL)
	if err != nil {
		return stats, fmt.Errorf("prepare execution event skill insert: %w", err)
	}
	defer func() { _ = insertSkill.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)

	var eventSeq int64
	for scanner.Scan() {
		stats.LinesRead++
		var event team.ExecutionEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			stats.MalformedRows++
			continue
		}
		if event.RunID == "" {
			stats.MissingRunIDRows++
			continue
		}
		eventSeq++
		if err := insertExecutionEventRow(ctx, insertEvent, insertSkill, eventSeq, event); err != nil {
			return stats, fmt.Errorf("insert execution event %d: %w", eventSeq, err)
		}
		stats.RowsLoaded++
	}
	if err := scanner.Err(); err != nil {
		return stats, fmt.Errorf("read execution events: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("commit execution events: %w", err)
	}
	committed = true
	return stats, nil
}

func insertExecutionEventRow(ctx context.Context, insertEvent, insertSkill *sql.Stmt, eventSeq int64, event team.ExecutionEvent) error {
	var timestampUnixNS any
	if ts, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
		timestampUnixNS = ts.UnixNano()
	}
	_, err := insertEvent.ExecContext(ctx,
		eventSeq, event.Version, event.Timestamp, timestampUnixNS, event.RunID, event.Team,
		event.TaskID, event.Agent, event.Attempt, event.Status, event.Model, event.TaskType, event.TeamRevision,
		len(event.Skills) > 0,
		event.DurationMS, event.Usage.InputTokens, event.Usage.OutputTokens, event.Usage.TotalTokens, event.Usage.ProgressTokens,
		string(event.Outcome), string(event.StopReason), string(event.AcceptanceState), event.RepairAttempts, string(event.Phase),
		event.Provider, event.FailureSignature,
	)
	if err != nil {
		return err
	}
	for _, skill := range dedupeNonEmpty(event.Skills) {
		if _, err := insertSkill.ExecContext(ctx, eventSeq, event.RunID, event.TaskID, skill); err != nil {
			return fmt.Errorf("insert skill %q: %w", skill, err)
		}
	}
	return nil
}

func dedupeNonEmpty(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
