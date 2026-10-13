package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Branch-specific session state that session.json checkpoints and replay must
// rebuild from a branch's lineage: the runtime workflow, the acceptance
// contract revisions, and the continuation checkpoint.

// WorkflowStateChangedPayload records the runtime workflow snapshot a
// checkpoint publishes. Replay keeps the last one in a lineage. The runtime
// workspace root is workspace configuration, not branch state, so it is not
// recorded.
type WorkflowStateChangedPayload struct {
	State         Phase                 `json:"state"`
	PhaseResults  map[Phase]PhaseResult `json:"phase_results,omitempty"`
	RetryAttempts map[string]int        `json:"retry_attempts,omitempty"`
}

func newWorkflowStateChangedPayload(state Phase, results map[Phase]PhaseResult, retry *RetryState) WorkflowStateChangedPayload {
	payload := WorkflowStateChangedPayload{State: state, PhaseResults: results}
	if retry != nil {
		payload.RetryAttempts = retry.Attempts
	}
	return payload
}

// emitWorkflowStateChange appends payload unless it equals the snapshot this
// coordinator last appended. The event carries no idempotency key: replay
// keeps the last snapshot, so a repeated append is harmless, while a key
// would swallow a return to an earlier state. The caller serializes calls
// with the session.json write of the same snapshot, so the lineage and the
// checkpoint advance in the same order.
func (c *Coordinator) emitWorkflowStateChange(payload WorkflowStateChangedPayload) error {
	if c == nil || c.eventStore == nil || payload.State == "" {
		return nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal workflow state: %w", err)
	}
	sum := sha256.Sum256(data)
	fingerprint := hex.EncodeToString(sum[:])
	if fingerprint == c.workflowEventFingerprint {
		return nil
	}
	if err := c.emitEvent(string(EventWorkflowStateChanged), "coordinator", "", payload); err != nil {
		return fmt.Errorf("append workflow state: %w", err)
	}
	c.workflowEventFingerprint = fingerprint
	return nil
}

func validateWorkflowStateChangedPayload(event RunEvent) error {
	var payload WorkflowStateChangedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode %s payload: %w", event.Type, err)
	}
	if strings.TrimSpace(string(payload.State)) == "" {
		return fmt.Errorf("%s payload has no workflow state", event.Type)
	}
	return nil
}

func reduceWorkflowStateChanged(session *SessionData, event RunEvent) {
	var payload WorkflowStateChangedPayload
	if json.Unmarshal(event.Payload, &payload) != nil || payload.State == "" {
		return
	}
	session.WorkflowState = payload.State
	session.PhaseResults = payload.PhaseResults
	retry := NewRetryState()
	for signature, attempts := range payload.RetryAttempts {
		retry.Attempts[signature] = attempts
	}
	session.RetryState = retry
}

// reduceAcceptanceContractModified appends the revision the coordinator
// records with acceptance_contract_modified. The event envelope carries its
// time.
func reduceAcceptanceContractModified(session *SessionData, event RunEvent) {
	var revision AcceptanceContractRevision
	if json.Unmarshal(event.Payload, &revision) != nil || revision.Revision <= 0 {
		return
	}
	revision.Timestamp = event.Timestamp
	session.AcceptanceContractRevisions = append(session.AcceptanceContractRevisions, revision)
}

// reduceContinuationCheckpoint keeps the last continuation checkpoint. Every
// save emits the complete checkpoint, including the resumed transition.
func reduceContinuationCheckpoint(session *SessionData, event RunEvent) {
	var checkpoint ContinuationCheckpoint
	if json.Unmarshal(event.Payload, &checkpoint) != nil || checkpoint.Status == "" {
		return
	}
	session.ContinuationCheckpoint = &checkpoint
}
