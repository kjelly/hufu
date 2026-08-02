package team

import "strings"

// systemicScopeKey aggregates failures across distinct tasks by
// (component, operation, class, digest). The criterion is deliberately
// excluded so a defect that manifests under different criteria still
// aggregates (§6.2). Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func systemicScopeKey(fp FailureFingerprint) string {
	return strings.Join([]string{fp.Component, fp.Operation, string(fp.Class), fp.Digest}, "\x00")
}

// systemicScopePrefixKey is the deterministic pre-dispatch prefix of a
// systemic scope: (component, operation). It is the strongest signal
// available for a candidate task before it has failed (class and digest
// are failure-time properties). Used to block future un-fingerprinted
// tasks whose component+operation matches an escalated systemic scope
// (§6.2: 停止對該 scope 派工). Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func systemicScopePrefixKey(component, operation string) string {
	return component + "\x00" + operation
}

// splitSystemicScopePrefix extracts the (component, operation) prefix from
// a systemic scope key produced by systemicScopeKey. Returns ok=false if
// the key is malformed (fewer than 2 fields).
func splitSystemicScopePrefix(scopeKey string) (component, operation string, ok bool) {
	parts := strings.SplitN(scopeKey, "\x00", 3)
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// SystemicDispositionForClass returns the escalation disposition for a
// systemic failure of the given class (§6.2): protocol / environment /
// contract → needs_human; any other → replan_required. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func SystemicDispositionForClass(class TaskFailureClass) string {
	switch class {
	case FailureProtocol, FailureEnvironment, FailureContract:
		return "needs_human"
	default:
		return "replan_required"
	}
}

// systemicTaskCount returns the number of distinct task IDs that have
// observed a failure in the given systemic scope. Used for event payload
// and metrics. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func (s *AntiThrashingState) systemicTaskCount(scopeKey string) int {
	if s == nil || s.SystemicCounts == nil {
		return 0
	}
	return len(s.SystemicCounts[scopeKey])
}

// recordSystemic tracks how many distinct task IDs have observed an
// equivalent (component, operation, class, digest) failure. When the
// distinct task count reaches MaxSystemicFailureTasks the scope is
// escalated: SystemicEscalations is incremented exactly once per scope
// (tracked by EscalatedSystemicScopes, separate from the hard-block set
// so warn-only runs can still count/emit once), and under HardEnforcement
// the scope's (component, operation) prefix is recorded in
// BlockedSystemicScopePrefixes so future un-fingerprinted tasks with the
// same component+operation are blocked at dispatch (§6.2: 停止對該 scope
// 派工). Returns true only on the threshold-crossing call that first
// escalates this scope.
//
// The escalation is irreversible for the run: once a systemic defect is
// declared, dispatch to that scope is blocked even if one of the
// contributing tasks later makes criterion progress (a systemic defect
// is a property of the system, not of a single criterion). Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func (s *AntiThrashingState) recordSystemic(item *TodoItem, fp FailureFingerprint, limits ReliabilityConfig) bool {
	if limits.MaxSystemicFailureTasks <= 0 {
		return false
	}
	s.ensureSystemicMaps()
	key := systemicScopeKey(fp)
	taskID := ""
	if item != nil {
		taskID = strings.TrimSpace(item.ID)
	}
	if taskID == "" {
		// A failure with no task identity cannot contribute a distinct
		// task; skip to avoid undercounting or overcounting.
		return false
	}
	if s.SystemicCounts[key] == nil {
		s.SystemicCounts[key] = make(map[string]bool)
	}
	s.SystemicCounts[key][taskID] = true
	// One-time escalation per scope, in both live and warn-only modes.
	// EscalatedSystemicScopes is the durable marker that this scope has
	// already crossed the threshold and been counted; it is reconstructed
	// by rebuild() so replay does not re-emit.
	if s.EscalatedSystemicScopes[key] {
		return false
	}
	if len(s.SystemicCounts[key]) >= limits.MaxSystemicFailureTasks {
		s.EscalatedSystemicScopes[key] = true
		s.SystemicEscalations++
		if limits.HardEnforcement {
			s.BlockedSystemicScopes[key] = true
			s.BlockedSystemicScopePrefixes[systemicScopePrefixKey(fp.Component, fp.Operation)] = true
			s.HardBlocked = true
		}
		return true
	}
	return false
}

