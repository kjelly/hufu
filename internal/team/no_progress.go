package team

import (
	"context"
	"fmt"
	"time"
)

// No-progress budget enforcement (§8.1, WP-12).
//
// The no-progress budget bounds a run that keeps consuming tokens, coordinator
// turns, or tasks without any objective progress. "Objective progress" means
// a criterion transitioning fail→pass (the existing resetAfterCriterionProgress
// path) or an objective verifier transitioning fail→pass; task `done` does NOT
// reset the counters.
//
// The enforcement decision is a pure function of the three counters and their
// configured limits — no coordinator state, no I/O — so it is trivially
// table-testable. The coordinator holds the counters as plain fields and
// mutates them at the real accounting chokepoints (tokens via
// recordReliabilityUsage plus coordinator-stream accounting, turns at the
// runOrchestrator boundary, and tasks at AddBatch). Reset paths are the
// existing resetAfterCriterionProgress block (three call sites) and the
// durable objective-verifier transition path; both zero all three counters.
//
// Disposition ladder (§8.1):
//   - all counters below their limits → continue
//   - any counter at its limit → replan_required (first threshold)
//   - any counter at twice its limit (a second threshold crossing after the
//     first) → stop_partial
//
// A limit of 0 disables that one counter (the YAML `0` override). warn-only
// mode (HardEnforcement == false) never returns stop/replan — the coordinator
// emits the event but does not stop or replan.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12

// NoProgressDisposition is the enforcement outcome for the no-progress budget.
type NoProgressDisposition string

const (
	// NoProgressContinue: all counters below their limits.
	NoProgressContinue NoProgressDisposition = "continue"
	// NoProgressReplan: at least one counter reached its limit (first
	// threshold). The coordinator should emit a no_progress_replan event and
	// force a replan/wrap-up turn, refusing further blind delegation.
	NoProgressReplan NoProgressDisposition = "replan_required"
	// NoProgressStop: at least one counter reached twice its limit (second
	// threshold crossing after the first). The coordinator should force
	// wrap-up, evaluate the run outcome to partial with a continuation
	// record, and stop the run.
	NoProgressStop NoProgressDisposition = "stop_partial"
)

// NoProgressCounters is the input to decideNoProgress: the three run-scoped
// counters since the last objective criterion progress.
type NoProgressCounters struct {
	Tokens int64 `json:"tokens_since_progress,omitempty"`
	Turns  int   `json:"turns_since_progress,omitempty"`
	Tasks  int   `json:"tasks_since_progress,omitempty"`
}

// NoProgressLimits is the configured ceiling for each counter. A limit of 0
// disables that counter (enforcement ignores it).
type NoProgressLimits struct {
	MaxTokens int64
	MaxTurns  int
	MaxTasks  int
}

// decideNoProgress evaluates the three counters against the configured limits
// and returns a disposition. It is a pure function: no I/O, no coordinator
// state. hardEnforcement gates whether stop/replan dispositions are returned;
// when false (warn-only) the function always returns NoProgressContinue so the
// caller can emit a warning event without stopping or replanning.
//
// Ladder (§8.1):
//   - below all limits → continue
//   - any counter at its limit → replan_required
//   - any counter at twice its limit → stop_partial
//
// A 0 limit disables that counter. The "twice its limit" check uses the
// configured (non-zero) limit, so a disabled counter never triggers stop.
func decideNoProgress(counters NoProgressCounters, limits NoProgressLimits, hardEnforcement bool) (NoProgressDisposition, string) {
	if !hardEnforcement {
		// warn-only: never stop or replan. The caller emits the event.
		return NoProgressContinue, "warn-only"
	}

	// Second threshold: any counter at twice its limit → stop_partial.
	if limits.MaxTokens > 0 && counters.Tokens >= 2*limits.MaxTokens {
		return NoProgressStop, "tokens since progress reached twice the limit"
	}
	if limits.MaxTurns > 0 && counters.Turns >= 2*limits.MaxTurns {
		return NoProgressStop, "turns since progress reached twice the limit"
	}
	if limits.MaxTasks > 0 && counters.Tasks >= 2*limits.MaxTasks {
		return NoProgressStop, "tasks since progress reached twice the limit"
	}

	// First threshold: any counter at its limit → replan_required.
	if limits.MaxTokens > 0 && counters.Tokens >= limits.MaxTokens {
		return NoProgressReplan, "tokens since progress reached the limit"
	}
	if limits.MaxTurns > 0 && counters.Turns >= limits.MaxTurns {
		return NoProgressReplan, "turns since progress reached the limit"
	}
	if limits.MaxTasks > 0 && counters.Tasks >= limits.MaxTasks {
		return NoProgressReplan, "tasks since progress reached the limit"
	}

	return NoProgressContinue, ""
}

