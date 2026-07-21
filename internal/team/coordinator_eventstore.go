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
		if err := es.Append(RunEvent{
			Type:    "user_message_added",
			Actor:   "user",
			Payload: payload,
		}); err != nil {
			log.Printf("warning: dual-write user_message_added event failed: %v", err)
		}
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
		if err := es.Append(RunEvent{
			Type:    "assistant_message_added",
			Actor:   "assistant",
			Payload: payload,
		}); err != nil {
			log.Printf("warning: dual-write assistant_message_added event failed: %v", err)
		}
	}
}

func (c *Coordinator) addSessionUserMessage(content string) {
	if c == nil {
		return
	}
	RecordSessionUserMessage(c.sessionData, c.eventStore, content)
}

func (c *Coordinator) addSessionAssistantMessage(content string) {
	if c == nil {
		return
	}
	RecordSessionAssistantMessage(c.sessionData, c.eventStore, content)
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
	if err := c.eventStore.Append(RunEvent{
		Type:    eventType,
		Actor:   actor,
		TaskID:  taskID,
		Payload: rawPayload,
	}); err != nil {
		log.Printf("warning: dual-write event emit failed for type %s: %v", eventType, err)
		c.dualWriteFailures.Add(1)
	}
}

func (c *Coordinator) emitTaskEventsFromCheckpoint(tasks []*TodoItem) {
	if c == nil || c.eventStore == nil {
		return
	}
	c.mu.Lock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	c.mu.Unlock()

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
		case TaskSkipped:
			eventType = "task_skipped"
		case TaskBlocked:
			eventType = "task_blocked"
		}
		if eventType == "" {
			continue
		}

		transitionKey := fmt.Sprintf("%s:%s:%d", item.ID, item.Status, item.Retries)
		c.mu.Lock()
		alreadyEmitted := c.emittedTaskTransitions[transitionKey]
		if !alreadyEmitted {
			c.emittedTaskTransitions[transitionKey] = true
		}
		c.mu.Unlock()

		if alreadyEmitted {
			continue
		}

		var rawPayload json.RawMessage
		data, err := json.Marshal(map[string]interface{}{
			"id":         item.ID,
			"desc":       item.Desc,
			"status":     string(item.Status),
			"output":     item.Output,
			"agent":      item.Agent,
			"depends_on": item.DependsOn,
		})
		if err == nil {
			rawPayload = data
		}

		if err := c.eventStore.Append(RunEvent{
			Type:           eventType,
			Actor:          item.Agent,
			TaskID:         item.ID,
			IdempotencyKey: transitionKey,
			Payload:        rawPayload,
		}); err != nil {
			log.Printf("warning: dual-write task event emit failed for %s (%s): %v", item.ID, eventType, err)
			c.dualWriteFailures.Add(1)
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

// DualWriteFailures returns the count of failed dual-write event store appends.
func (c *Coordinator) DualWriteFailures() int64 {
	if c == nil {
		return 0
	}
	return c.dualWriteFailures.Load()
}
