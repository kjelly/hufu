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
	initialPending := c.initialDelegationPending()
	if initialPending && !c.initialDelegationAttempted.CompareAndSwap(false, true) {
		return c.rejectDelegationPolicy("the configured initial delegation was already attempted; start a new run instead of re-dispatching it")
	}
	if len(policy.AllowedWorkers) > 0 {
		allowed := make(map[string]bool, len(policy.AllowedWorkers))
		for _, name := range policy.AllowedWorkers {
			allowed[strings.ToLower(strings.TrimSpace(name))] = true
		}
		var forbidden []string
		for _, task := range tasks {
			name := strings.ToLower(strings.TrimSpace(task.Agent))
			if !allowed[name] {
				forbidden = append(forbidden, task.Agent)
			}
		}
		if len(forbidden) > 0 {
			return c.rejectDelegationPolicy(fmt.Sprintf(
				"delegated worker(s) are outside the configured allowlist %s: %s",
				formatAgentNames(policy.AllowedWorkers), formatAgentNames(forbidden)))
		}
	}
	if initialPending && !sameAgentSequence(tasks, policy.InitialBatch) {
		return c.rejectDelegationPolicy(fmt.Sprintf(
			"canonical delegation phase is initial_pending; first delegation must contain exactly the configured ordered workers %s; received %s",
			formatAgentNames(policy.InitialBatch), formatTaskAgents(tasks)))
	}

	if len(policy.NoRedispatchAfterSuccess) == 0 {
		return c.validateContextFilePolicy(tasks)
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
	return c.validateContextFilePolicy(tasks)
}

func (c *Coordinator) validateContextFilePolicy(tasks []TaskDef) error {
	if c == nil || c.session == nil || !c.session.Config.Delegation.ForbidContextFiles {
		return nil
	}
	for _, task := range tasks {
		if len(task.ContextFiles) > 0 {
			return c.rejectDelegationPolicy(fmt.Sprintf(
				"context_files are forbidden by this team's delegation policy (agent %q)", task.Agent))
		}
	}
	return nil
}

// initialDelegationPending reports the only state in which the coordinator
// must dispatch the configured exact first batch. It is deliberately derived
// solely from the durable delegation phase, never from task prose, STM/LTM,
// vector memory, or conversation history.
func (c *Coordinator) initialDelegationPending() bool {
	return c != nil && c.session != nil && c.session.Config.Delegation.RequireExactInitialBatch && c.delegationPhase() == DelegationPhaseInitialPending
}

// delegationPhase returns the only state allowed to control an exact initial
// batch. Session history and memory are intentionally excluded: they can be
// retained after archive or corruption and are not execution receipts. Legacy
// session files derive active state from restored TODOs, then persist the
// explicit phase on the next checkpoint.
func (c *Coordinator) delegationPhase() DelegationPhase {
	if c == nil || c.session == nil || !c.session.Config.Delegation.RequireExactInitialBatch {
		return DelegationPhaseActive
	}
	if c.sessionData != nil {
		switch c.sessionData.DelegationPhase {
		case DelegationPhaseInitialPending, DelegationPhaseActive:
			return c.sessionData.DelegationPhase
		}
	}
	if c.taskTracker != nil && len(c.taskTracker.TodoList().Items()) > 0 {
		return DelegationPhaseActive
	}
	return DelegationPhaseInitialPending
}

// markInitialDelegationAccepted advances the durable phase immediately before
// the first TODO batch is created. The TodoList callback then checkpoints both
// the phase and the new tasks atomically from the coordinator's perspective.
func (c *Coordinator) markInitialDelegationAccepted() {
	if c == nil || c.session == nil || !c.session.Config.Delegation.RequireExactInitialBatch || c.delegationPhase() != DelegationPhaseInitialPending {
		return
	}
	if c.sessionData != nil {
		c.sessionData.DelegationPhase = DelegationPhaseActive
	}
}

// bindInitialTaskContracts compiles the configured first batch into the only
// execution/output contract that can reach a worker. A coordinator may supply
// a goal, but it may not silently replace any machine-enforced contract field.
// Rejecting conflicts here happens before TODO creation, model calls, or retry
// accounting.
func (c *Coordinator) bindInitialTaskContracts(tasks []TaskDef) ([]TaskDef, error) {
	if c == nil || c.session == nil || c.taskTracker == nil || !c.session.Config.Delegation.BindInitialTaskContracts {
		return tasks, nil
	}
	policy := c.session.Config.Delegation
	items := c.taskTracker.TodoList().Items()
	if policy.RequireExactInitialBatch {
		if c.delegationPhase() != DelegationPhaseInitialPending {
			return tasks, nil
		}
	} else if !isUnseenInitialContractBatch(tasks, items, policy.InitialBatch) {
		return tasks, nil
	}
	bound, _, err := CompileInitialTaskContracts(c.session, tasks)
	return bound, err
}

func isUnseenInitialContractBatch(tasks []TaskDef, items []*TodoItem, initialBatch []string) bool {
	if len(tasks) == 0 || len(initialBatch) == 0 {
		return false
	}
	initial := make(map[string]bool, len(initialBatch))
	for _, name := range initialBatch {
		initial[strings.ToLower(strings.TrimSpace(name))] = true
	}
	seen := make(map[string]bool, len(items)+len(tasks))
	for _, item := range items {
		if item == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(item.Agent))
		if !initial[name] {
			return false
		}
		seen[name] = true
	}
	for _, task := range tasks {
		name := strings.ToLower(strings.TrimSpace(task.Agent))
		if !initial[name] || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

// serializeMutationTasks turns an accidentally batched set of state-changing
// tasks into an explicit DAG wave. The model semaphore controls inference
// load, not target/workspace safety; a coordinator can still put two mutation
// tasks in one request_agent call. Read-only tasks remain eligible to run in
// parallel, while every mutation waits for the previous mutation in the same
// batch. This is a generic safety normalization, not a workflow-specific
// adapter.
func (c *Coordinator) serializeMutationTasks(tasks []TaskDef) []TaskDef {
	out := make([]TaskDef, len(tasks))
	copy(out, tasks)
	lastMutation := -1
	for i := range out {
		switch c.effectiveSideEffect(out[i]) {
		case SideEffectWorkspaceWrite, SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential:
			if lastMutation >= 0 && !containsInt(out[i].DependsOn, lastMutation) {
				out[i].DependsOn = append(append([]int(nil), out[i].DependsOn...), lastMutation)
			}
			lastMutation = i
		}
	}
	return out
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
