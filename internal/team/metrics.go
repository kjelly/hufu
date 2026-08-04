package team

import (
	"strconv"
	"time"
)

// Metrics returns a copy of the coordinator's reliability counters.
func (c *Coordinator) Metrics() RunMetrics {
	if c == nil {
		return RunMetrics{}
	}
	c.metricsMu.RLock()
	byClass := make(map[TaskFailureClass]int, len(c.retriesByFailureClass))
	for class, count := range c.retriesByFailureClass {
		byClass[class] = count
	}
	repairCounts := make(map[string]int, len(c.antiThrashing.RepairsByCriterion))
	for id, count := range c.antiThrashing.RepairsByCriterion {
		repairCounts[id] = count
	}
	lastStrategies := make(map[string]RecoveryStrategy, len(c.antiThrashing.LastStrategy))
	for fp, strategy := range c.antiThrashing.LastStrategy {
		lastStrategies[fp] = strategy
	}
	metrics := RunMetrics{RetriesByFailureClass: byClass, Compactions: c.compactions,
		FailuresByClass: make(map[TaskFailureClass]int), FailuresByPhase: make(map[string]int),
		RetryAttemptsAvoidedByDisposition: make(map[RetryDisposition]int),
		ProtocolRepairFailuresByReason:    make(map[RepairFailureReason]int),
		RepeatedFailureFingerprints:       repeatedFingerprintCount(c.antiThrashing.Counts),
		SystemicFingerprintsEscalated:     c.antiThrashing.SystemicEscalations,
		RecoveryStrategyChanges:           c.antiThrashing.StrategyChanges,
		LastRecoveryStrategies:            lastStrategies,
		DiagnosticTasksSinceProgress:      c.antiThrashing.DiagnosticSinceProgress,
		RepairAttemptsByCriterion:         repairCounts, AntiThrashingWarnings: c.antiThrashing.Warnings,
		PreflightFailuresCaught: c.preflightFailuresCaught, NonAssertingVerifiersRejected: c.nonAssertingVerifiersRejected}
	metrics.RepeatedFailureFingerprintsStopped = metrics.RepeatedFailureFingerprints
	metrics.TokensSinceCriterionProgress = c.tokensSinceCriterionProgress
	// No-progress budget counters (§8.1, WP-12). Read under the same lock.
	metrics.TurnsSinceCriterionProgress = c.turnsSinceCriterionProgress
	metrics.TasksSinceCriterionProgress = c.tasksSinceCriterionProgress
	c.metricsMu.RUnlock()
	// No-progress budget configured limits (§8.1, WP-12). 0 = disabled.
	rc := c.reliabilityConfig()
	metrics.MaxTokensWithoutProgress = int64(rc.MaxTokensWithoutProgress)
	metrics.MaxTurnsWithoutProgress = rc.MaxTurnsWithoutProgress
	metrics.MaxTasksWithoutProgress = rc.MaxTasksWithoutProgress
	if c.sessionData != nil {
		for _, result := range c.sessionData.CriterionResults {
			if result.State == CriterionPassed {
				metrics.AcceptanceCriteriaPassed++
			}
		}
	}
	metrics.TasksByCriterion = make(map[string]int)
	if c.taskTracker != nil {
		accumulateTodoMetrics(&metrics, c.taskTracker.TodoList().Items())
		accumulateFailureEventMetrics(&metrics, c.failureEventsForMetrics(c.taskTracker.TodoList().Items()))
	}
	if metrics.TasksWithVerifier > 0 {
		metrics.TypedVerifierAdoptionRate = float64(metrics.TypedVerifiers) / float64(metrics.TasksWithVerifier)
	}
	if c.sessionData != nil && c.sessionData.LastCriterionProgressAt != "" {
		if last, err := time.Parse(time.RFC3339Nano, c.sessionData.LastCriterionProgressAt); err == nil && !last.IsZero() {
			metrics.TimeSinceCriterionProgressSeconds = int64(time.Since(last).Seconds())
		}
	}
	return metrics
}

func accumulateTodoMetrics(metrics *RunMetrics, items []*TodoItem) {
	for _, item := range items {
		if item == nil {
			continue
		}
		for _, criterionID := range item.Advances {
			metrics.TasksByCriterion[criterionID]++
		}
		if item.Verify != "" || item.VerifySpec != nil {
			metrics.TasksWithVerifier++
			if item.VerifySpec != nil {
				metrics.TypedVerifiers++
			}
		} else if item.Status == TaskDone {
			metrics.TasksDoneWithoutObjectiveVerifier++
		}
		accumulateVerificationMetrics(metrics, item)
		accumulateStepBudgetMetrics(metrics, item)
		for _, receipt := range item.ExecutionReceipts {
			accumulateProtocolRepairMetrics(metrics, receipt.RepairProvenance)
		}
		metrics.ReplayAttempts += item.Retries
		if item.RecoveryState != "" && item.RecoveryState != RecoveryStateNotStarted {
			metrics.ReconciliationAttempts++
			if item.RecoveryState == RecoveryStateComplete {
				metrics.ReconciliationSucceeded++
			}
		}
		// A replay is unsafe only when a task with a non-replayable side
		// effect was redriven while its prior operation was partial or
		// unknown. This is an explicit task/recovery fact, not an inference
		// from avoided-retry or repeated-failure counters.
		if item.Retries > 0 && nonReplayableSideEffect(item.SideEffect) &&
			(item.RecoveryState == RecoveryStatePartial || item.RecoveryState == RecoveryStateUnknown) &&
			(item.Execution.AllowsReplay == nil || *item.Execution.AllowsReplay) {
			metrics.UnsafeReplaysDetected += item.Retries
		}
	}
}