// noProgressCounters returns a snapshot of the three no-progress counters
// under the metrics lock. Caller must hold no lock.
func (c *Coordinator) noProgressCounters() NoProgressCounters {
	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return NoProgressCounters{
		Tokens: c.tokensSinceCriterionProgress,
		Turns:  c.turnsSinceCriterionProgress,
		Tasks:  c.tasksSinceCriterionProgress,
	}
}

// recordNoProgressTasks accounts tasks at the point they are actually
// created. Keeping this beside the other no-progress accounting helpers makes
// it harder for alternate creation paths (direct agents, sub-agents, or the
// todo tool) to silently bypass the WP-12 budget.
func (c *Coordinator) recordNoProgressTasks(count int) {
	if c == nil || count <= 0 {
		return
	}
	c.metricsMu.Lock()
	c.tasksSinceCriterionProgress += count
	c.metricsMu.Unlock()
}

// noteObjectiveVerifierResult records objective progress when a task verifier
// transitions from a recorded failure to a pass. A first-ever pass is not a
// fail→pass transition, and a pass after a pass must not repeatedly extend the
// run. Verification history is read from the current todo result and durable
// per-attempt receipts so retries and crash-resume preserve the transition.
func (c *Coordinator) noteObjectiveVerifierResult(todoID string, passed bool) {
	if c == nil || todoID == "" || !passed || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return
	}
	var previous *VerificationResult
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ID != todoID {
			continue
		}
		if item.VerifyResult != nil {
			copyResult := *item.VerifyResult
			previous = &copyResult
		} else {
			for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
				if item.ExecutionReceipts[i].VerifyResult != nil {
					copyResult := *item.ExecutionReceipts[i].VerifyResult
					previous = &copyResult
					break
				}
			}
			if previous == nil && item.ExecutionReceipt != nil && item.ExecutionReceipt.VerifyResult != nil {
				copyResult := *item.ExecutionReceipt.VerifyResult
				previous = &copyResult
			}
		}
		break
	}
	if previous == nil || isVerifySuccess(previous) {
		return
	}

	// This is objective progress independent of acceptance-criterion links.
	// Keep the reset under metricsMu so concurrent worker completions cannot
	// observe a partially reset budget.
	c.metricsMu.Lock()
	c.tokensSinceCriterionProgress = 0
	c.turnsSinceCriterionProgress = 0
	c.tasksSinceCriterionProgress = 0
	c.noProgressReplanTripped = false
	c.noProgressStopTripped = false
	c.reliabilityUsageByAttempt = make(map[string]int)
	c.metricsMu.Unlock()
	if c.sessionData != nil {
		c.sessionData.LastCriterionProgressAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_ = c.emitEvent("objective_verifier_progress", "coordinator", todoID, map[string]interface{}{
		"previous_exit_code": previous.ExitCode,
		"passed":             true,
	})
}

// llmUsageNeedsDirectNoProgressAccounting reports whether an LLM stream has
// its usage carried by a worker execution receipt. The absence of the marker
// intentionally means direct accounting: coordinator and auxiliary streams
// are not receipt-backed, while explicit true is reserved for worker/direct
// agent execution paths that record cumulative receipt usage.
func llmUsageNeedsDirectNoProgressAccounting(ctx context.Context) bool {
	if ctx == nil {
		return true
	}
	expected, ok := ctx.Value(llmUsageReceiptExpectedKey{}).(bool)
	return !ok || !expected
}

// noProgressLimits returns the configured limits from the effective reliability
// config.
func (c *Coordinator) noProgressLimits() NoProgressLimits {
	rc := c.reliabilityConfig()
	return NoProgressLimits{
		MaxTokens: int64(rc.MaxTokensWithoutProgress),
		MaxTurns:  rc.MaxTurnsWithoutProgress,
		MaxTasks:  rc.MaxTasksWithoutProgress,
	}
}

// noProgressReplanPending reports whether the first threshold has already
// caused a replan request in the current accumulation. The coordinator gets
// one continuation turn to respond to that request; if that turn returns
// without objective progress, the next boundary is a stop/partial boundary.
func (c *Coordinator) noProgressReplanPending() bool {
	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return c.noProgressReplanTripped
}

func (c *Coordinator) noProgressStopPending() bool {
	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return c.noProgressStopTripped
}

