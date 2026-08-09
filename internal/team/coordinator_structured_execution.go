package team

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// SetStructuredStepRunner replaces the default exact-tool adapter. This is the
// extension point for generative producer/repair workers; lifecycle ordering,
// mutation freeze, receipts, and submit-result validation remain coordinator
// owned.
func (c *Coordinator) SetStructuredStepRunner(runner StructuredStepRunner) {
	if c == nil {
		return
	}
	c.taskAttemptsMu.Lock()
	c.structuredStepRunner = runner
	c.taskAttemptsMu.Unlock()
}

func (c *Coordinator) configuredStructuredStepRunner() StructuredStepRunner {
	if c == nil {
		return nil
	}
	c.taskAttemptsMu.RLock()
	runner := c.structuredStepRunner
	c.taskAttemptsMu.RUnlock()
	return runner
}

// RunStructuredTask executes one TodoItem's structured contract through the
// coordinator-owned receipt registry. The same registry backs submit_result,
// so a later success claim can be accepted only if it cites the complete
// lifecycle actually run here.
func (c *Coordinator) RunStructuredTask(ctx context.Context, todoID string, attempt int, runner StructuredStepRunner) (*StructuredExecutionResult, error) {
	if c == nil {
		return nil, fmt.Errorf("coordinator is nil")
	}
	if strings.TrimSpace(todoID) == "" {
		return nil, fmt.Errorf("structured task todo id is required")
	}
	contract := c.executionContractForTask(todoID)
	if len(contract.Steps) == 0 {
		return nil, fmt.Errorf("todo %q has no structured execution contract", todoID)
	}
	if attempt <= 0 {
		return nil, fmt.Errorf("structured task attempt must be positive")
	}
	item := c.todoItemByID(todoID)
	task := TaskDef{Execution: contract}
	if item != nil {
		task.Agent = item.Agent
	}
	return c.runStructuredTask(ctx, todoID, attempt, runner, task)
}

func (c *Coordinator) runStructuredTask(ctx context.Context, todoID string, attempt int, runner StructuredStepRunner, task TaskDef) (*StructuredExecutionResult, error) {
	c.setCurrentTaskAttempt(todoID, attempt)
	contract := task.Execution
	upstream, err := c.resolveStructuredUpstreamOutputs(todoID, contract)
	if err != nil {
		return nil, err
	}
	item := c.todoItemByID(todoID)
	var agentDef *agent.AgentDef
	if item != nil && strings.TrimSpace(item.Agent) != "" && c.session != nil && len(c.session.Agents) > 0 {
		agentDef, _, err = c.AgentPool().ResolveAgentName(item.Agent)
		if err != nil {
			return nil, fmt.Errorf("resolve structured task agent %q: %w", item.Agent, err)
		}
	}
	return RunStructuredExecution(ctx, StructuredExecutionRequest{
		TaskID: todoID, Attempt: attempt, Contract: contract, Registry: c.executionStepReceiptRegistry(), UpstreamOutputs: upstream,
		SelectModel: func(step ExecutionStep, repairAttempt int) string {
			return c.selectStructuredStepModel(task, step, repairAttempt, agentDef)
		},
	}, runner)
}

func (c *Coordinator) executeStructuredCoordinatorTask(ctx context.Context, task TaskDef, todoID string) (string, error) {
	runner := c.configuredStructuredStepRunner()
	if runner == nil {
		return "", fmt.Errorf("structured task %q has no configured step runner", todoID)
	}
	if err := c.validateTaskModel(&task); err != nil {
		return "", err
	}
	var agentDef *agent.AgentDef
	if strings.TrimSpace(task.Agent) != "" && c.session != nil && len(c.session.Agents) > 0 {
		resolvedAgent, _, err := c.AgentPool().ResolveAgentName(task.Agent)
		if err != nil {
			return "", err
		}
		agentDef = resolvedAgent
	}
	_, exactRunner := runner.(*coordinatorDeclaredToolRunner)
	if exactRunner && agentDef == nil {
		return "", fmt.Errorf("structured task %q exact-tool runner cannot resolve agent %q", todoID, task.Agent)
	}
	if exactRunner && len(agentDef.MCPTools) > 0 {
		if c.mcpManager == nil {
			return "", fmt.Errorf("structured task %q agent %q requires MCP tools but no MCP manager is configured", todoID, agentDef.Name)
		}
		if err := c.mcpManager.LoadAgentMCPServer(agentDef.Name, agentDef.MCPTools, agentDef.Shell); err != nil {
			return "", fmt.Errorf("load MCP tools for structured task agent %q: %w", agentDef.Name, err)
		}
		defer func() { _ = c.mcpManager.UnloadAgentMCPServer(strings.ToLower(agentDef.Name)) }()
	}
	attempt := c.currentTaskAttempt(todoID) + 1
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskInProgress, "executing structured lifecycle", ""); err != nil {
		return "", err
	}
	result, err := c.runStructuredTask(ctx, todoID, attempt, runner, task)
	if err != nil {
		_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskError, err.Error(), "")
		return "", err
	}
	ids := make([]string, len(result.Receipts))
	for i, receipt := range result.Receipts {
		ids[i] = receipt.ID
	}
	summary := fmt.Sprintf("structured execution completed in state %s with %d receipt(s)", result.State, len(ids))
	typedResult := &TaskResult{
		TaskID: todoID, Agent: task.Agent, Attempt: attempt, Status: TaskResultStatusSuccess,
		Summary: summary, Source: "runtime", ReceiptIDs: ids, Confidence: 1,
		Outputs: structuredTaskOutputs(task.Execution, result),
	}
	if err := c.validateTaskResultReceiptClaims(todoID, typedResult); err != nil {
		_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskError, err.Error(), "")
		return "", err
	}
	c.storeSubmittedTaskResult(todoID, typedResult)
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskDone, summary, summary); err != nil {
		return "", err
	}
	c.recordTerminalTypedTaskResult(todoID)
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	return summary, nil
}

