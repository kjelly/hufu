package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
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

	if err := c.validateTaskGoalInvariants(tasks); err != nil {
		return err
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

// validateTaskGoalInvariants rejects a selected task goal before TODO creation
// or worker startup.  The comparison is intentionally literal and generic;
// teams own the domain-specific selector and payload in their YAML.
func (c *Coordinator) validateTaskGoalInvariants(tasks []TaskDef) error {
	if c == nil || c.session == nil {
		return nil
	}
	for taskIndex, task := range tasks {
		agentName := strings.ToLower(strings.TrimSpace(task.Agent))
		for invariantIndex, invariant := range c.session.Config.Delegation.TaskGoalInvariants {
			if agentName != strings.ToLower(strings.TrimSpace(invariant.Agent)) || !strings.Contains(task.Goal, invariant.WhenGoalContains) {
				continue
			}
			for _, required := range invariant.RequiredLiterals {
				if !strings.Contains(task.Goal, required) {
					return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].goal violates task-goal-invariants[%d]: required literal is missing", taskIndex, invariantIndex))
				}
			}
			for _, forbidden := range invariant.ForbiddenLiterals {
				if strings.Contains(task.Goal, forbidden) {
					return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].goal violates task-goal-invariants[%d]: forbidden literal is present", taskIndex, invariantIndex))
				}
			}
			if invariant.RequiredTaskReference != nil {
				if err := c.validateTaskGoalReference(task.Goal, *invariant.RequiredTaskReference); err != nil {
					return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].goal violates task-goal-invariants[%d]: %v", taskIndex, invariantIndex, err))
				}
			}
			seenReferences := make(map[string]bool, len(invariant.RequiredTaskReferences))
			for _, reference := range invariant.RequiredTaskReferences {
				if err := c.validateTaskGoalReference(task.Goal, reference); err != nil {
					return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].goal violates task-goal-invariants[%d]: %v", taskIndex, invariantIndex, err))
				}
				id := taskGoalReferenceValue(task.Goal, reference.GoalPrefix)
				if seenReferences[id] {
					return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].goal violates task-goal-invariants[%d]: required task references must name distinct completed Todos", taskIndex, invariantIndex))
				}
				seenReferences[id] = true
			}
			if len(invariant.RequiredToolSequence) > 0 && !sameStringSequence(task.Execution.ToolSequence, invariant.RequiredToolSequence) {
				return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].execution.tool_sequence violates task-goal-invariants[%d]: required exact tool sequence is missing", taskIndex, invariantIndex))
			}
			for _, field := range invariant.ForbiddenExecutionFields {
				if executionContractFieldPresent(task.Execution, field) {
					return c.rejectDelegationPolicy(fmt.Sprintf("tasks[%d].execution.%s violates task-goal-invariants[%d]: forbidden execution field is present", taskIndex, field, invariantIndex))
				}
			}
		}
	}
	return nil
}

func taskGoalReferenceValue(goal, prefix string) string {
	prefix = strings.TrimSpace(prefix)
	for _, line := range strings.Split(goal, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

func (c *Coordinator) validateTaskGoalReference(goal string, reference agent.TaskGoalReference) error {
	prefix := strings.TrimSpace(reference.GoalPrefix)
	if prefix == "" {
		return fmt.Errorf("required task reference has an empty goal prefix")
	}
	var values []string
	for _, line := range strings.Split(goal, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			values = append(values, strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		}
	}
	if len(values) != 1 || values[0] == "" {
		return fmt.Errorf("required task reference %q must occur exactly once with a non-empty Todo ID", prefix)
	}
	taskID := values[0]
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ID != taskID {
			continue
		}
		if item.Status != TaskDone {
			return fmt.Errorf("referenced Todo %q is %s, not done", taskID, item.Status)
		}
		if !strings.EqualFold(strings.TrimSpace(item.Agent), strings.TrimSpace(reference.Agent)) {
			return fmt.Errorf("referenced Todo %q was produced by %q, not %q", taskID, item.Agent, reference.Agent)
		}
		if !strings.Contains(item.Desc, reference.TaskContains) {
			return fmt.Errorf("referenced Todo %q does not match the required task selector", taskID)
		}
		return nil
	}
	return fmt.Errorf("referenced Todo %q does not exist", taskID)
}

func sameStringSequence(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// executionContractFieldPresent deliberately recognizes only configured
// ExecutionContract fields. Team validation rejects unknown field names so a
// typo cannot silently weaken a pre-dispatch boundary.
func executionContractFieldPresent(contract ExecutionContract, field string) bool {
	switch field {
	case "kind":
		return contract.Kind != ""
	case "requires_result":
		return contract.RequiresResult
	case "requires_verification":
		return contract.RequiresVerification
	case "allows_replay":
		return contract.AllowsReplay != nil
	case "forbid_artifacts":
		return contract.ForbidArtifacts
	case "steps":
		return len(contract.Steps) > 0
	case "tool_sequence":
		return len(contract.ToolSequence) > 0
	case "tool_input_sequence":
		return len(contract.ToolInputSequence) > 0
	case "tool_input_canonical_sequence":
		return len(contract.ToolInputCanonicalSequence) > 0
	case "tool_input_transform_sequence":
		return len(contract.ToolInputTransformSequence) > 0
	case "tool_input_field":
		return contract.ToolInputField != ""
	case "tool_input_value_sequence":
		return len(contract.ToolInputValueSequence) > 0
	case "tool_expected_exit_codes":
		return len(contract.ToolExpectedExitCodes) > 0
	default:
		return false
	}
}

func knownExecutionContractField(field string) bool {
	switch field {
	case "kind", "requires_result", "requires_verification", "allows_replay", "forbid_artifacts", "steps", "tool_sequence", "tool_input_sequence", "tool_input_canonical_sequence", "tool_input_transform_sequence", "tool_input_field", "tool_input_value_sequence", "tool_expected_exit_codes":
		return true
	default:
		return false
	}
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
	if (c.taskTracker != nil && len(c.taskTracker.TodoList().Items()) > 0) || (c.sessionData != nil && len(c.sessionData.Tasks) > 0) {
		return DelegationPhaseActive
	}
	if c.sessionData != nil {
		switch c.sessionData.DelegationPhase {
		case DelegationPhaseInitialPending, DelegationPhaseActive:
			return c.sessionData.DelegationPhase
		}
	}
	return DelegationPhaseInitialPending
}

// markInitialDelegationAccepted advances the durable phase immediately before
// the first TODO batch is created. The TodoList callback then checkpoints both
// the phase and the new tasks atomically from the coordinator's perspective.
// It returns true when it advanced the phase, so a caller that subsequently
// fails to create any task can revert the in-memory advance and keep the
// initial-batch policy intact for a retry.
func (c *Coordinator) markInitialDelegationAccepted() bool {
	if c == nil || c.session == nil || !c.session.Config.Delegation.RequireExactInitialBatch || c.delegationPhase() != DelegationPhaseInitialPending {
		return false
	}
	if c.sessionData != nil {
		c.sessionData.DelegationPhase = DelegationPhaseActive
	}
	return true
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

// bindTaskGoalContracts applies an opt-in, goal-selected static contract to a
// later dispatch. The template is authoritative so generic coordinator schema
// defaults cannot leak into a dynamic closed sequence.
func (c *Coordinator) bindTaskGoalContracts(tasks []TaskDef) ([]TaskDef, error) {
	if c == nil || c.session == nil || c.taskTracker == nil {
		return tasks, nil
	}
	bound, _, err := CompileTaskGoalContracts(c.session, tasks)
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
