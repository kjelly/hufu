package improve

// Insertion of projected execution events into the TEMP analytics schema
// (spec.md §8). Rows come from team.ExecutionEventProjector over the
// canonical event store; see loadEventStore.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// loadStats reports what happened during ingestion, for tests and
// diagnostics only — spec.md §8.1 explicitly keeps this out of Report.
type loadStats struct {
	RowsLoaded       int64
	MissingRunIDRows int64
}

const insertExecutionEventPrefix = `
INSERT INTO execution_events (
    event_seq, version, timestamp_raw, timestamp_unix_ns, run_id, team,
	 task_id, agent, attempt, status, model, task_type, team_revision,
	 skills_reported,
	 duration_ms, input_tokens, output_tokens, total_tokens, progress_tokens,
	 cache_read_tokens, cache_creation_tokens,
    outcome, stop_reason, acceptance_state, repair_attempts, phase,
    provider, failure_signature
) VALUES `

const insertExecutionEventSkillPrefix = `
INSERT OR IGNORE INTO execution_event_skills (event_seq, run_id, task_id, skill)
VALUES `

const (
	executionEventColumnCount = 28
	executionSkillColumnCount = 4
)

// insertExecutionRows inserts projected rows into TEMP execution_events /
// execution_event_skills in projection order. A row without a run ID cannot
// be attributed to a run and is skipped.
func (s *sqliteAnalyticsSession) insertExecutionRows(ctx context.Context, tx *sql.Tx, rows []team.ExecutionEvent, stats *loadStats) error {
	eventBatchRows, err := s.configuredBatchRows(ctx, executionEventColumnCount, s.batchConfig.Execution)
	if err != nil {
		return fmt.Errorf("configure execution event batch: %w", err)
	}
	skillBatchRows, err := s.configuredBatchRows(ctx, executionSkillColumnCount, s.batchConfig.Skills)
	if err != nil {
		return fmt.Errorf("configure execution skill batch: %w", err)
	}
	insertEvent, err := newSQLiteBatchInserter(ctx, tx, insertExecutionEventPrefix, executionEventColumnCount, eventBatchRows)
	if err != nil {
		return fmt.Errorf("create execution event batch: %w", err)
	}
	defer func() { _ = insertEvent.Close() }()
	insertSkill, err := newSQLiteBatchInserter(ctx, tx, insertExecutionEventSkillPrefix, executionSkillColumnCount, skillBatchRows)
	if err != nil {
		return fmt.Errorf("create execution skill batch: %w", err)
	}
	defer func() { _ = insertSkill.Close() }()

	// Continue after rows already loaded so separate loads keep a single
	// insertion order.
	var eventSeq int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(event_seq), 0) FROM execution_events").Scan(&eventSeq); err != nil {
		return fmt.Errorf("read execution event sequence: %w", err)
	}
	for _, event := range rows {
		if event.RunID == "" {
			stats.MissingRunIDRows++
			continue
		}
		eventSeq++
		if err := insertExecutionEventRow(insertEvent, insertSkill, eventSeq, event); err != nil {
			return fmt.Errorf("insert execution event %d: %w", eventSeq, err)
		}
		stats.RowsLoaded++
	}
	if err := insertEvent.Flush(); err != nil {
		return fmt.Errorf("flush execution events: %w", err)
	}
	if err := insertSkill.Flush(); err != nil {
		return fmt.Errorf("flush execution skills: %w", err)
	}
	return nil
}

func insertExecutionEventRow(insertEvent, insertSkill *sqliteBatchInserter, eventSeq int64, event team.ExecutionEvent) error {
	var timestampUnixNS any
	if ts, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
		timestampUnixNS = ts.UnixNano()
	}
	err := insertEvent.Add(
		eventSeq, event.Version, event.Timestamp, timestampUnixNS, event.RunID, event.Team,
		event.TaskID, event.Agent, event.Attempt, event.Status, event.Model, event.TaskType, event.TeamRevision,
		len(event.Skills) > 0,
		event.DurationMS, event.Usage.InputTokens, event.Usage.OutputTokens, event.Usage.TotalTokens, event.Usage.ProgressTokens,
		event.Usage.CacheReadTokens, event.Usage.CacheCreationTokens,
		string(event.Outcome), string(event.StopReason), string(event.AcceptanceState), event.RepairAttempts, string(event.Phase),
		event.Provider, event.FailureSignature,
	)
	if err != nil {
		return err
	}
	for _, skill := range dedupeNonEmpty(event.Skills) {
		if err := insertSkill.Add(eventSeq, event.RunID, event.TaskID, skill); err != nil {
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
