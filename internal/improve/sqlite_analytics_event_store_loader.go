package improve

// Streaming ingestion of the canonical event store (event_store.jsonl) into
// the TEMP analytics schema. One validated pass feeds both tables: memory
// events stream straight into memory_events, and every event feeds
// team.ExecutionEventProjector, whose rows become execution_events. The
// projector holds its rows until the stream ends because a later transition
// can still attribute receipt usage to an earlier row. The reader is owned by
// internal/team: it validates the whole hash chain without opening an
// EventStore, which would create files or publish a cache.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

type eventStoreLoadStats struct {
	EventsRead int64
	Execution  loadStats
	Memory     memoryLoadStats
}

// loadEventStore streams workspace's event store once. withMemory also loads
// memory_events; LatestTeam needs execution rows only. A missing event store
// loads nothing. A store that fails validation is an error: no execution
// data can be trusted from it. Errors carry their analytics stage.
func (s *sqliteAnalyticsSession) loadEventStore(ctx context.Context, workspace string, withMemory bool) (eventStoreLoadStats, error) {
	var stats eventStoreLoadStats
	if s.selectedRunsReady {
		return stats, newAnalyticsError(AnalyticsStageLoadExecution, fmt.Errorf("cannot ingest execution events after run selection"))
	}
	if withMemory && s.taskViewsReady {
		return stats, newAnalyticsError(AnalyticsStageLoadMemory, fmt.Errorf("cannot ingest memory events after task projections materialize"))
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return stats, newAnalyticsError(AnalyticsStageLoadExecution, fmt.Errorf("begin event store transaction: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var insertMemory *sqliteBatchInserter
	if withMemory {
		batchRows, err := s.configuredBatchRows(ctx, memoryEventColumnCount, s.batchConfig.Memory)
		if err != nil {
			return stats, newAnalyticsError(AnalyticsStageLoadMemory, fmt.Errorf("configure memory event batch: %w", err))
		}
		insertMemory, err = newSQLiteBatchInserter(ctx, tx, insertMemoryEventPrefix, memoryEventColumnCount, batchRows)
		if err != nil {
			return stats, newAnalyticsError(AnalyticsStageLoadMemory, fmt.Errorf("create memory event batch: %w", err))
		}
		defer func() { _ = insertMemory.Close() }()
	}

	projector := team.NewExecutionEventProjector()
	var memorySeq int64
	var memoryErr error
	streamErr := team.StreamValidatedRunEvents(ctx, workspace, func(event team.RunEvent) error {
		stats.EventsRead++
		projector.Add(event)
		if insertMemory == nil {
			return nil
		}
		if !isMemoryEventType(event.Type) {
			stats.Memory.EventsSkipped++
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			stats.Memory.MalformedPayloadRows++
			return nil
		}
		memorySeq++
		if memoryErr = insertMemoryEventRow(insertMemory, memorySeq, event, payload); memoryErr != nil {
			return memoryErr
		}
		stats.Memory.RowsLoaded++
		return nil
	})
	if memoryErr != nil {
		return stats, newAnalyticsError(AnalyticsStageLoadMemory, fmt.Errorf("insert memory event: %w", memoryErr))
	}
	if streamErr != nil {
		return stats, newAnalyticsError(AnalyticsStageLoadExecution, fmt.Errorf("read event store: %w", streamErr))
	}
	if insertMemory != nil {
		if err := insertMemory.Flush(); err != nil {
			return stats, newAnalyticsError(AnalyticsStageLoadMemory, fmt.Errorf("flush memory events: %w", err))
		}
	}
	if err := s.insertExecutionRows(ctx, tx, projector.Rows(), &stats.Execution); err != nil {
		return stats, newAnalyticsError(AnalyticsStageLoadExecution, err)
	}
	if err := tx.Commit(); err != nil {
		return stats, newAnalyticsError(AnalyticsStageLoadExecution, fmt.Errorf("commit event store rows: %w", err))
	}
	committed = true
	return stats, nil
}

func insertMemoryEventRow(insert *sqliteBatchInserter, eventSeq int64, event team.RunEvent, payload map[string]any) error {
	var timestampUnixNS any
	if timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
		timestampUnixNS = timestamp.UnixNano()
	}
	return insert.Add(
		eventSeq, event.RunID, event.TaskID, event.Attempt, event.Type, event.Timestamp,
		timestampUnixNS, memoryPayloadString(payload, "retrieval_id"), memoryPayloadString(payload, "context_item_id"),
		memoryPayloadString(payload, "content_hash"), memoryPayloadString(payload, "policy_version"), memoryPayloadString(payload, "reason_code"),
		memoryPayloadInt(payload, "token_count"), memoryPayloadString(payload, "disposition"),
		memoryPayloadString(payload, "signal"), memoryPayloadString(payload, "direction"),
	)
}
