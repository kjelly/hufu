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
		RepeatedFailureFingerprints:   repeatedFingerprintCount(c.antiThrashing.Counts),
		SystemicFingerprintsEscalated: c.antiThrashing.SystemicEscalations,
		RecoveryStrategyChanges:       c.antiThrashing.StrategyChanges,
		LastRecoveryStrategies:        lastStrategies,
		DiagnosticTasksSinceProgress:  c.antiThrashing.DiagnosticSinceProgress,
		RepairAttemptsByCriterion:     repairCounts, AntiThrashingWarnings: c.antiThrashing.Warnings}
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
		for _, item := range c.taskTracker.TodoList().Items() {
			if item == nil {
				continue
			}
			for _, criterionID := range item.Advances {
				metrics.TasksByCriterion[criterionID]++
			}
			if item.VerifyResult != nil && item.VerifyResult.WeakWarning {
				metrics.WeakVerifierWarnings++
			}
			if item.Status == TaskError && item.VerifyResult != nil && item.VerifyResult.ExitCode != 0 {
				metrics.WorkerSuccessRejected++
			}
			if item.RecoveryState != "" && item.RecoveryState != RecoveryStateNotStarted && item.Status != TaskDone {
				metrics.ExecutionReplaysAvoided++
			}
			for _, receipt := range item.ExecutionReceipts {
				// A progress submission is evidence that execution is incomplete,
				// not a protocol-repair failure. Keep it out of the protocol
				// repair counters (§7); successful repairs and legacy receipts
				// without a reason remain counted.
				if receipt.RepairProvenance != nil && receipt.RepairProvenance.Attempted {
					metrics.ProtocolRepairsAttempted += protocolRepairAttemptCount(receipt.RepairProvenance)
					if receipt.RepairProvenance.Success {
						metrics.ProtocolRepairsSucceeded++
					}
				}
			}
		}
	}
	if c.sessionData != nil && c.sessionData.LastCriterionProgressAt != "" {
		if last, err := time.Parse(time.RFC3339Nano, c.sessionData.LastCriterionProgressAt); err == nil && !last.IsZero() {
			metrics.TimeSinceCriterionProgressSeconds = int64(time.Since(last).Seconds())
		}
	}
	return metrics
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