// ensureSystemicMaps lazily initializes the systemic-scope maps so callers
// do not need to repeat the nil checks. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func (s *AntiThrashingState) ensureSystemicMaps() {
	if s.SystemicCounts == nil {
		s.SystemicCounts = make(map[string]map[string]bool)
	}
	if s.BlockedSystemicScopes == nil {
		s.BlockedSystemicScopes = make(map[string]bool)
	}
	if s.BlockedSystemicScopePrefixes == nil {
		s.BlockedSystemicScopePrefixes = make(map[string]bool)
	}
	if s.EscalatedSystemicScopes == nil {
		s.EscalatedSystemicScopes = make(map[string]bool)
	}
}

// blockReasonSystemic returns true if the candidate task (described by its
// TaskDef and the scheduler's todo item) falls within an escalated
// systemic scope. Two matching paths:
//  1. The candidate's todo item already carries a failure fingerprint
//     whose full (component, operation, class, digest) scope was
//     escalated (a re-dispatch of a contributing task).
//  2. The candidate has NOT yet failed (no fingerprint), but its
//     deterministic (component, operation) prefix matches an escalated
//     systemic scope. This is the §6.2 "停止對該 scope 派工" requirement
//     for future un-fingerprinted tasks: class and digest are
//     failure-time properties, so the strongest pre-dispatch signal is
//     (component, operation), and we conservatively block any candidate
//     whose prefix matches.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func (s *AntiThrashingState) blockReasonSystemic(task TaskDef, item *TodoItem) bool {
	if len(s.BlockedSystemicScopes) == 0 {
		return false
	}
	if item != nil {
		for _, fp := range item.FailureFingerprints {
			if s.BlockedSystemicScopes[systemicScopeKey(fp)] {
				return true
			}
		}
	}
	if len(s.BlockedSystemicScopePrefixes) > 0 {
		component := strings.TrimSpace(task.Agent)
		// Use stableOperation (not failureOperation) so a future
		// un-fingerprinted candidate derives the SAME operation as the
		// failed task that escalated the scope, even though the failed
		// task's LastOperation was populated during execution and the
		// candidate's is empty. Refs:
		// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
		operation := stableOperation(item)
		if component != "" && operation != "" {
			if s.BlockedSystemicScopePrefixes[systemicScopePrefixKey(component, operation)] {
				return true
			}
		}
	}
	return false
}

// applySystemicThreshold is the rebuild-time systemic pass: it marks any
// systemic scope that reached the distinct-task threshold. The one-time
// escalation count (EscalatedSystemicScopes + SystemicEscalations) is
// recorded in both hard-enforcement and warn-only modes so replay is
// stable and the metric matches live behavior; the hard block
// (BlockedSystemicScopes + prefix map + HardBlocked) only applies under
// HardEnforcement. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func (s *AntiThrashingState) applySystemicThreshold(limits ReliabilityConfig) {
	if limits.MaxSystemicFailureTasks <= 0 {
		return
	}
	s.ensureSystemicMaps()
	for key, taskIDs := range s.SystemicCounts {
		if len(taskIDs) < limits.MaxSystemicFailureTasks {
			continue
		}
		if !s.EscalatedSystemicScopes[key] {
			s.EscalatedSystemicScopes[key] = true
			s.SystemicEscalations++
		}
		if limits.HardEnforcement {
			s.BlockedSystemicScopes[key] = true
			// Recover the (component, operation) prefix from the scope
			// key so future un-fingerprinted tasks can be blocked. The
			// scope key is component \x00 operation \x00 class \x00
			// digest; the prefix is the first two fields.
			if comp, op, ok := splitSystemicScopePrefix(key); ok {
				s.BlockedSystemicScopePrefixes[systemicScopePrefixKey(comp, op)] = true
			}
			s.HardBlocked = true
		}
	}
}
