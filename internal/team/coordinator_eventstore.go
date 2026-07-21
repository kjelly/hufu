package team

import (
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"time"
)

// RecordSessionUserMessage adds a user message to SessionData and dual-writes a user_message_added event to EventStore if available.
func RecordSessionUserMessage(session *SessionData, es *EventStore, content string) {
	if session == nil {
		return
	}
	session.AddEntry("user", content)
	if es != nil {
		payload, _ := json.Marshal(map[string]string{
			"role":    "user",
			"content": content,
		})
		_ = es.Append(RunEvent{
			Type:    "user_message_added",
			Actor:   "user",
			Payload: payload,
		})
	}
}

// RecordSessionAssistantMessage adds an assistant message to SessionData and dual-writes an assistant_message_added event to EventStore if available.
func RecordSessionAssistantMessage(session *SessionData, es *EventStore, content string) {
	if session == nil {
		return
	}
	session.AddEntry("assistant", content)
	if es != nil {
		payload, _ := json.Marshal(map[string]string{
			"role":    "assistant",
			"content": content,
		})
		_ = es.Append(RunEvent{
			Type:    "assistant_message_added",
			Actor:   "assistant",
			Payload: payload,
		})
	}
}

// initEventStore initializes the EventStore on Coordinator.
func (c *Coordinator) initEventStore() {
	if c.session == nil || c.session.Workspace == "" {
		return
	}
	runID := c.executionRunID
	if runID == "" {
		runID = fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	sessionID := filepath.Base(c.session.Workspace)
	es, err := NewEventStore(c.session.Workspace, runID, sessionID)
	if err != nil {
		log.Printf("warning: init event store failed: %v", err)
		return
	}
	c.eventStore = es
}

// emitEvent logs a RunEvent to the coordinator's eventStore if initialized.
func (c *Coordinator) emitEvent(eventType, actor, taskID string, payload map[string]interface{}) {
	if c == nil || c.eventStore == nil {
		return
	}
	var rawPayload json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err == nil {
			rawPayload = data
		}
	}
	_ = c.eventStore.Append(RunEvent{
		Type:    eventType,
		Actor:   actor,
		TaskID:  taskID,
		Payload: rawPayload,
	})
}

func (c *Coordinator) emitTaskEventsFromCheckpoint(tasks []*TodoItem) {
	if c == nil || c.eventStore == nil {
		return
	}
	for _, item := range tasks {
		if item == nil {
			continue
		}
		var eventType string
		switch item.Status {
		case TaskPending:
			eventType = "task_created"
		case TaskInProgress:
			eventType = "task_started"
		case TaskDone:
			eventType = "task_completed"
		case TaskError:
			eventType = "task_failed"
		case TaskSkipped, TaskBlocked:
			eventType = "task_blocked"
		}
		if eventType != "" {
			c.emitEvent(eventType, item.Agent, item.ID, map[string]interface{}{
				"id":         item.ID,
				"desc":       item.Desc,
				"status":     string(item.Status),
				"output":     item.Output,
				"agent":      item.Agent,
				"depends_on": item.DependsOn,
			})
		}
	}
}

// EventStore returns the active EventStore for the coordinator (may be nil).
func (c *Coordinator) EventStore() *EventStore {
	if c == nil {
		return nil
	}
	return c.eventStore
}
