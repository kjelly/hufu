package team

import (
	"strconv"
	"strings"
)

func (c *Coordinator) contextRunID() string {
	if c == nil {
		return ""
	}
	if c.executionRunID != "" {
		return c.executionRunID
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		if runID := c.taskTracker.TodoList().RunID(); runID != "" {
			return runID
		}
	}
	seed := "local"
	if c.sessionData != nil && c.sessionData.CreatedAt != "" {
		seed = c.sessionData.CreatedAt
	} else if c.session != nil && c.session.Workspace != "" {
		seed = c.session.Workspace
	}
	return "run-context-" + hashContentKey(seed)[:16]
}

func taskContextPhase(task TaskDef) Phase {
	if task.Phase != "" {
		return task.Phase
	}
	return PhaseExecute
}

func (c *Coordinator) newTaskContextRequest(task TaskDef, todoID string, attempt int, trigger ContextTrigger, agentName, agentRole string, failure *ContextFailure) ContextRequest {
	capabilities := append([]string(nil), task.Requires...)
	if c != nil && c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		capabilities = append(capabilities, c.phaseWorkflow.executionContext().Capabilities.Required...)
	}
	dependencies := make([]string, 0, len(task.DependsOn))
	if c != nil && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil && item.ID == todoID {
				dependencies = append(dependencies, item.DependsOn...)
				break
			}
		}
	}
	if len(dependencies) == 0 {
		for _, dependency := range task.DependsOn {
			dependencies = append(dependencies, strconv.Itoa(dependency))
		}
	}
	actionType := ""
	if task.Action != nil {
		actionType = task.Action.Type
	}
	modelExecutionID := contextModelExecutionID(todoID, agentName, task.Model)
	if c != nil && strings.TrimSpace(c.modelExecutionID) != "" {
		modelExecutionID = c.modelExecutionID
	}
	r := ContextRequest{
		SchemaVersion: ContextRequestSchemaVersion, RunID: c.contextRunID(), TaskID: todoID, Attempt: attempt,
		Goal: task.Goal, Constraints: task.Constraints, AgentName: agentName, AgentRole: agentRole,
		Phase: taskContextPhase(task), Trigger: trigger, ActionType: actionType, Capabilities: capabilities,
		DependencyIDs: dependencies, VerificationCriteria: task.Verify, Failure: failure,
		ModelExecutionID: modelExecutionID,
	}
	if c != nil && c.session != nil {
		r.EnvironmentFingerprint = hashContentKey(strings.TrimSpace(c.projectDir) + "\x00" + strings.TrimSpace(c.session.Config.Name))
	}
	r.AssignRequestID()
	return r
}

func contextModelExecutionID(todoID, agentName, model string) string {
	identity := strings.Join([]string{strings.TrimSpace(todoID), strings.TrimSpace(agentName), strings.TrimSpace(model)}, "\x00")
	if strings.Trim(identity, "\x00") == "" {
		return "model-execution-unknown"
	}
	return "model-execution-" + hashContentKey(identity)
}

func (c *Coordinator) newCoordinatorContextRequest(goal string, continuation bool, attempt int) ContextRequest {
	trigger := ContextTriggerCoordinatorStart
	if continuation {
		trigger = ContextTriggerContinuation
	}
	r := ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: c.contextRunID(), Attempt: attempt, Goal: goal, AgentName: "coordinator", AgentRole: "coordinator", Phase: PhaseInit, Trigger: trigger}
	if c != nil && c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		r.Phase = c.phaseWorkflow.State()
	}
	r.AssignRequestID()
	return r
}
