package team

import (
	"encoding/json"
	"strings"
)

// DeprecatedMemoryToolUsage is a content-free aggregate used to decide when
// compatibility aliases can be removed. It never includes memory input.
// DeprecatedMemoryToolReport counts only events stamped with the
// coordinator's current executionRunID. NewEventStore reuses the same
// session-level JSONL log across runs, so without a RunID filter a fresh
// run would inherit prior successful, fail-closed, and denied events
// (HF-MEM5-007: 本次 run counts only). When no active run is in scope
// the report falls back to the most recently completed-run snapshot that
// beginExecutionRun captured just before clearing coordinator state; this
// preserves the per-run aggregate for post-run --report generation. The
// snapshot's *presence* (not its length) is the sentinel for "a completed
// run was captured": a zero-use run captures an empty snapshot too, and
// that empty snapshot is the authoritative answer for that run — falling
// through to disk rehydration on empty length would misreport a fresh
// run as having inherited the prior run's compatibility counts. Disk
// rehydration is therefore reserved for a coordinator that has never
// completed a run (fresh process state with no in-memory capture).
type DeprecatedMemoryToolUsage struct {
	Tool       string `json:"tool"`
	Calls      int    `json:"calls"`
	Success    int    `json:"success"`
	FailClosed int    `json:"fail_closed"`
	Denied     int    `json:"denied"`
}

// computeDeprecatedMemoryToolUsage runs the same content-free aggregate
// over a slice of RunEvent as DeprecatedMemoryToolReport does against the
// live event store. It exists so beginExecutionRun's deferred close can
// snapshot the just-completed run's counts before clearing the active
// runID and closing the event store, and so DeprecatedMemoryToolReport
// can reuse the same logic over a possibly-rehydrated event stream.
func computeDeprecatedMemoryToolUsage(activeRunID string, events []RunEvent) []DeprecatedMemoryToolUsage {
	order := []string{"stm_write", "ltm_update", "memory_save"}
	counts := make(map[string]*DeprecatedMemoryToolUsage, len(order))
	for _, name := range order {
		counts[name] = &DeprecatedMemoryToolUsage{Tool: name}
	}
	for _, event := range events {
		// RunID is stamped by EventStore.AppendPersisted. Without this filter
		// a single shared session log aggregates every prior run's events.
		if strings.TrimSpace(event.RunID) != activeRunID {
			continue
		}
		switch event.Type {
		case "deprecated_memory_tool_called":
			var payload struct {
				ToolName    string `json:"tool_name"`
				Disposition string `json:"disposition"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || counts[payload.ToolName] == nil {
				continue
			}
			entry := counts[payload.ToolName]
			entry.Calls++
			if payload.Disposition == "success" {
				entry.Success++
			} else {
				entry.FailClosed++
			}
		case "policy_decision":
			var payload struct {
				Kind     string         `json:"kind"`
				Tool     string         `json:"tool"`
				Decision PolicyDecision `json:"decision"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Kind == "tool" && payload.Decision.Code == DecisionDeny && counts[payload.Tool] != nil {
				counts[payload.Tool].Denied++
			}
		}
	}
	var report []DeprecatedMemoryToolUsage
	for _, name := range order {
		entry := counts[name]
		if entry.Calls+entry.Denied > 0 {
			report = append(report, *entry)
		}
	}
	return report
}

func (c *Coordinator) DeprecatedMemoryToolReport() []DeprecatedMemoryToolUsage {
	if c == nil {
		return nil
	}
	activeRunID := strings.TrimSpace(c.executionRunID)
	if activeRunID != "" && c.eventStore != nil {
		events, err := c.eventStore.ReadEvents()
		if err == nil {
			return computeDeprecatedMemoryToolUsage(activeRunID, events)
		}
	}
	// No active run is in scope (post-run --report or a coordinator that
	// never began a run). Fall back to the snapshot the deferred close in
	// beginExecutionRun captured just before it cleared executionRunID and
	// closed the event store, so the per-run aggregate stays observable
	// after the run completes (HF-MEM5-007 post-run visibility).
	//
	// The capture flag, not the slice length, decides whether a completed
	// run was captured. A zero-use run captures an empty slice; that empty
	// slice is the authoritative per-run answer and must not fall through
	// to disk rehydration (which would inherit the prior run's counts).
	c.lastCompletedRunDeprecatedMu.RLock()
	captured := c.lastCompletedRunDeprecatedCapture
	snapshot := append([]DeprecatedMemoryToolUsage(nil), c.lastCompletedRunDeprecatedReport...)
	c.lastCompletedRunDeprecatedMu.RUnlock()
	if captured {
		return snapshot
	}
	// Last resort: try to rehydrate from the on-disk event log so a
	// coordinator that resumed a session (no live event store, no in-memory
	// snapshot) can still surface the last run's counts. Without this, a
	// crash-resumed run with a fresh executionRunID would silently drop
	// the prior run's compatibility report.
	if c.session != nil && c.session.Workspace != "" {
		if store, err := NewEventStore(c.session.Workspace, "", ""); err == nil {
			defer func() { _ = store.Close() }()
			if events, err := store.ReadEvents(); err == nil {
				// Pick the most recent runID present in the on-disk log
				// that has at least one compatibility observation. Using
				// the most-recent runID (rather than nil) keeps the
				// "per-run" contract for the resumed run while still
				// surfacing the prior run's counts when only history is
				// available.
				lastRunID := latestRunIDWithCompatibilityUsage(events)
				if lastRunID != "" {
					return computeDeprecatedMemoryToolUsage(lastRunID, events)
				}
			}
		}
	}
	return nil
}

// latestRunIDWithCompatibilityUsage returns the most recent RunID present
// in the event stream that has at least one deprecated_memory_tool_called
// or tool-deny observation. The scan order is chronological so a resumed
// session surfaces its immediate predecessor's counts.
func latestRunIDWithCompatibilityUsage(events []RunEvent) string {
	var current string
	for _, event := range events {
		switch event.Type {
		case "deprecated_memory_tool_called":
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ToolName != "" {
				if event.RunID != "" {
					current = event.RunID
				}
			}
		case "policy_decision":
			var payload struct {
				Kind string `json:"kind"`
				Tool string `json:"tool"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Kind == "tool" && (payload.Tool == "stm_write" || payload.Tool == "ltm_update" || payload.Tool == "memory_save") {
				if event.RunID != "" {
					current = event.RunID
				}
			}
		}
	}
	return current
}

// captureCompletedRunDeprecatedReport builds and stores the snapshot the
// deferred close in beginExecutionRun uses so DeprecatedMemoryToolReport
// can keep returning the just-completed run's counts after the event
// store is closed. It is also exposed for tests so they can drive the
// same lifecycle without going through RunOrchestrator.
func (c *Coordinator) captureCompletedRunDeprecatedReport() {
	if c == nil {
		return
	}
	c.executionEventsMu.RLock()
	runID := c.executionRunID
	store := c.eventStore
	c.executionEventsMu.RUnlock()
	if runID == "" || store == nil {
		return
	}
	events, err := store.ReadEvents()
	if err != nil {
		return
	}
	snapshot := computeDeprecatedMemoryToolUsage(strings.TrimSpace(runID), events)
	c.lastCompletedRunDeprecatedMu.Lock()
	c.lastCompletedRunDeprecatedReport = snapshot
	c.lastCompletedRunDeprecatedCapture = true
	c.lastCompletedRunDeprecatedMu.Unlock()
}
