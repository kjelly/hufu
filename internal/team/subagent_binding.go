package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// ProviderBinding is the durable provider/session identity for one task
// occurrence (docs/hufu-external-coding-agent-runtime-spec.md §7.1). It MUST
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
	payload := ProviderSessionBoundPayload{
		TaskID: taskID, Attempt: attempt, Provider: binding.Provider, Protocol: binding.Protocol,
		SessionID: binding.SessionID, ExecutionWorldID: binding.ExecutionWorldID, CWD: binding.CWD,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("persist provider session binding: marshal payload: %w", err)
	}
	key := fmt.Sprintf("provider-session-bound:%s:%d:%s", taskID, attempt, binding.SessionID)
	if _, err := journal.Append(ctx, RunEvent{
		Type: string(EventProviderSessionBound), Actor: "coordinator", TaskID: taskID, Attempt: attempt,
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
	return nil
}

// resolveSubagentProvider implements the static provider-selection precedence
// (docs/hufu-external-coding-agent-runtime-spec.md §6.4):
//
//	TaskDef.SubagentProvider > AgentDef.SubagentProvider >
//	TeamConfig.SubagentProviderDefault > hufu-local
//
// The result is normalized to lowercase and validated against
// SubagentRegistry before it is ever assigned to a durable task occurrence,
// so an unknown provider fails closed before any TODO, model call, or
// workspace side effect.
func (c *Coordinator) resolveSubagentProvider(task TaskDef, def *agent.AgentDef) (string, error) {
	candidate := strings.ToLower(strings.TrimSpace(task.SubagentProvider))
	if candidate == "" && def != nil {
		candidate = strings.ToLower(strings.TrimSpace(def.SubagentProvider))
	}
	if candidate == "" && c != nil && c.session != nil {
		candidate = strings.ToLower(strings.TrimSpace(c.session.Config.SubagentProviderDefault))
	}
	if candidate == "" {
		candidate = localSubagentProviderName
	}
	if c == nil {
		return "", fmt.Errorf("resolve subagent provider: coordinator is unavailable")
	}
	if _, err := c.SubagentRegistry().Resolve(candidate); err != nil {
		return "", fmt.Errorf("resolve subagent provider: %w", err)
	}
	return candidate, nil
}
