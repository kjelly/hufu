package improve

// Memory event rows from the canonical event store; loadEventStore streams
// them in the same pass as the execution projection.

type memoryLoadStats struct {
	RowsLoaded           int64
	EventsSkipped        int64
	MalformedPayloadRows int64
}

const insertMemoryEventPrefix = `
INSERT INTO memory_events (
    event_seq, run_id, task_id, attempt, type, timestamp_raw,
	timestamp_unix_ns, retrieval_id, context_item_id, content_hash, policy_version, reason_code, token_count,
	disposition, signal, direction
) VALUES `

const memoryEventColumnCount = 16

const (
	memoryRetrievedEvent       = "memory_retrieved"
	memoryUsageRecordedEvent   = "memory_usage_recorded"
	memoryOutcomeRecordedEvent = "memory_outcome_recorded"
)

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