func (c *Coordinator) failureEventsForMetrics(items []*TodoItem) []*FailureEventPayload {
	if c != nil && c.eventStore != nil {
		events, err := c.eventStore.ReadEvents()
		if err == nil {
			failures := make([]*FailureEventPayload, 0)
			for _, event := range events {
				// The event store is append-only across coordinator runs. A
				// reliability snapshot must describe this run, not historical
				// failures from the same workspace. Leave unscoped reads intact
				// for callers reconstructing legacy stores without an active run.
				if c.executionRunID != "" && event.RunID != c.executionRunID {
					continue
				}
				switch event.Type {
				case "task_failed", "task_blocked", "task_protocol_incomplete":
					failure, present := mergeFailureEventJSON(nil, event.Payload)
					if present && failure != nil {
						failures = append(failures, failure)
					}
				}
			}
			return failures
		}
	}
	failures := make([]*FailureEventPayload, 0, len(items))
	for _, item := range items {
		if item != nil && item.FailureEvent != nil {
			failures = append(failures, item.FailureEvent)
		}
	}
	return failures
}

func accumulateFailureEventMetrics(metrics *RunMetrics, failures []*FailureEventPayload) {
	for _, failure := range failures {
		if failure == nil {
			continue
		}
		metrics.FailuresByClass[failure.FailureClass]++
		if failure.Phase != "" {
			metrics.FailuresByPhase[failure.Phase]++
		}
		if failure.RetryDisposition != RetryWorker && failure.RetryDisposition != RetryNone {
			metrics.RetryAttemptsAvoidedByDisposition[failure.RetryDisposition]++
		}
		if failure.FailureClass == FailureCancelled {
			metrics.CancelledTasksExcludedFromRetries++
		}
	}
}

func accumulateTimeoutRecoveryMetrics(metrics *RunMetrics, item *TodoItem) {
	if item.FailureEvent != nil && item.FailureEvent.FailureClass == FailureTimeout && item.Resolution != nil && item.Resolution.Status == "reconciled" {
		metrics.TimeoutTasksRecovered++
	}
}

func accumulateVerificationMetrics(metrics *RunMetrics, item *TodoItem) {
	receiptAttempts := make(map[string]bool)
	receiptResults := make(map[string]bool)
	for index, receipt := range item.ExecutionReceipts {
		if receipt.VerifyResult == nil {
			continue
		}
		attemptKey := receipt.RunID + "\x00" + receipt.TaskID + "\x00" + strconv.Itoa(receipt.Attempt)
		if receipt.RunID == "" && receipt.TaskID == "" && receipt.Attempt == 0 {
			attemptKey = "receipt-index:" + strconv.Itoa(index)
		}
		if receiptAttempts[attemptKey] {
			continue
		}
		receiptAttempts[attemptKey] = true
		receiptResults[verificationMetricKey(receipt.VerifyResult)] = true
		accumulateVerificationResultMetrics(metrics, receipt.VerifyResult, true)
	}
	if item.VerifyResult != nil {
		key := verificationMetricKey(item.VerifyResult)
		if !receiptResults[key] {
			accumulateVerificationResultMetrics(metrics, item.VerifyResult, item.Status == TaskError)
		}
	}
	accumulateTimeoutRecoveryMetrics(metrics, item)
	if item.RecoveryState != "" && item.RecoveryState != RecoveryStateNotStarted && item.Status != TaskDone {
		metrics.ExecutionReplaysAvoided++
	}
}

func verificationMetricKey(result *VerificationResult) string {
	if result == nil {
		return ""
	}
	return result.Command + "\x00" + result.WorkDir + "\x00" + strconv.Itoa(result.ExitCode) + "\x00" + result.Fingerprint + "\x00" + result.EvaluatedAt.UTC().Format(time.RFC3339Nano) + "\x00" + result.OverturnReason
}

func accumulateVerificationResultMetrics(metrics *RunMetrics, result *VerificationResult, rejected bool) {
	if result.WeakWarning {
		metrics.WeakVerifierWarnings++
	}
	if rejected && result.ExitCode != 0 {
		metrics.WorkerSuccessRejected++
	}
	if result.Overturned {
		metrics.VerificationsOverturned++
	}
}

