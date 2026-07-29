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
	return RunMetrics{RetriesByFailureClass: byClass, Compactions: c.compactions}
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
