package team

import (
	"fmt"
	"strings"
)

// normalizeTaskReferenceID is the shared logical task-ID policy used by
// static contract validation and runtime resolution. Runtime Todo IDs remain
// exact identifiers; only declared PlanTaskIDs use this normalized form.
func normalizeTaskReferenceID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// resolveTaskReference is the single logical-to-runtime task identity
// boundary. Static contracts and persisted plans use PlanTaskID; task
// results, artifacts, receipts, and execution events use the runtime Todo ID.
// A reference is valid only when it identifies one runtime task.
func (c *Coordinator) resolveTaskReference(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", fmt.Errorf("task reference must not be empty")
	}
	logicalKey := normalizeTaskReferenceID(reference)
	matches := make(map[string]struct{})
	if c != nil && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item == nil {
				continue
			}
			if item.ID == reference || (item.PlanTaskID != "" && normalizeTaskReferenceID(item.PlanTaskID) == logicalKey) {
				matches[item.ID] = struct{}{}
			}
		}
	}
	if len(matches) == 1 {
		for runtimeID := range matches {
			return runtimeID, nil
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("task reference %q is ambiguous: matches %d runtime tasks", reference, len(matches))
	}

	// A few low-level callers/tests operate on a result cache without a Todo
	// projection. Preserve exact runtime-ID lookup for that case; it is not a
	// logical-ID resolution and therefore cannot hide a misspelled PlanTaskID.
	var cached bool
	if c != nil {
		c.taskResultsMu.RLock()
		_, cached = c.taskResults[reference]
		c.taskResultsMu.RUnlock()
	}
	if cached {
		return reference, nil
	}
	return "", fmt.Errorf("task reference %q does not identify a runtime task", reference)
}
