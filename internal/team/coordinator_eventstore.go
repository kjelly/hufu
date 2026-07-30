package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
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
			"content": utils.RedactSecrets(content),
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
			"content": utils.RedactSecrets(content),
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
	// Bind the store to the active session branch (if any) so events written
	// during this run are collected into that branch's lineage (§8).
	if st, err := LoadSessionTree(c.session.Workspace); err == nil && st.ActiveBranch != "" {
		es.SetBranchID(st.ActiveBranch)
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
			rawPayload = json.RawMessage(utils.RedactSecrets(string(data)))
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
		case TaskProtocolIncomplete:
			eventType = "task_protocol_incomplete"
		}
		if eventType == "" {
			continue
		}

		transitionKey := taskTransitionEventKey(item)
		c.mu.Lock()
		alreadyEmitted := c.emittedTaskTransitions[transitionKey]
		if !alreadyEmitted {
			c.emittedTaskTransitions[transitionKey] = true
		}
		c.mu.Unlock()

		if alreadyEmitted {
			continue
		}

		payload := map[string]interface{}{
			"id":         item.ID,
			"desc":       item.Desc,
			"status":     string(item.Status),
			"output":     item.Output,
			"agent":      item.Agent,
			"depends_on": item.DependsOn,
		}
		if item.Verify != "" {
			payload["verify"] = item.Verify
			payload["verify_mode"] = item.VerifyMode
		}
		if item.VerifySpec != nil {
			payload["verify_spec"] = item.VerifySpec
		}
		if item.VerifyResult != nil {
			payload["verify_result"] = item.VerifyResult
		}
		if item.ExecutionReceipt != nil {
			payload["execution_receipt"] = item.ExecutionReceipt
		}
		if len(item.ExecutionReceipts) > 0 {
			payload["execution_receipts"] = item.ExecutionReceipts
		}

		var rawPayload json.RawMessage
		data, err := json.Marshal(payload)
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

		c.emitArtifactEvents(item)
	}
}

// taskTransitionEventKey identifies the checkpointed task state that has
// already been emitted. A verifier is part of that durable state: retaining
// only ID/status/retry would suppress a changed typed contract and make branch
// replay restore stale verification requirements. The no-verifier form stays
// byte-for-byte compatible with existing idempotency keys.
func taskTransitionEventKey(item *TodoItem) string {
	if item == nil {
		return ""
	}
	base := fmt.Sprintf("%s:%s:%d", item.ID, item.Status, item.Retries)
	if normalizedVerificationSpecForCache(item.VerifySpec, item.Verify, item.VerifyMode) == nil {
		return base
	}
	contract := taskCacheIdentityWithSpec("", item.VerifySpec, item.Verify, item.VerifyMode)
	sum := sha256.Sum256([]byte(contract))
	return base + ":verify-" + hex.EncodeToString(sum[:8])
}

// emitArtifactEvents dual-writes one artifact_created event per artifact path
// declared by a completed task's typed result. Emission is idempotent per
// (task, path) within this coordinator instance.
func (c *Coordinator) emitArtifactEvents(item *TodoItem) {
	if c.eventStore == nil || item == nil || item.TypedResult == nil {
		return
	}
	for _, art := range item.TypedResult.Artifacts {
		if art.Path == "" {
			continue
		}
		key := fmt.Sprintf("artifact:%s:%s", item.ID, art.Path)
		c.mu.Lock()
		alreadyEmitted := c.emittedTaskTransitions[key]
		if !alreadyEmitted {
			c.emittedTaskTransitions[key] = true
		}
		c.mu.Unlock()
		if alreadyEmitted {
			continue
		}

		payload, _ := json.Marshal(map[string]string{
			"path":        art.Path,
			"description": art.Description,
			"task_id":     item.ID,
		})
		if err := c.eventStore.Append(RunEvent{
			Type:           "artifact_created",
			Actor:          item.Agent,
			TaskID:         item.ID,
			IdempotencyKey: key,
			Payload:        payload,
		}); err != nil {
			log.Printf("warning: dual-write artifact event emit failed for %s (%s): %v", item.ID, art.Path, err)
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