// accumulateStepBudgetMetrics counts attempts that were cut off by the step
// budget. These land in the run's failure counters as protocol failures — the
// worker did omit its result — so without a separate counter a run whose tasks
// simply needed more tool calls is indistinguishable from one whose model
// ignored the result contract, and the reported diagnosis points at the wrong
// fix. Attempts are deduped by (run, task, attempt) the same way verification
// metrics are, so a replayed receipt is not counted twice.
func accumulateStepBudgetMetrics(metrics *RunMetrics, item *TodoItem) {
	seen := make(map[string]bool)
	for index, receipt := range item.ExecutionReceipts {
		if receipt.StepBudget == nil || !receipt.StepBudget.Exhausted {
			continue
		}
		key := receipt.RunID + "\x00" + receipt.TaskID + "\x00" + strconv.Itoa(receipt.Attempt)
		if receipt.RunID == "" && receipt.TaskID == "" && receipt.Attempt == 0 {
			key = "receipt-index:" + strconv.Itoa(index)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		metrics.StepBudgetExhaustions++
	}
}

func accumulateProtocolRepairMetrics(metrics *RunMetrics, provenance *RepairProvenance) {
	if provenance == nil || !provenance.Attempted {
		return
	}
	metrics.ProtocolRepairsAttempted += protocolRepairAttemptCount(provenance)
	if provenance.Success {
		metrics.ProtocolRepairsSucceeded++
	}
	for _, attempt := range provenance.History {
		if attempt.FailureReason != "" {
			metrics.ProtocolRepairFailuresByReason[attempt.FailureReason]++
		}
	}
	if len(provenance.History) == 0 && provenance.FailureReason != "" {
		metrics.ProtocolRepairFailuresByReason[provenance.FailureReason]++
	}
}

// protocolRepairAttemptCount counts only repair turns that remain protocol
// failures. A progress_not_final turn is execution evidence and is excluded,
// even when it follows an earlier invalid_schema repair in the same receipt.
func protocolRepairAttemptCount(provenance *RepairProvenance) int {
	if provenance == nil || !provenance.Attempted {
		return 0
	}
	if len(provenance.History) == 0 {
		if provenance.FailureReason == RepairFailureProgressNotFinal {
			return 0
		}
		attempts := provenance.RepairAttempts
		if attempts < 1 {
			return 1
		}
		return attempts
	}
	count := 0
	for _, attempt := range provenance.History {
		if attempt.FailureReason != RepairFailureProgressNotFinal {
			count++
		}
	}
	return count
}

func (c *Coordinator) recordReliabilityUsage(taskID string, attempt, tokens int) {
	if c == nil || taskID == "" || attempt < 1 || tokens <= 0 {
		return
	}
	owner := c
	if c.noProgressUsageOwner != nil {
		owner = c.noProgressUsageOwner
		if c.noProgressUsageNamespace != "" {
			taskID = c.noProgressUsageNamespace + ":" + taskID
		}
	}
	owner.metricsMu.Lock()
	if owner.reliabilityUsageByAttempt == nil {
		owner.reliabilityUsageByAttempt = make(map[string]int)
	}
	key := taskID + ":" + strconv.Itoa(attempt)
	prior := owner.reliabilityUsageByAttempt[key]
	if tokens > prior {
		owner.tokensSinceCriterionProgress += int64(tokens - prior)
		owner.reliabilityUsageByAttempt[key] = tokens
	}
	owner.metricsMu.Unlock()
}

// recordNoProgressTokens adds token usage that is not associated with a
// worker execution receipt, such as coordinator model turns. Worker usage is
// accounted by recordReliabilityUsage so its cumulative planned/done events
// are not double-counted.
func (c *Coordinator) recordNoProgressTokens(tokens int64) {
	if c == nil || tokens <= 0 {
		return
	}
	if c.noProgressUsageOwner != nil {
		c.noProgressUsageOwner.recordNoProgressTokens(tokens)
		return
	}
	c.metricsMu.Lock()
	c.tokensSinceCriterionProgress += tokens
	c.metricsMu.Unlock()
}

func repeatedFingerprintCount(counts map[string]int) int {
	n := 0
	for _, count := range counts {
		if count >= 2 {
			n++
		}
	}
	return n
}

func (c *Coordinator) recordRetry(class TaskFailureClass) {
	if c == nil || IsCancelledClass(class) {
		return
	}
	c.metricsMu.Lock()
	if c.retriesByFailureClass == nil {
		c.retriesByFailureClass = make(map[TaskFailureClass]int)
	}
	c.retriesByFailureClass[class]++
	c.metricsMu.Unlock()
}

func (c *Coordinator) recordCompaction() {
	if c == nil {
		return
	}
	c.metricsMu.Lock()
	c.compactions++
	c.metricsMu.Unlock()
}
