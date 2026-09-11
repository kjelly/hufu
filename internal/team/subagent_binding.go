package team

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kjelly/hufu/internal/execution"
)

// ProviderBinding is the durable provider/session identity for one task
// occurrence (docs/architecture/execution-runtime.md). It MUST
// NOT contain credentials or secrets. Provider is immutable once set;
// SessionID/TurnID/EffectiveModel MAY transition from empty to populated as
// a later external-provider phase establishes a persistent session — no
// current code path populates them yet.
type ProviderBinding struct {
	Provider string `json:"provider"`
	Protocol string `json:"protocol,omitempty"`

	// Stable provider-side reasoning/session identity.
	SessionID string `json:"session_id,omitempty"`

	// Last active provider turn, diagnostic only.
	TurnID string `json:"turn_id,omitempty"`

	// Effective provider-reported identity.
	ProviderVersion string `json:"provider_version,omitempty"`
	EffectiveModel  string `json:"effective_model,omitempty"`

	// Hufu-owned execution world identity.
	ExecutionWorldID string `json:"execution_world_id,omitempty"`

	// Frozen task workspace root/cwd assertion.
	CWD string `json:"cwd,omitempty"`

	// Provider-native sandbox projection used for this binding.
	SandboxMode string `json:"sandbox_mode,omitempty"`

	ResumeSupported bool `json:"resume_supported,omitempty"`
}

func cloneProviderBinding(b *ProviderBinding) *ProviderBinding {
	if b == nil {
		return nil
	}
	clone := *b
	return &clone
}

// ProviderSessionBoundPayload is the durable payload for
// EventProviderSessionBound (§7.4).
type ProviderSessionBoundPayload struct {
	TaskID           string `json:"task_id"`
	Attempt          int    `json:"attempt"`
	Provider         string `json:"provider"`
	Protocol         string `json:"protocol,omitempty"`
	SessionID        string `json:"session_id,omitempty"`
	ExecutionWorldID string `json:"execution_world_id,omitempty"`
	CWD              string `json:"cwd,omitempty"`
}

// BackendSessionBoundPayload is the canonical Phase 6 durable session event.
// ProviderSessionBoundPayload remains accepted only when replaying legacy logs.
type BackendSessionBoundPayload struct {
	TaskID           string                    `json:"task_id"`
	Attempt          int                       `json:"attempt"`
	ExecutionTarget  execution.ExecutionTarget `json:"execution_target"`
	Backend          string                    `json:"backend"`
	Protocol         string                    `json:"protocol,omitempty"`
	SessionID        string                    `json:"session_id,omitempty"`
	ExecutionWorldID string                    `json:"execution_world_id,omitempty"`
	CWD              string                    `json:"cwd,omitempty"`
}

// persistProviderSessionBinding durably records an external provider's
// session identity for one attempt, then updates the live Todo projection so
// a same-process retry/resume sees it without needing an event replay. It
// MUST be called, and succeed, before the caller sends any turn that depends
// on the session (§7.4, INV-11) — codexStartOrResumeThread's onSessionBound
// callback enforces exactly that ordering by construction.
func (c *Coordinator) persistProviderSessionBinding(ctx context.Context, taskID string, attempt int, binding ProviderBinding) error {
	if c == nil {
		return fmt.Errorf("persist provider session binding: coordinator is unavailable")
	}
	journal := c.EventJournal()
	if journal == nil {
		return fmt.Errorf("persist provider session binding: event journal is unavailable")
	}
	item := c.todoItemByID(taskID)
	if item == nil {
		return fmt.Errorf("persist provider session binding: task %q is unavailable", taskID)
	}
	target := item.ExecutionTarget
	if target.IsZero() {
		target = targetFromLegacyIdentity(item.Model, item.SubagentProvider)
	}
	payload := BackendSessionBoundPayload{
		TaskID: taskID, Attempt: attempt, ExecutionTarget: target, Backend: target.Backend, Protocol: binding.Protocol,
		SessionID: binding.SessionID, ExecutionWorldID: binding.ExecutionWorldID, CWD: binding.CWD,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("persist provider session binding: marshal payload: %w", err)
	}
	key := fmt.Sprintf("backend-session-bound:%s:%d:%s", taskID, attempt, binding.SessionID)
	if _, err := journal.Append(ctx, RunEvent{
		Type: string(EventBackendSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: attempt,
		IdempotencyKey: key, Payload: data,
	}); err != nil {
		return fmt.Errorf("persist provider session binding: %w", err)
	}
	if c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fmt.Errorf("persist provider session binding: task tracker is unavailable")
	}
	if err := c.taskTracker.TodoList().SetProviderBinding(taskID, &binding); err != nil {
		return fmt.Errorf("persist provider session binding: update projection: %w", err)
	}
	if err := c.taskTracker.TodoList().SetBackendBinding(taskID, backendBindingFromProviderBinding(&binding, target)); err != nil {
		return fmt.Errorf("persist provider session binding: update backend projection: %w", err)
	}
	return nil
}
