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
	defer c.metricsMu.RUnlock()
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
		RepeatedFailureFingerprints:  repeatedFingerprintCount(c.antiThrashing.Counts),
		RecoveryStrategyChanges:      c.antiThrashing.StrategyChanges,
		LastRecoveryStrategies:       lastStrategies,
		DiagnosticTasksSinceProgress: c.antiThrashing.DiagnosticSinceProgress,
		RepairAttemptsByCriterion:    repairCounts, AntiThrashingWarnings: c.antiThrashing.Warnings}
	metrics.TokensSinceCriterionProgress = c.tokensSinceCriterionProgress
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
				if receipt.RepairProvenance != nil && receipt.RepairProvenance.Attempted {
					metrics.ProtocolRepairsAttempted++
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

func (c *Coordinator) recordReliabilityUsage(taskID string, attempt, tokens int) {
	if c == nil || taskID == "" || attempt < 1 || tokens <= 0 {
		return
	}
	c.metricsMu.Lock()
	if c.reliabilityUsageByAttempt == nil {
		c.reliabilityUsageByAttempt = make(map[string]int)
	}
	key := taskID + ":" + strconv.Itoa(attempt)
	prior := c.reliabilityUsageByAttempt[key]
	if tokens > prior {
		c.tokensSinceCriterionProgress += int64(tokens - prior)
		c.reliabilityUsageByAttempt[key] = tokens
	}
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
	if c == nil {
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