// enforceNoProgressBudget evaluates the three no-progress counters against the
// configured limits and translates the disposition into the existing wrap-up /
// outcome-evaluation machinery. It is called at the task-end / round boundary
// (ExecuteTasks) and the turn boundary (ensureFinished continuation loop).
//
// Disposition handling (§8.1, WP-12):
//   - continue: no action.
//   - replan_required (first threshold): emit a notifiable no_progress_replan
//     event and mark a replan turn pending. The run is NOT terminal and new
//     delegation remains available to that replan turn. noProgressReplanTripped
//     records that the first threshold fired so the next boundary can stop if
//     the replan made no objective progress.
//   - stop_partial (second threshold): force wrap-up, evaluate the run
//     outcome to partial with a continuation record, and stop the run.
//
// Returns (stopped, reason). When stopped is true the caller returns an error
// instructing the coordinator to call finish. When false the caller continues
// normally. warn-only mode (HardEnforcement == false) emits the event but never
// stops or replans (decideNoProgress returns continue).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
func (c *Coordinator) enforceNoProgressBudget() (bool, string) {
	counters := c.noProgressCounters()
	limits := c.noProgressLimits()
	rc := c.reliabilityConfig()
	disposition, reason := decideNoProgress(counters, limits, rc.HardEnforcement)

	switch disposition {
	case NoProgressContinue:
		// warn-only: emit a warning event but do not stop or replan.
		if !rc.HardEnforcement && (counters.Tokens > 0 || counters.Turns > 0 || counters.Tasks > 0) {
			// Only emit if a counter is non-zero and a threshold would have
			// been hit under hard enforcement.
			if wouldTrip(counters, limits) {
				c.report(c.newEvent("no_progress_replan").
					withMessage(fmt.Sprintf("warn-only: %s (tokens=%d turns=%d tasks=%d)", reason, counters.Tokens, counters.Turns, counters.Tasks)).
					withTodoID(CoordTodoID))
			}
		}
		return false, ""

	case NoProgressReplan:
		// A replan was already requested for this accumulation and the
		// continuation turn did not produce objective progress. Do not emit
		// an unbounded sequence of identical replan warnings; escalate to the
		// terminal partial outcome on the next boundary.
		c.metricsMu.Lock()
		alreadyReplanned := c.noProgressReplanTripped
		if !alreadyReplanned {
			c.noProgressReplanTripped = true
		}
		c.metricsMu.Unlock()
		if alreadyReplanned {
			return c.stopForNoProgress(reason + " after replan")
		}
		// First threshold: emit an explicit replan request without entering
		// terminal wrap-up. Replan and wrap-up are different states: setting
		// wrapUp here would make ExecuteTasks reject the very delegation that
		// the replan turn may need in order to make objective progress.
		c.report(c.newEvent("no_progress_replan").
			withMessage(fmt.Sprintf("%s (tokens=%d turns=%d tasks=%d); forcing a replan turn", reason, counters.Tokens, counters.Turns, counters.Tasks)).
			withTodoID(CoordTodoID))
		return false, ""

	case NoProgressStop:
		return c.stopForNoProgress(reason)
	}
	return false, ""
}

// stopForNoProgress is the single terminal enforcement path. Keeping the
// outcome construction here ensures every counter (tokens, turns, tasks)
// produces the same partial result and continuation checkpoint semantics.
func (c *Coordinator) stopForNoProgress(reason string) (bool, string) {
	counters := c.noProgressCounters()
	limits := c.noProgressLimits()
	c.metricsMu.Lock()
	c.noProgressStopTripped = true
	c.metricsMu.Unlock()
	c.SetWrapUp()
	c.report(c.newEvent("no_progress_replan").
		withMessage(fmt.Sprintf("%s (tokens=%d turns=%d tasks=%d); stopping run with partial outcome", reason, counters.Tokens, counters.Turns, counters.Tasks)).
		withTodoID(CoordTodoID))
	var items []*TodoItem
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		items = c.taskTracker.TodoList().Items()
	}
	failedTasks := failedTodoItems(items)
	unresolvedPending := pendingTodoItems(items)
	allUnresolved := append(failedTasks, unresolvedPending...)
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: toTaskReferences(allUnresolved),
		BudgetExceeded:  true,
		Reason:          reason,
		Stats:           SummarizeRunStats(items),
		Metrics:         c.Metrics(),
		GoalMode:        c.GoalMode(),
	})
	evaluated.Continuation = &ContinuationInfo{
		TurnCount:               counters.Turns,
		MaxTurns:                limits.MaxTurns,
		Reason:                  reason,
		NoProgress:              &counters,
		NoProgressReplanPending: c.noProgressReplanPending(),
	}
	c.SetLastRunResult(&evaluated)
	// Preserve a durable resume handoff even when the stop is observed at a
	// task boundary before ensureFinished has entered its continuation loop.
	maxContinuationTurns := 0
	if c.session != nil {
		maxContinuationTurns = c.session.Config.MaxCoordinatorTurns
	}
	c.saveContinuationCheckpoint(counters.Turns, maxContinuationTurns, reason, "pending")
	return true, reason
}

// wouldTrip reports whether any counter would have hit a threshold (first or
// second) under hard enforcement. Used by warn-only mode to decide whether to
// emit a warning event.
func wouldTrip(counters NoProgressCounters, limits NoProgressLimits) bool {
	if limits.MaxTokens > 0 && counters.Tokens >= limits.MaxTokens {
		return true
	}
	if limits.MaxTurns > 0 && counters.Turns >= limits.MaxTurns {
		return true
	}
	if limits.MaxTasks > 0 && counters.Tasks >= limits.MaxTasks {
		return true
	}
	return false
}
