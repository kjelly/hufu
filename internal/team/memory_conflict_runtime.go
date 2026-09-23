package team

import (
	"context"
	"log"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// openConflictsForItems maps shared persistent memories to their open memory
// conflicts for manifest attribution only. It is best effort: a repository
// without ConflictLookup or a failed query returns nil, is logged and
// reported as degraded observability, and never fails the model call or
// changes what enters the prompt.
func (c *Coordinator) openConflictsForItems(ctx context.Context, items []contextstore.ContextItem) map[string][]string {
	if c == nil || c.session == nil || len(items) == 0 {
		return nil
	}
	lookup, ok := c.contextRepo.(contextstore.ConflictLookup)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	scope := c.contextScope()
	conflicts, err := lookup.OpenConflictsForItems(ctx, scope.ProjectID, scope.TeamID, ids)
	if err != nil {
		redacted := contextstore.RedactSecrets(err.Error())
		log.Printf("warning: memory conflict lookup degraded: %s", redacted)
		_ = c.emitEvent("observability_degraded", "memory_conflict", "", map[string]any{"component": "memory_conflict", "error": redacted})
		return nil
	}
	return conflicts
}
