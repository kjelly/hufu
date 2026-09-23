package context

import "time"

// IsCurrentPersistentKnowledge reports whether item is current, confirmed,
// persistent knowledge at project, team, or agent scope: confirmed, not
// superseded, without session/branch/task/attempt scope, not expired, and
// marked persistent by its metadata. Scope values, kinds, and content checks
// stay with the caller. ValidUntil is deliberately not consulted, matching
// the promotion eligibility contract.
func IsCurrentPersistentKnowledge(item ContextItem, now time.Time) bool {
	if item.Lifecycle != LifecycleConfirmed || item.SupersededBy != "" {
		return false
	}
	if item.Scope.SessionID != "" || item.Scope.BranchID != "" || item.Scope.TaskID != "" || item.Scope.AttemptID != "" {
		return false
	}
	if item.ExpiresAt != nil && !item.ExpiresAt.After(now) {
		return false
	}
	return item.Metadata != nil && (item.Metadata["memory_lifetime"] == "persistent" || item.Metadata["memory_tier"] == "persistent")
}
