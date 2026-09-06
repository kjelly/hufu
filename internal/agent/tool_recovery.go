package agent

import (
	"fmt"
	"sort"
	"strings"
)

// Per-tool recovery declarations
// (docs/hufu-decision-aware-runtime-spec.md §30.1).
//
// A task's side-effect class says how dangerous its mutation is. It does not
// say whether that mutation can be undone: that is a property of the specific
// tool the worker invokes. The commit gate's require-rollback prerequisite is
// decided from this declaration, so it lives in authored configuration rather
// than in anything a model can supply at runtime.

// ToolRecoveryDecl is the recovery contract an agent declares for one of its
// tools. Every field is optional; an absent declaration means the tool offers
// no recovery affordance, which is what a require-rollback profile blocks on.
type ToolRecoveryDecl struct {
	// RetrySafe marks an operation whose repetition is harmless. It is a
	// claim about the tool, not about a particular call.
	RetrySafe bool `yaml:"retry-safe,omitempty" json:"retry_safe,omitempty"`
	// IdempotencyKey names the request field that makes a repeated call
	// collapse into the first one.
	IdempotencyKey string `yaml:"idempotency-key,omitempty" json:"idempotency_key,omitempty"`
	// ReconcileTool is a read-only probe that classifies whether this tool's
	// operation completed.
	ReconcileTool string `yaml:"reconcile-tool,omitempty" json:"reconcile_tool,omitempty"`
	// CompensateTool is the operation that undoes a completed mutation. It is
	// the only way to satisfy require-rollback.
	CompensateTool string `yaml:"compensate-tool,omitempty" json:"compensate_tool,omitempty"`
}

// ValidateToolRecovery rejects declarations that could not be acted on. A
// silently ignored entry is worse than a rejected one: an operator would
// believe a rollback path exists where the gate can find none.
func ValidateToolRecovery(decls map[string]ToolRecoveryDecl) error {
	names := make([]string, 0, len(decls))
	for name := range decls {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("tool-recovery has an entry with an empty tool name")
		}
		decl := decls[name]
		for field, value := range map[string]string{
			"reconcile-tool":  decl.ReconcileTool,
			"compensate-tool": decl.CompensateTool,
			"idempotency-key": decl.IdempotencyKey,
		} {
			if value != "" && strings.TrimSpace(value) == "" {
				return fmt.Errorf("tool-recovery[%q].%s is blank", name, field)
			}
		}
		if strings.EqualFold(strings.TrimSpace(decl.CompensateTool), strings.TrimSpace(name)) {
			return fmt.Errorf("tool-recovery[%q].compensate-tool cannot be the tool it compensates", name)
		}
	}
	return nil
}
