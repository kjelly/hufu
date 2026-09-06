package team

import (
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Per-tool recovery resolution for the commit gate
// (docs/hufu-decision-aware-runtime-spec.md §30.1).
//
// EvaluateCommitGate decides require-rollback from the invoked tool's
// ToolRecoverySpec. This file is the only place that spec is produced, so
// there is exactly one answer to "can this tool's mutation be undone" and it
// comes from authored configuration.

// resolveToolRecovery returns the recovery contract for one invoked tool.
//
// Precedence mirrors resolveTaskRecovery: an explicit task declaration
// overrides the agent default, field by field, so a task can add a
// compensating operation without restating the whole contract. Tool names are
// matched case-insensitively because the tool boundary is.
func resolveToolRecovery(def *agent.AgentDef, task TaskDef, toolName string) ToolRecoverySpec {
	spec := ToolRecoverySpec{}
	// Least specific first, so a more specific declaration overwrites it.
	// A static action is gated as "capability:type"; an operator who declares
	// recovery for the capability covers every operation it exposes, and may
	// still name one operation exactly.
	for _, name := range toolRecoveryLookupNames(toolName) {
		if def != nil {
			if decl, ok := lookupToolRecoveryDecl(def.ToolRecovery, name); ok {
				mergeToolRecovery(&spec, ToolRecoverySpec{
					RetrySafe:      decl.RetrySafe,
					IdempotencyKey: decl.IdempotencyKey,
					ReconcileTool:  decl.ReconcileTool,
					CompensateTool: decl.CompensateTool,
				})
			}
		}
		if override, ok := lookupToolRecoverySpec(task.ToolRecovery, name); ok {
			mergeToolRecovery(&spec, override)
		}
	}
	// A task-level reconcile probe classifies the whole task, including this
	// tool's operation. Inheriting it keeps an author from restating the same
	// command once per tool.
	if spec.ReconcileTool == "" {
		spec.ReconcileTool = strings.TrimSpace(task.ReconcileTool)
	}
	return spec
}

// toolRecoveryLookupNames returns the declaration keys that apply to one
// invoked tool or action, least specific first.
func toolRecoveryLookupNames(toolName string) []string {
	name := normalizedToolName(toolName)
	if name == "" {
		return nil
	}
	if capability, _, found := strings.Cut(name, ":"); found && capability != "" {
		return []string{capability, name}
	}
	return []string{name}
}

// mergeToolRecovery applies a more specific declaration over a less specific
// one, field by field, so a narrow override need not restate the contract.
func mergeToolRecovery(spec *ToolRecoverySpec, override ToolRecoverySpec) {
	if override.RetrySafe {
		spec.RetrySafe = true
	}
	if value := strings.TrimSpace(override.IdempotencyKey); value != "" {
		spec.IdempotencyKey = value
	}
	if value := strings.TrimSpace(override.ReconcileTool); value != "" {
		spec.ReconcileTool = value
	}
	if value := strings.TrimSpace(override.CompensateTool); value != "" {
		spec.CompensateTool = value
	}
}

func normalizedToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func lookupToolRecoveryDecl(decls map[string]agent.ToolRecoveryDecl, name string) (agent.ToolRecoveryDecl, bool) {
	for key, decl := range decls {
		if normalizedToolName(key) == name {
			return decl, true
		}
	}
	return agent.ToolRecoveryDecl{}, false
}

func lookupToolRecoverySpec(specs map[string]ToolRecoverySpec, name string) (ToolRecoverySpec, bool) {
	for key, spec := range specs {
		if normalizedToolName(key) == name {
			return spec, true
		}
	}
	return ToolRecoverySpec{}, false
}

// agentCanInvoke reports whether an agent is able to call the named tool.
//
// It exists so require-rollback means "a rollback path the worker can
// actually take". A compensate tool the agent may not invoke is a promise
// nobody can keep, and accepting it would let a typo pass the gate.
func agentCanInvoke(def *agent.AgentDef, toolName string) bool {
	name := normalizedToolName(toolName)
	if name == "" {
		return false
	}
	if def == nil {
		// Without a resolved agent the tool set is unknown. Fail open here
		// rather than block every mutation: the gate's other prerequisites
		// still apply, and a missing agent is already rejected upstream.
		return true
	}
	for _, declared := range strings.Split(def.Tools, ",") {
		declared = normalizedToolName(declared)
		if declared == "" {
			continue
		}
		if strings.HasPrefix(declared, "-") {
			continue
		}
		if declared == name || declared == "*" || declared == "all" {
			return true
		}
	}
	for mcpName := range def.MCPTools {
		if normalizedToolName(mcpName) == name {
			return true
		}
	}
	return false
}
