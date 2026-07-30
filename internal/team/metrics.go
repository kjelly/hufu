package team

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
	return RunMetrics{RetriesByFailureClass: byClass, Compactions: c.compactions,
		RepeatedFailureFingerprints:  repeatedFingerprintCount(c.antiThrashing.Counts),
		RecoveryStrategyChanges:      c.antiThrashing.StrategyChanges,
		LastRecoveryStrategies:       lastStrategies,
		DiagnosticTasksSinceProgress: c.antiThrashing.DiagnosticSinceProgress,
		RepairAttemptsByCriterion:    repairCounts, AntiThrashingWarnings: c.antiThrashing.Warnings}
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