func (c *Coordinator) resolveStructuredUpstreamOutputs(todoID string, contract ExecutionContract) (map[string]map[string]StructuredOutputValue, error) {
	item := c.todoItemByID(todoID)
	if item == nil {
		return nil, fmt.Errorf("structured task %q does not exist", todoID)
	}
	dependencies := make(map[string]bool, len(item.DependsOn))
	for _, dependency := range item.DependsOn {
		dependencies[dependency] = true
	}
	resolved := make(map[string]map[string]StructuredOutputValue)
	for _, step := range contract.Steps {
		for _, reference := range step.References {
			if reference.TaskID == "" {
				continue
			}
			actualTaskID := ""
			var dependencyItem *TodoItem
			for _, candidate := range c.taskTracker.TodoList().Items() {
				if candidate != nil && dependencies[candidate.ID] && (candidate.ID == reference.TaskID || candidate.PlanTaskID == reference.TaskID) {
					actualTaskID = candidate.ID
					dependencyItem = candidate
					break
				}
			}
			if actualTaskID == "" {
				return nil, fmt.Errorf("step %q references task %q which is not a declared dependency", step.ID, reference.TaskID)
			}
			upstream := c.GetTaskResult(actualTaskID)
			if upstream == nil || !taskResultStatusIsSuccessful(upstream.Status) {
				return nil, fmt.Errorf("step %q references task %q before it has a successful result", step.ID, reference.TaskID)
			}
			output, ok := upstream.Outputs[reference.Output]
			if !ok {
				return nil, fmt.Errorf("task %q has no runtime output %q", reference.TaskID, reference.Output)
			}
			if normalizedExecutionOutputKind(output.Kind) != normalizedExecutionOutputKind(reference.Kind) {
				return nil, fmt.Errorf("task %q output %q kind %q does not match reference kind %q", reference.TaskID, reference.Output, output.Kind, reference.Kind)
			}
			if reference.Schema != "" && output.Schema != reference.Schema {
				return nil, fmt.Errorf("task %q output %q schema %q does not match reference schema %q", reference.TaskID, reference.Output, output.Schema, reference.Schema)
			}
			outputScope := output.Scope
			if outputScope == "" {
				outputScope = "task"
			}
			referenceScope := reference.Scope
			if referenceScope == "" {
				referenceScope = "task"
			}
			if outputScope != referenceScope {
				return nil, fmt.Errorf("task %q output %q scope %q does not match reference scope %q", reference.TaskID, reference.Output, outputScope, referenceScope)
			}
			if outputScope == "secret" && (dependencyItem == nil || !strings.EqualFold(strings.TrimSpace(dependencyItem.Agent), strings.TrimSpace(item.Agent))) {
				return nil, fmt.Errorf("task %q output %q is secret-scoped and cannot cross agent identities", reference.TaskID, reference.Output)
			}
			if output.Kind == ExecutionOutputReceipt {
				receipt, exists := c.executionStepReceiptRegistry().Get(output.ReceiptID)
				if !exists || receipt.TaskID != actualTaskID || receipt.ExitCode != 0 {
					return nil, fmt.Errorf("task %q output %q does not reference a successful owned receipt", reference.TaskID, reference.Output)
				}
			}
			if resolved[reference.TaskID] == nil {
				resolved[reference.TaskID] = make(map[string]StructuredOutputValue)
			}
			resolved[reference.TaskID][reference.Output] = output
		}
	}
	return resolved, nil
}

func structuredTaskOutputs(contract ExecutionContract, result *StructuredExecutionResult) map[string]StructuredOutputValue {
	if result == nil {
		return nil
	}
	latestReceipt := make(map[string]ExecutionStepReceipt)
	for _, receipt := range result.Receipts {
		if receipt.ExitCode == 0 {
			latestReceipt[receipt.StepID] = receipt
		}
	}
	outputs := make(map[string]StructuredOutputValue)
	for _, step := range contract.Steps {
		for _, declaration := range step.Outputs {
			value := StructuredOutputValue{Kind: normalizedExecutionOutputKind(declaration.Kind), Schema: declaration.Schema, Scope: declaration.Scope}
			switch value.Kind {
			case ExecutionOutputArtifact:
				if artifact, ok := result.Artifacts[declaration.Name]; ok {
					copyArtifact := artifact
					value.Artifact = &copyArtifact
				}
			case ExecutionOutputFact:
				if fact, ok := result.Facts[declaration.Name]; ok {
					copyFact := fact
					value.Fact = &copyFact
				}
			case ExecutionOutputReceipt:
				value.ReceiptID = latestReceipt[step.ID].ID
			}
			outputs[declaration.Name] = value
		}
	}
	return outputs
}
