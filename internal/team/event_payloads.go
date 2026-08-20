package team

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event payloads intentionally contain references and redacted summaries,
// never raw transcripts or canonical context content. The structs only model
// fields required to validate terminal/reducer-consumed records; legacy v1
// records continue through the compatibility path below.
type SessionMessageEventPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type TaskTransitionEventPayload struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type RunFinishedEventPayload struct {
	Outcome         RunOutcome        `json:"outcome"`
	GoalSatisfied   bool              `json:"goal_satisfied,omitempty"`
	GoalMode        GoalMode          `json:"goal_mode,omitempty"`
	Response        string            `json:"response,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	StopReason      StopReason        `json:"stop_reason,omitempty"`
	ExitCode        int               `json:"exit_code,omitempty"`
	UnresolvedTasks []TaskReference   `json:"unresolved_tasks,omitempty"`
	Acceptance      *AcceptanceResult `json:"acceptance,omitempty"`
	Stats           *RunStats         `json:"stats,omitempty"`
	Metrics         *RunMetrics       `json:"metrics,omitempty"`
}

// ValidateEventPayload validates an event after EventStore has filled its
// identity and redacted its payload, but before it becomes durable. Schema v1
// is deliberately accepted as a read/replay compatibility format. Unknown
// event types are accepted so a newer writer cannot make an older reader lose
// an otherwise valid hash chain.
func ValidateEventPayload(event RunEvent) error {
	if event.SchemaVersion <= 0 {
		return fmt.Errorf("event schema version must be positive")
	}
	if strings.TrimSpace(event.Type) == "" {
		return fmt.Errorf("event type is empty")
	}
	// Schema v1 intentionally allowed sparse events. Preserve those old
	// workspaces (and their hash chains); v2 is the current event-first
	// contract and requires complete durable identity.
	if event.SchemaVersion >= eventStoreSchemaVersion && (strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.RunID) == "" || strings.TrimSpace(event.SessionID) == "" || strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.Timestamp) == "") {
		return fmt.Errorf("event identity is incomplete")
	}
	if len(event.Payload) > 0 && !json.Valid(event.Payload) {
		return fmt.Errorf("event %q payload is not valid JSON", event.Type)
	}
	if IsTerminalEvent(event.Type) && IsEmptyPayload(event.Payload) {
		return fmt.Errorf("terminal event %q has empty payload", event.Type)
	}
	if event.SchemaVersion == eventStoreLegacySchemaVersion {
		return nil
	}

	switch EventType(event.Type) {
	case EventUserMessageAdded, EventAssistantMessageAdded:
		var payload SessionMessageEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		if strings.TrimSpace(payload.Content) == "" {
			return fmt.Errorf("%s payload has empty content", event.Type)
		}
	case EventTaskCreated, EventTaskPlanned, EventTaskStarted, EventTaskVerifying, EventTaskPaused, EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskProtocolIncomplete, EventTaskCancelled, EventTaskRemoved, EventTaskResolution:
		if strings.TrimSpace(event.TaskID) == "" {
			return fmt.Errorf("%s event has empty task id", event.Type)
		}
		var payload TaskTransitionEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		// Old task_started records did not always carry id/status. Keep schema
		// v1 replayable while enforcing the complete v2 transition contract.
		if strings.TrimSpace(payload.ID) == "" || strings.TrimSpace(payload.Status) == "" {
			return fmt.Errorf("%s payload lacks task transition identity", event.Type)
		}
	case EventRunFinished:
		var payload RunFinishedEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_finished payload: %w", err)
		}
		if payload.Outcome == "" {
			return fmt.Errorf("run_finished payload lacks outcome")
		}
	}
	return nil
}
