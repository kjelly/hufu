package improve

// Streaming ingestion of the canonical event_store.jsonl memory events. The
// reader is deliberately owned by internal/team: it validates the complete
// durable hash chain without opening EventStore, which would create files or
// publish a cache. This loader keeps its TEMP writes transactional so a chain
// error after a valid prefix cannot leave partial analytics rows behind.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

type memoryLoadStats struct {
	RowsLoaded           int64
	EventsSkipped        int64
	MalformedPayloadRows int64
}

const insertMemoryEventSQL = `
INSERT INTO memory_events (
    event_seq, run_id, task_id, attempt, type, timestamp_raw,
    timestamp_unix_ns, retrieval_id, reason_code, token_count,
    disposition, signal, direction
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const (
	memoryRetrievedEvent       = "memory_retrieved"
	memoryUsageRecordedEvent   = "memory_usage_recorded"
	memoryOutcomeRecordedEvent = "memory_outcome_recorded"
)

// loadMemoryEvents streams the canonical event store into TEMP memory_events.
// Missing, corrupt, or otherwise unreadable canonical event stores are
// intentionally nonfatal, matching collectMemoryMetrics' behavior. SQL
// errors from the TEMP insert and commit are still returned to the caller.
func (s *sqliteAnalyticsSession) loadMemoryEvents(ctx context.Context, workspace string) (memoryLoadStats, error) {
	var stats memoryLoadStats
	if s.taskViewsReady {
		return stats, fmt.Errorf("cannot ingest memory events after task projections materialize")
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin memory events transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	insertEvent, err := tx.PrepareContext(ctx, insertMemoryEventSQL)
	if err != nil {
		return stats, fmt.Errorf("prepare memory event insert: %w", err)
	}
	defer func() { _ = insertEvent.Close() }()

	var eventSeq int64
	var insertErr error
	streamErr := team.StreamValidatedRunEvents(ctx, workspace, func(event team.RunEvent) error {
		if !isMemoryEventType(event.Type) {
			stats.EventsSkipped++
			return nil
		}

		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			stats.MalformedPayloadRows++
			return nil
		}

		eventSeq++
		var timestampUnixNS any
		if timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
			timestampUnixNS = timestamp.UnixNano()
		}
		_, insertErr = insertEvent.ExecContext(ctx,
			eventSeq, event.RunID, event.TaskID, event.Attempt, event.Type, event.Timestamp,
			timestampUnixNS, memoryPayloadString(payload, "retrieval_id"), memoryPayloadString(payload, "reason_code"),
			memoryPayloadInt(payload, "token_count"), memoryPayloadString(payload, "disposition"),
			memoryPayloadString(payload, "signal"), memoryPayloadString(payload, "direction"),
		)
		if insertErr != nil {
			return insertErr
		}
		stats.RowsLoaded++
		return nil
	})
	if insertErr != nil {
		return stats, fmt.Errorf("insert memory event: %w", insertErr)
	}
	if streamErr != nil {
		// The transaction is rolled back by the deferred cleanup. Canonical
		// event-store validation/read failures are nonfatal by design.
		return stats, nil
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("commit memory events: %w", err)
	}
	committed = true
	return stats, nil
}

func isMemoryEventType(eventType string) bool {
	switch eventType {
	case memoryRetrievedEvent, memoryUsageRecordedEvent, memoryOutcomeRecordedEvent:
		return true
	default:
		return false
	}
}

func memoryPayloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func memoryPayloadInt(payload map[string]any, key string) int64 {
	value, ok := payload[key].(float64)
	if !ok {
		return 0
	}
	return int64(value)
}
