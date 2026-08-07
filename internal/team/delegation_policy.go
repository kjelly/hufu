package team

import (
	"fmt"
	"strings"
)

// validateDelegationPolicy rejects configured dispatch violations before a
// TODO is created or a worker can start. A rejected batch is returned to the
// coordinator as a normal tool error and the existing independent tasks keep
// their terminal state.
func (c *Coordinator) validateDelegationPolicy(tasks []TaskDef) error {
	if c == nil || c.session == nil || c.taskTracker == nil {
		return nil
	}
	policy := c.session.Config.Delegation
	items := c.taskTracker.TodoList().Items()
	if policy.RequireExactInitialBatch && len(items) == 0 && !sameAgentSequence(tasks, policy.InitialBatch) {
		return c.rejectDelegationPolicy(fmt.Sprintf(
			"first delegation must contain exactly the configured ordered workers %s; received %s",
			formatAgentNames(policy.InitialBatch), formatTaskAgents(tasks)))
	}

	if len(policy.NoRedispatchAfterSuccess) == 0 {
		return nil
	}
	successful := make(map[string]bool)
	protected := make(map[string]bool, len(policy.NoRedispatchAfterSuccess))
	for _, name := range policy.NoRedispatchAfterSuccess {
		protected[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, item := range items {
		if item != nil && item.Status == TaskDone && protected[strings.ToLower(item.Agent)] {
			successful[strings.ToLower(item.Agent)] = true
		}
	}
	var duplicates []string
	for _, task := range tasks {
		if name := strings.ToLower(task.Agent); successful[name] {
			duplicates = append(duplicates, task.Agent)
		}
	}
	if len(duplicates) > 0 {
		return c.rejectDelegationPolicy(fmt.Sprintf(
			"workers with successful terminal results may not be redispatched in this team: %s",
			formatAgentNames(duplicates)))
	}
	return nil
}

func (c *Coordinator) rejectDelegationPolicy(message string) error {
	// policy_decision is persisted by the normal event pipeline, providing
	// terminal evidence without changing prior task state or cancelling work.
	c.report(c.newEvent("policy_decision").withMessage("delegation rejected: " + message))
	return fmt.Errorf("delegation policy violation: %s", message)
}

func sameAgentSequence(tasks []TaskDef, want []string) bool {
	if len(tasks) != len(want) {
		return false
	}
	for i, task := range tasks {
		if !strings.EqualFold(strings.TrimSpace(task.Agent), strings.TrimSpace(want[i])) {
			return false
		}
	}
	return true
}

func formatTaskAgents(tasks []TaskDef) string {
	names := make([]string, 0, len(tasks))
	for _, task := range tasks {
		names = append(names, task.Agent)
	}
	return formatAgentNames(names)
}

func formatAgentNames(names []string) string {
	return "[" + strings.Join(names, ", ") + "]"
}
