package team

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

const EventStructuredStepInvocationObserved EventType = "structured_step_invocation_observed"

// The association is runtime-owned. Its source is an already committed model
// profile, not a model name or a timestamp supplied by the step runner.
type structuredStepInvocationObserved struct {
	OccurrenceRevision int    `json:"occurrence_revision,omitzero"`
	Version            int    `json:"version"`
	ReceiptID          string `json:"receipt_id"`
	StepID             string `json:"step_id"`
	RepairAttempt      int    `json:"repair_attempt,omitzero"`
	ModelExecutionID   string `json:"model_execution_id"`
	ProfileEventID     string `json:"profile_event_id"`
	ProfileHash        string `json:"profile_hash"`
	Outcome            string `json:"outcome"`
}

func commitStructuredStepInvocation(ctx context.Context, c *Coordinator, request agent.ProviderRequest, profile RunEvent) error {
	if scope := structuredStepScopeFromContext(ctx); scope != nil {
		return scope.commit(ctx, c, request, profile)
	}
	return nil
}

func decodeStructuredStepInvocation(event RunEvent) (structuredStepInvocationObserved, error) {
	var payload structuredStepInvocationObserved
	if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
		return payload, fmt.Errorf("decode structured step invocation: %w", err)
	}
	if event.Type != string(EventStructuredStepInvocationObserved) || event.SchemaVersion < eventStoreSchemaVersion ||
		event.RunID == "" || event.BranchID == "" || event.TaskID == "" || event.Attempt < 1 || event.Actor != "runtime" ||
		payload.OccurrenceRevision < 0 || payload.Version != 1 || payload.ReceiptID == "" || payload.StepID == "" || payload.RepairAttempt < 0 ||
		payload.ModelExecutionID == "" || payload.ProfileEventID == "" || !validStructuredStepDigest(payload.ProfileHash) ||
		event.IdempotencyKey != event.BranchID+":step-invocation:"+payload.ReceiptID+":"+payload.ModelExecutionID {
		return payload, fmt.Errorf("invalid structured step invocation identity")
	}
	switch agent.ProviderInvocationOutcome(payload.Outcome) {
	case agent.ProviderInvocationSuccess, agent.ProviderInvocationProviderError, agent.ProviderInvocationCancelled, agent.ProviderInvocationStreamAbandoned:
	default:
		return payload, fmt.Errorf("invalid structured step invocation outcome")
	}
	return payload, nil
}

func validStructuredStepDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// EffectiveExecutionKind leaves old receipt bytes intact while projecting
// their absent execution evidence explicitly as unknown.
func (r ExecutionStepReceipt) EffectiveExecutionKind() string {
	if r.ExecutionKind == "" {
		return "unknown"
	}
	return r.ExecutionKind
}
