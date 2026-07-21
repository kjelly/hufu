package team

import (
	"context"
	"strings"

	"github.com/anomalyco/hufu/internal/agent"
)

type SideEffectClass string

const (
	SideEffectNone           SideEffectClass = "none"
	SideEffectWorkspaceWrite SideEffectClass = "workspace_write"
	SideEffectExternalWrite  SideEffectClass = "external_write"
	SideEffectInfraMutation  SideEffectClass = "infra_mutation"
	SideEffectCredential     SideEffectClass = "credential_mutation"
)

type RecoveryPolicy string

const (
	RecoveryRetry     RecoveryPolicy = "retry"
	RecoveryReconcile RecoveryPolicy = "reconcile"
	RecoveryManual    RecoveryPolicy = "manual"
	RecoveryNever     RecoveryPolicy = "never"
)

type ToolRecoverySpec struct {
	RetrySafe      bool   `json:"retry_safe,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	ReconcileTool  string `json:"reconcile_tool,omitempty"`
	CompensateTool string `json:"compensate_tool,omitempty"`
}

const (
	RecoveryStateNotStarted = "not_started"
	RecoveryStateComplete   = "complete"
	RecoveryStatePartial    = "partial"
	RecoveryStateUnknown    = "unknown"
)

// Reconcile exit codes used by read-only probe commands to classify the
// state of an interrupted task (§11.4–11.5). A reconcile/verify tool is
// expected to exit with one of these codes:
//   - 0 (ReconcileExitComplete):   the operation finished successfully
//   - 1 (ReconcileExitNotStarted): the operation never started
//   - 2 (ReconcileExitPartial):    the operation partially completed
//   - any other code:              state cannot be determined (unknown)
const (
	ReconcileExitComplete   = 0
	ReconcileExitNotStarted = 1
	ReconcileExitPartial    = 2
)

// highRiskTools are tools capable of mutating external infrastructure or
// escalating privileges; their presence on an agent implies the most
// conservative side-effect class.
var highRiskTools = map[string]SideEffectClass{
	"sudo": SideEffectInfraMutation, // privilege escalation / infra-level ops
}

// externalTools touch systems outside the local workspace (remote hosts,
// external APIs); their presence implies external_write unless a higher
// risk tool is also present.
var externalTools = map[string]bool{
	"ssh": true, // remote host operations
}

// writeTools can mutate the local workspace; their presence implies
// workspace_write unless a higher risk tool is also present.
var writeTools = map[string]bool{
	"bash":          true,
	"golang":        true,
	"lua":           true,
	"write":         true,
	"edit":          true,
	"multiedit":     true,
	"download":      true,
	"fetch":         true,
	"agentic_fetch": true,
}

// InferSideEffectClass derives a conservative side-effect classification from
// an agent's comma-separated tool list (§11.2). The highest-risk tool present
// determines the class:
//
//	sudo              → infra_mutation
//	ssh               → external_write
//	bash/golang/lua/  → workspace_write
//	write/edit/...
//	(read-only only)  → none
//	(empty)           → none
//
// This is a heuristic default only; an explicit side_effect declared on the
// task (TaskDef) or the agent (.md frontmatter) always takes precedence.
func InferSideEffectClass(tools string) SideEffectClass {
	if tools == "" || tools == "all" {
		// "all" grants every tool including sudo/ssh — be conservative.
		if tools == "all" {
			return SideEffectInfraMutation
		}
		return SideEffectNone
	}
	hasExternal := false
	hasWrite := false
	for _, t := range strings.Split(tools, ",") {
		name := strings.TrimSpace(t)
		if class, ok := highRiskTools[name]; ok {
			return class
		}
		if externalTools[name] {
			hasExternal = true
		}
		if writeTools[name] {
			hasWrite = true
		}
	}
	if hasExternal {
		return SideEffectExternalWrite
	}
	if hasWrite {
		return SideEffectWorkspaceWrite
	}
	return SideEffectNone
}

// DefaultRecoveryPolicy derives the default recovery policy based on SideEffectClass
// and whether unattended mode is active (§11.2 - §11.3 & strict doc §15).
func DefaultRecoveryPolicy(class SideEffectClass, isUnattended bool) RecoveryPolicy {
	switch class {
	case SideEffectNone, SideEffectWorkspaceWrite:
		return RecoveryRetry
	case SideEffectExternalWrite:
		if isUnattended {
			return RecoveryManual
		}
		return RecoveryReconcile
	case SideEffectInfraMutation:
		return RecoveryManual
	case SideEffectCredential:
		return RecoveryManual
	default:
		// Backward compatibility: unspecified/empty side effect maintains existing behavior (retry).
		return RecoveryRetry
	}
}

// ResolveRecoveryPolicy returns the explicit policy if provided, otherwise infers
// the default recovery policy from the side-effect class.
func ResolveRecoveryPolicy(explicit RecoveryPolicy, class SideEffectClass, isUnattended bool) RecoveryPolicy {
	if explicit != "" {
		return explicit
	}
	return DefaultRecoveryPolicy(class, isUnattended)
}

// reconcileInterruptedTask executes the read-only reconciliation flow for a task (§11.4, §15.2):
// 1. inspect unfinished operation & side-effect class
// 2. run read-only reconcile tool or verify command if available
// 3. classify state: complete, not_started, partial, unknown
func (c *Coordinator) reconcileInterruptedTask(ctx context.Context, it *TodoItem) string {
	reconcileCmd := it.ReconcileTool
	if reconcileCmd == "" {
		reconcileCmd = it.Verify
	}

	if reconcileCmd != "" {
		res, err := c.verifyTaskDeliverable(ctx, nil, reconcileCmd)
		if err == nil {
			return RecoveryStateComplete
		}
		if res != nil {
			switch res.ExitCode {
			case ReconcileExitComplete:
				return RecoveryStateComplete
			case ReconcileExitNotStarted:
				return RecoveryStateNotStarted
			case ReconcileExitPartial:
				return RecoveryStatePartial
			default:
				return RecoveryStateUnknown
			}
		}
		return RecoveryStateUnknown
	}

	if strings.TrimSpace(it.Output) != "" {
		return RecoveryStateComplete
	}

	return RecoveryStateUnknown
}

// resolveTaskRecovery applies the 3-tier side-effect / recovery precedence
// (§11.2) for a task being dispatched by the coordinator:
//
//  1. task-level explicit (TaskDef fields) — highest priority
//  2. agent-level default (agent .md frontmatter: SideEffect/Recovery/ReconcileTool)
//  3. tool-inferred heuristic (InferSideEffectClass) — lowest, only for SideEffect
//
// A nil agentDef (unknown agent, already rejected by validation) yields the
// task-level values unchanged (empty → SideEffectNone → retry), preserving
// backward compatibility.
func resolveTaskRecovery(def *agent.AgentDef, t TaskDef) (SideEffectClass, RecoveryPolicy, string) {
	sideEffect := t.SideEffect
	recovery := t.Recovery
	reconcileTool := t.ReconcileTool
	if def != nil {
		if sideEffect == "" {
			sideEffect = SideEffectClass(def.SideEffect)
		}
		if sideEffect == "" {
			sideEffect = InferSideEffectClass(def.Tools)
		}
		if recovery == "" {
			recovery = RecoveryPolicy(def.Recovery)
		}
		if reconcileTool == "" {
			reconcileTool = def.ReconcileTool
		}
	}
	return sideEffect, recovery, reconcileTool
}
