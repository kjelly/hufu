package team

// Single-task execution: building the worker prompt, running the agent with
// status reporting, deliverable verification, and failure reflection.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/audit"
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (string, error) {
	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}

	agentDef, _, err := c.resolveAgentName(task.Agent)
	if err != nil {
		c.PersistFailure(task.Agent, taskDesc, todoID, c.FailureDetail(err, "error"))
		return "", err
	}
	agentName := strings.ToLower(agentDef.Name)

	if len(agentDef.MCPTools) > 0 {
		defer func() {
			_ = c.mcpManager.UnloadAgentMCPServer(agentName)
		}()
	}

	// Check if agent has extra-models configured
	if len(agentDef.ExtraModels) > 0 {
		return c.executeTaskWithExtraModels(parentCtx, agentName, agentDef, taskDesc, todoID, task.Verify, task.AdversarialVerify)
	}

	// When a model-list is configured, validate the requested model is in it.
	// When no model-list is configured, ignore task.Model entirely — the agent's
	// own model (from agent.md > team.yaml) is used via resolveAgentModel below.
	if len(c.modelList) == 0 {
		task.Model = ""
	} else if task.Model != "" {
		found := false
		for _, m := range c.modelList {
			if m.ID == task.Model {
				found = true
				break
			}
		}
		if !found {
			var validIDs []string
			for _, m := range c.modelList {
				validIDs = append(validIDs, m.ID)
			}
			return "", fmt.Errorf("unknown model %q for agent %q (valid models: %v)", task.Model, task.Agent, validIDs)
		}
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	maxRetries := c.session.Config.MaxRetries
	if agentDef.MaxRetries >= 0 {
		maxRetries = agentDef.MaxRetries
	}
	if maxRetries < 1 {
		maxRetries = 1
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	if agentDef.Skills != "" {
		skills := strings.Split(agentDef.Skills, ",")
		for i, s := range skills {
			skills[i] = strings.TrimSpace(s)
		}
		c.taskTracker.TodoList().SetSkills(todoID, skills)
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	resolvedModel := c.resolveAgentModel(agentDef, task.Model)

	if c.think {
		c.emitThinkDelegation(agentName, taskDesc, resolvedModel)
	}

	c.report(c.newEvent("start").withAgent(agentName).withMessage(taskDesc).withModel(resolvedModel).withTodoID(todoID))
	prevAgent := c.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	prevTask := c.getSnapshotField(func(s *currentSnapshot) string { return s.Task })
	prevTodoID := c.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = agentName })
	c.updateSnapshot(func(s *currentSnapshot) { s.Task = taskDesc })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = todoID })
	defer func() {
		c.updateSnapshot(func(s *currentSnapshot) { s.Agent = prevAgent })
		c.updateSnapshot(func(s *currentSnapshot) { s.Task = prevTask })
		c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = prevTodoID })
	}()
	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "working", taskDesc, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, agentName, "working", taskDesc)

	timing := &taskTiming{}
	timing.reset()

	if task.PlanFirst && task.PlanID == "" {
		c.pendingPlansMu.Lock()
		existing := c.pendingPlans[todoID]
		c.pendingPlans[todoID] = &PlanEntry{
			TodoID: todoID,
			Agent:  agentName,
			Goal:   task.Goal,
			Status: "",
			ReviewCount: func() int {
				if existing != nil {
					return existing.ReviewCount
				}
				return 0
			}(),
			Task: task,
		}
		c.pendingPlansMu.Unlock()
	}

	var ag fantasy.Agent
	if task.PlanFirst && task.PlanID == "" {
		planAg, planErr := agent.CreateAgent(parentCtx, c.providerManager.GetProvider(resolvedModel), agent.AgentConfig{
			Def:        agentDef,
			TeamConfig: &c.session.Config,
			WorkDir:    c.projectDir,
			MaxSteps:   agent.DefaultMaxSteps,
		}, append(agent.SelectTools(c.coreTools, agentDef.Tools), &submitPlanTool{coordinator: c, todoID: todoID}))
		if planErr != nil {
			detail := c.FailureDetail(planErr, "")
			c.report(c.newEvent("error").withAgent(agentName).withMessage(planErr.Error()).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, detail)
			return "", planErr
		}
		ag = planAg
	} else {
		var err error
		ag, err = c.getOrCreateAgent(parentCtx, agentDef, task.Model)
		if err != nil {
			detail := c.FailureDetail(err, "")
			c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, detail)
			return "", err
		}
	}

	var prompt string
	if task.PlanFirst && task.PlanID != "" {
		c.pendingPlansMu.Lock()
		entry := c.pendingPlans[task.PlanID]
		if entry == nil {
			c.pendingPlansMu.Unlock()
			return "", fmt.Errorf("plan not found for id %s", task.PlanID)
		}
		planText := entry.PlanText
		c.pendingPlansMu.Unlock()

		prompt = "## Goal\n\n" + task.Goal
		if task.Constraints != "" {
			prompt += "\n\n## Constraints\n\n" + task.Constraints
		}
		prompt += "\n\n## Approved Plan\n\n" + planText
		stmPath := STMPath(c.session.Workspace)
		prompt += fmt.Sprintf("\n\n## Instructions\n\nExecute the approved plan above. You have already planned — now implement each step.\n\n- Key knowledge from previous agents is provided below. You do NOT need to read `%s` at the start. Only read it later if you need to check for *new* updates from concurrent agents.\n- Write key discoveries to `stm_write` immediately when found.\n- Call finish when done.", stmPath)
	} else if task.PlanFirst {
		prompt = "## Goal\n\n" + task.Goal
		if task.Constraints != "" {
			prompt += "\n\n## Constraints\n\n" + task.Constraints
		}
		prompt += "\n\n## Instructions\n\nDraft a detailed task execution plan before doing any work. Your plan should be a numbered list of concrete, actionable steps with brief descriptions. Consider your skills, available tools, and the project context. Call `submit_plan` with your complete plan when ready. Do NOT execute any steps yet — only plan."
	} else {
		prompt = "## Goal\n\n" + task.Goal
		if task.Constraints != "" {
			prompt += "\n\n## Constraints\n\n" + task.Constraints
		}
		stmPath := STMPath(c.session.Workspace)
		prompt += fmt.Sprintf("\n\n## Instructions\n\nYou are a domain expert. Determine your own implementation approach based on the goal above.\n\n- Key knowledge from previous agents is provided below. You do NOT need to read `%s` at the start. Only read it later if you need to check for *new* updates from concurrent agents.\n- When you discover something important (API shape, file location, decision, error), write it to `stm.md` immediately via `stm_write` — do not wait until the end.", stmPath)
	}

	// SSH session tracking is handled by the ssh tool's response hint.
	// No coordinator-level tracking is needed - each SSH call is independent.

	prompt = c.appendSkillContext(prompt, agentDef, agentName, task.Goal, todoID)

	if len(task.ContextFiles) > 0 {
		var contextBuilder strings.Builder
		contextBuilder.WriteString("Context files:\n\n")
		for _, f := range task.ContextFiles {
			content, err := readShared(c.session.Workspace, f)
			if err != nil {
				fmt.Fprintf(&contextBuilder, "(could not read %s: %v)\n", f, err)
			} else {
				fmt.Fprintf(&contextBuilder, "### %s\n```\n%s\n```\n\n", f, content)
			}
		}
		prompt = contextBuilder.String() + "\n---\n\n" + prompt
	}

	// Inject STM knowledge-transfer sections after the goal so the agent knows
	// what to do before reading prior context, then concurrent tasks, then LTM
	// as background. These are assembled under a combined character budget, in
	// priority order, so a small model's context window is not overwhelmed —
	// lower-priority blocks (LTM) are dropped first when over budget.
	var auxParts []string
	if stmCtx := c.buildTaskSTMContext(); stmCtx != "" {
		auxParts = append(auxParts, stmCtx)
	}
	if concurrentCtx := c.buildConcurrentTasksContext(todoID); concurrentCtx != "" {
		auxParts = append(auxParts, concurrentCtx)
	}
	if ltmCtx := c.buildLTMContext(); ltmCtx != "" {
		auxParts = append(auxParts, ltmCtx)
	}
	if aux := assembleContextWithinBudget(auxParts, maxWorkerAuxContextChars); aux != "" {
		prompt = prompt + aux
	}

	var conversationHistory []fantasy.Message
	var lastErr error
	// appliedHint tracks the most recent reflection hint fed into a retry (and
	// the error that triggered it) so a rescued task can persist the lesson.
	var appliedHint, appliedHintTrigger string
	// Plan-first agents carry a submitPlanTool the escalated replacement would
	// lose, so escalation only applies to the normal execution path.
	escalate := taskEscalationEnabled(task, &c.session.Config, len(c.modelList)) &&
		(!task.PlanFirst || task.PlanID != "")
	for attempt := 1; attempt <= maxRetries; attempt++ {
		currentPrompt := prompt
		if attempt > 1 {
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("retry %d/%d — continuing from previous progress", attempt, maxRetries)))
			if lastErr != nil {
				if hint := c.reflectOnFailure(parentCtx, agentName, task.Goal, lastErr.Error()); hint != "" {
					currentPrompt += hint
					appliedHint = strings.TrimPrefix(hint, reflectionHeader)
					appliedHintTrigger = lastErr.Error()
				}
			}
			if escalate {
				if next := nextStrongerModel(c.modelList, resolvedModel); next != "" {
					if escAg, escErr := c.getOrCreateAgent(parentCtx, agentDef, next); escErr == nil {
						ag = escAg
						c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("escalating model %s → %s (attempt %d)", resolvedModel, next, attempt)).withTodoID(todoID))
						resolvedModel = next
					} else {
						c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("escalation to model %s failed, retrying with %s: %v", next, resolvedModel, escErr)).withTodoID(todoID))
					}
				}
			}
		}

		var output string
		var steps []fantasy.StepResult
		var err error
		func() {
			taskCtx, cancel := context.WithTimeout(parentCtx, agentTimeout)
			defer cancel()
			taskCtx = tools.AskUserAwareDeadline(taskCtx)
			taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
			taskCtx = context.WithValue(taskCtx, modelKey{}, resolvedModel)
			taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
			taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, taskDesc)
			if len(agentDef.Guard) > 0 {
				taskCtx = context.WithValue(taskCtx, tools.GuardRulesKey, agentDef.Guard)
			}
			if len(agentDef.AllowedPaths) > 0 {
				taskCtx = context.WithValue(taskCtx, tools.AgentAllowedPathsKey, agentDef.AllowedPaths)
			}
			if agentDef.RestrictedPath != "" {
				taskCtx = context.WithValue(taskCtx, tools.AgentRestrictedPathKey, agentDef.RestrictedPath)
			}
			if c.noNet || agentDef.NoNet {
				taskCtx = context.WithValue(taskCtx, tools.AgentNetworkBlockKey, true)
			}
			if c.forceMCP || agentDef.ForceMCP {
				taskCtx = context.WithValue(taskCtx, tools.AgentForceMCPKey, true)
			}
			if c.unattended {
				taskCtx = context.WithValue(taskCtx, tools.UnattendedKey, true)
				taskCtx = context.WithValue(taskCtx, tools.AskUserChoiceSelectorKey, tools.AskUserChoiceSelector(func(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
					return c.chooseAskUserResponse(ctx, question, qtype, opts, allowAny)
				}))
			}
			if c.autoApprove {
				taskCtx = context.WithValue(taskCtx, tools.AutoApproveKey, true)
			}

			// Merge team-level and agent-level tool allowlists.
			// Agent .md "tools" field is treated as an explicit allowlist:
			// if an agent has "bash" in its tools, it should be able to use
			// bash without prompting the user for permission.
			allowedTools := make([]string, len(c.session.Config.ToolsAllowed))
			copy(allowedTools, c.session.Config.ToolsAllowed)
			if agentDef.Tools != "" {
				for _, t := range strings.Split(agentDef.Tools, ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						allowedTools = append(allowedTools, t)
					}
				}
			}
			if len(allowedTools) > 0 {
				taskCtx = context.WithValue(taskCtx, tools.AgentToolsAllowedKey, allowedTools)
			}

			// Inject permanent session-level permissions
			c.sessionToolPermissionsMu.RLock()
			sessionPerms := make(map[string]bool, len(c.sessionToolPermissions))
			for k, v := range c.sessionToolPermissions {
				sessionPerms[k] = v
			}
			c.sessionToolPermissionsMu.RUnlock()
			taskCtx = context.WithValue(taskCtx, tools.AgentToolsSessionPermissionsKey, sessionPerms)

			// Provide callback to update session-level permissions
			taskCtx = context.WithValue(taskCtx, tools.ToolPermissionCallbackKey, tools.ToolPermissionCallback(func(name string, allowed bool) {
				c.sessionToolPermissionsMu.Lock()
				c.sessionToolPermissions[name] = allowed
				c.sessionToolPermissionsMu.Unlock()
			}))

			output, steps, err = c.runAgentWithStatusAndHistory(taskCtx, ag, agentName, currentPrompt, conversationHistory, timing)
		}()

		if err == nil {
			c.pendingPlansMu.Lock()
			planEntry := c.pendingPlans[todoID]
			c.pendingPlansMu.Unlock()
			if planEntry != nil && planEntry.Status == "submitted" {
				c.pendingPlansMu.Lock()
				planEntry.Agent = agentName
				planEntry.Goal = task.Goal
				planEntry.Task = task
				c.pendingPlansMu.Unlock()
				c.taskTracker.TodoList().UpdateStatus(todoID, TaskPlanned, "")
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("step").withAgent(agentName).withMessage("plan submitted").withTodoID(todoID))
				c.report(c.newEvent("done").withAgent(agentName).withMessage("plan submitted").withTodoID(todoID))
				if c.forcePlanFirst {
					return "", nil
				}
				return planEntry.PlanText, nil
			}
			// Deliverable verification: run an objective check before accepting
			// the agent's claim of success. A non-zero exit converts this into a
			// failure that flows into the normal retry path below.
			if task.Verify != "" {
				if verr := c.verifyTaskDeliverable(parentCtx, agentDef, task.Verify); verr != nil {
					err = fmt.Errorf("deliverable verification failed (command %q): %w", task.Verify, verr)
					c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("verification failed: %v", verr)).withTodoID(todoID))
				}
			}
			// Adversarial verification: skeptic votes try to refute the result.
			// A refutation flows into the same retry path as a failed verify.
			if err == nil && task.AdversarialVerify > 0 && c.Sidecar() != nil {
				if averr := c.adversarialVerify(parentCtx, task, output); averr != nil {
					err = averr
					c.report(c.newEvent("skeptic").withAgent(agentName).withMessage(averr.Error()).withTodoID(todoID))
				} else {
					c.report(c.newEvent("skeptic").withAgent(agentName).withMessage(fmt.Sprintf("confirmed by %d skeptic vote(s)", len(skepticLenses(task.AdversarialVerify)))).withTodoID(todoID))
				}
			}
			if err == nil {
				if verr := validateTaskOutput(task, output); verr != nil {
					err = fmt.Errorf("task completion validation failed: %w", verr)
					c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("completion validation failed: %v", verr)).withTodoID(todoID))
				}
			}
			if err == nil {
				if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "done", taskDesc, output); err != nil {
					log.Printf("warning: failed to write task file: %v", err)
				}
				_ = writeStatus(c.session.Workspace, agentName, "done", taskDesc)
				duration, modelTime, toolTime := timing.snapshot()
				c.taskTracker.TodoList().UpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output)
				c.updateTodoTiming(todoID, modelTime, toolTime)
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("done").withAgent(agentName).withOutput(output).withMessage("completed").withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
				if task.Summarize {
					output = c.summarizeOutput(parentCtx, output)
				}
				c.autoWriteSTMASync(agentName, taskDesc, output, "", true)
				if appliedHint != "" {
					c.persistReflexionLessonAsync(agentName, task.Goal, appliedHintTrigger, appliedHint, true)
				}
				return output, nil
			}
		}

		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		// Repeated-failure detection: if this attempt failed with the same error
		// as the previous one, retrying is unproductive — the agent is stuck
		// repeating the same action. Stop early instead of burning the remaining
		// retry budget on identical failures.
		if lastErr != nil && attempt < maxRetries && sameFailure(lastErr.Error(), err.Error()) {
			lastErr = err
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("stopping retries: attempt %d repeated the same failure", attempt)).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(fmt.Errorf("repeated failure after %d attempts: %w", attempt, err), "error"))
			break
		}

		lastErr = err
		if isTaskTimeout(err) {
			duration, modelTime, toolTime := timing.snapshot()
			c.report(c.newEvent("task_timeout").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d timed out after %s", attempt, duration.Round(time.Second))).withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
		}
		c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)).withModel(resolvedModel).withTodoID(todoID))
		c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(err, ""))

		if parentCtx.Err() != nil {
			break
		}
	}

	_, modelTime, toolTime := timing.snapshot()
	c.updateTodoTiming(todoID, modelTime, toolTime)
	detail := c.FailureDetail(lastErr, "")
	c.PersistFailure(agentName, taskDesc, todoID, detail)
	c.autoWriteSTMASync(agentName, taskDesc, "", lastErr.Error(), false)
	if maxRetries > 1 {
		c.persistReflexionLessonAsync(agentName, task.Goal, lastErr.Error(), appliedHint, false)
	}
	return "", fmt.Errorf("agent %q failed after %d attempts (model: %s): %w", agentName, maxRetries, resolvedModel, lastErr)
}

func (c *Coordinator) runAgentWithStatus(ctx context.Context, ag fantasy.Agent, agentName, prompt string, timing *taskTiming) (string, error) {
	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, agentName, prompt, nil, timing)
	return output, err
}

func isTaskTimeout(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded")
}

func (c *Coordinator) executeSidecarTask(ctx context.Context, task TaskDef, todoID string) (string, error) {
	s := c.Sidecar()
	if s == nil {
		// Sidecar not configured: gracefully fall back to normal agent execution
		log.Printf("[INFO] sidecar not configured for task %q, falling back to normal agent execution", task.Goal)
		return c.executeTask(ctx, task, todoID)
	}

	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("sidecar_call").withAgent(task.Agent).withMessage(taskDesc))

	if c.think {
		c.emitThinkDelegation(task.Agent, taskDesc, c.sidecarModel)
		c.emitThinkSidecar("Execute", fmt.Sprintf("running task via sidecar model: %s", c.sidecarModel))
	}

	sidecarTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if sidecarTimeout <= 0 {
		sidecarTimeout = 120 * time.Second
	}
	sidecarCtx, cancel := context.WithTimeout(ctx, sidecarTimeout)
	defer cancel()

	result, err := s.Execute(sidecarCtx, taskDesc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar execute failed for agent %q: %v\n", task.Agent, err)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("sidecar execution failed (model: %s): %w", c.sidecarModel, err)
	}

	c.taskTracker.TodoList().UpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(result, summaryMaxRunes), result)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(task.Agent).withOutput(result).withMessage("sidecar completed").withTodoID(todoID))
	return result, nil
}

// lastToolCallEntry tracks the most recent tool call for deadloop detection.
// Used by runAgentWithStatusAndHistory to detect stuck agents repeating the
// same failing tool call.
type lastToolCallEntry struct {
	toolName string
	input    string
}

func (c *Coordinator) runAgentWithStatusAndHistory(ctx context.Context, ag fantasy.Agent, agentName, prompt string, history []fantasy.Message, timing *taskTiming, extraStop ...fantasy.StopCondition) (string, []fantasy.StepResult, error) {
	reportFn := c.reportStatus
	workspace := c.session.Workspace
	teamName := c.session.Config.Name
	logWrite := func(entry string) { writeLLMLog(workspace, teamName, agentName, entry) }

	// Pick up the TodoItem ID injected by executeTask so events can be attributed to a task.
	todoID, _ := ctx.Value(todoIDKey{}).(string)

	var loopDetectMu sync.Mutex
	var lastToolCall *lastToolCallEntry
	consecutiveErrCount := 0
	// Maps in-flight tool call IDs to their input so error counting can match
	// on tool+input; counting by tool name alone lets one failure of input A
	// plus one failure of input B trip the detector on a first repeat of B.
	pendingToolInputs := make(map[string]string)
	tp := &ThinkParser{}

	streamCall := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		StopWhen: extraStop,
		PrepareStep: func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			llmLogRequest(logWrite, opts)
			return ctx, fantasy.PrepareStepResult{}, nil
		},
		OnStepStart: func(stepNumber int) error {
			reportFn(c.newEvent("step").withAgent(agentName).withTodoID(todoID).withStep(stepNumber).withMessage(fmt.Sprintf("step %d", stepNumber)))
			c.SetCurrentStep(stepNumber)
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			timing.beginTool()
			argsPreview := tc.Input
			if len(argsPreview) > 10000 {
				r := []rune(argsPreview)
				if len(r) > 10000 {
					argsPreview = string(r[:10000]) + "..."
				}
			}
			reportFn(c.newEvent("tool_call").withAgent(agentName).withTodoID(todoID).withTool(tc.ToolName, argsPreview))
			llmLogStreamEvent(logWrite, "tool_call", formatToolCallContent(tc))
			audit.LogToolCall(agentName, tc.ToolName, tc.Input)
			c.SetCurrentStage("tool")
			c.SetCurrentTool(tc.ToolName)

			// 🔁 Deadloop / thrashing detection!
			loopDetectMu.Lock()
			pendingToolInputs[tc.ToolCallID] = tc.Input
			if lastToolCall != nil && lastToolCall.toolName == tc.ToolName && lastToolCall.input == tc.Input {
				if consecutiveErrCount >= 2 {
					loopDetectMu.Unlock()
					return fmt.Errorf("agent %s is stuck in a loop executing the same failing command: %s (args: %s)", agentName, tc.ToolName, argsPreview)
				}
			} else {
				lastToolCall = &lastToolCallEntry{
					toolName: tc.ToolName,
					input:    tc.Input,
				}
				consecutiveErrCount = 0
			}
			loopDetectMu.Unlock()

			// Record tool call for skill pattern detection
			if c.skillDetector != nil {
				taskDesc := ""
				if s := c.current.Load(); s != nil {
					taskDesc = s.Task
				}
				if taskDesc == "" {
					taskDesc = "coordinator task"
				}
				c.skillDetector.RecordToolCall(agentName, tc.ToolName, tc.Input, taskDesc)
			}

			if skillName := c.extractSkillFromToolCall(tc.ToolName, tc.Input); skillName != "" {
				c.recordSkillUsage(skillName, agentName)
			}
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			timing.endTool()
			resultPreview := ""
			if tr.Result != nil {
				resultPreview, _ = toolResultOutputText(tr.Result)
			}
			resolvedModel, _ := ctx.Value(modelKey{}).(string)
			reportFn(c.newEvent("tool_result").withAgent(agentName).withTodoID(todoID).withToolResult(tr.ToolName, resultPreview).withModel(resolvedModel))
			llmLogStreamEvent(logWrite, "tool_result", formatToolResultContent(tr))
			_, isErrResult := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result)
			audit.LogToolResult(agentName, tr.ToolName, resultPreview, isErrResult)

			// 🔁 Track error count for the exact call (tool + input) the
			// detector is watching; results of other in-flight calls of the
			// same tool must not inflate the counter.
			loopDetectMu.Lock()
			callInput, tracked := pendingToolInputs[tr.ToolCallID]
			delete(pendingToolInputs, tr.ToolCallID)
			if tracked && lastToolCall != nil && lastToolCall.toolName == tr.ToolName && lastToolCall.input == callInput {
				if isErrResult {
					consecutiveErrCount++
				} else {
					consecutiveErrCount = 0
				}
			}
			loopDetectMu.Unlock()

			c.saveCheckpoint()
			return nil
		},
		OnTextDelta: func(id, text string) error {
			tp.Process(text, func(txt string) {
				reportFn(c.newEvent("text").withAgent(agentName).withTodoID(todoID).withMessage(txt))
				logWrite(txt)
			}, func(rsn string) {
				reportFn(c.newEvent("reasoning").withAgent(agentName).withTodoID(todoID).withMessage(rsn))
				logWrite(rsn)
			})
			return nil
		},
		OnReasoningDelta: func(id, text string) error {
			reportFn(c.newEvent("reasoning").withAgent(agentName).withTodoID(todoID).withMessage(text))
			logWrite(text)
			return nil
		},
		OnStreamFinish: func(usage fantasy.Usage, finishReason fantasy.FinishReason, providerMetadata fantasy.ProviderMetadata) error {
			tp.Flush(func(txt string) {
				reportFn(c.newEvent("text").withAgent(agentName).withTodoID(todoID).withMessage(txt))
				logWrite(txt)
			}, func(rsn string) {
				reportFn(c.newEvent("reasoning").withAgent(agentName).withTodoID(todoID).withMessage(rsn))
				logWrite(rsn)
			})
			llmLogStreamFinish(logWrite, finishReason, usage)

			if c.hooks != nil && c.hooks.HasHooks("after_llm_step") {
				resolvedModel, _ := ctx.Value(modelKey{}).(string)
				hookCtx := hooks.MakeContext(teamName, agentName, todoID, "", resolvedModel, "")
				hookPayload := hooks.HookPayload{
					HookPoint: "after_llm_step",
					Context:   hookCtx,
					Model:     resolvedModel,
					Usage: hooks.UsageSummary{
						PromptTokens:     int(usage.InputTokens),
						CompletionTokens: int(usage.OutputTokens),
						TotalTokens:      int(usage.TotalTokens),
					},
				}
				c.hooks.Dispatch(ctx, "after_llm_step", hookPayload)
			}

			return nil
		},
	}

	if c.hooks != nil && c.hooks.HasHooks("before_llm_step") {
		resolvedModel, _ := ctx.Value(modelKey{}).(string)
		hookCtx := hooks.MakeContext(teamName, agentName, todoID, "", resolvedModel, "")
		hookPayload := hooks.HookPayload{
			HookPoint: "before_llm_step",
			Context:   hookCtx,
			Model:     resolvedModel,
		}
		resp := c.hooks.Dispatch(ctx, "before_llm_step", hookPayload)
		if resp.Result == hooks.HookSkip {
			return "", nil, nil
		}
		if resp.Result == hooks.HookError {
			return "", nil, fmt.Errorf("hook error: %s", resp.ErrorMessage)
		}
	}

	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = agentName })
	c.SetCurrentStage("model")
	if resolvedModel, ok := ctx.Value(modelKey{}).(string); ok && resolvedModel != "" {
		c.SetCurrentModel(resolvedModel)
	}

	result, err := ag.Stream(ctx, streamCall)
	c.SetCurrentStage("idle")
	if result != nil {
		c.addStepTokens(result.Steps)
	}
	if err != nil {
		return "", nil, err
	}
	return result.Response.Content.Text(), result.Steps, nil
}

func (c *Coordinator) buildConcurrentTasksContext(excludeID string) string {
	items := c.taskTracker.TodoList().Items()
	var running []string
	for _, item := range items {
		if item.ID != excludeID && item.Status == TaskInProgress {
			running = append(running, fmt.Sprintf("- %s: %s", item.Agent, item.Desc))
		}
	}
	if len(running) == 0 {
		return ""
	}
	return "## Concurrent Tasks\n\nThe following agents are running in parallel with you. Avoid overlapping with their work:\n\n" + strings.Join(running, "\n")
}

// verifyTaskDeliverable runs the task's optional verify command and returns a
// non-nil error if the command exits non-zero (or cannot be run). This provides
// an objective, non-LLM check that the deliverable actually exists/works before
// a task is accepted as done. The command runs in the project directory using
// the team's (or agent's) configured shell, falling back to "sh".
func (c *Coordinator) verifyTaskDeliverable(parentCtx context.Context, agentDef *agent.AgentDef, command string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil
	}

	shell := "sh"
	if agentDef != nil && agentDef.Shell != "" {
		shell = agentDef.Shell
	} else if c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}

	timeout := time.Duration(c.session.Config.Timeout) * time.Second
	if timeout <= 0 || timeout > 120*time.Second {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-c", command)
	if c.projectDir != "" {
		cmd.Dir = c.projectDir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		return fmt.Errorf("%v%s", err, detail)
	}
	return nil
}

func (c *Coordinator) reflectOnFailure(ctx context.Context, agentName, goal, lastErr string) string {
	s := c.Sidecar()
	if s != nil {
		// Use a shorter timeout for reflection to avoid holding up retries
		reflectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		prompt := fmt.Sprintf("Agent %q failed to achieve goal: %q\nError: %s\n\nAnalyze the error and provide a concise hint (max 100 words) for the next attempt. Focus on what to change or avoid.", agentName, goal, lastErr)
		if reflection, err := s.Execute(reflectCtx, prompt); err == nil && strings.TrimSpace(reflection) != "" {
			return reflectionHeader + reflection
		}
	}
	// Fallback: deterministic, LLM-free hint derived from the error so retries
	// are never blind even when no sidecar is configured or it is unavailable.
	if hint := localFailureHint(lastErr); hint != "" {
		return reflectionHeader + hint
	}
	return ""
}

// sameFailure reports whether two error messages represent the same underlying
// failure, ignoring volatile prefixes like "attempt N failed". It is used to
// detect an agent stuck repeating an identical failing action across retries.
func sameFailure(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		// Drop a leading "attempt N failed:" wrapper if present.
		if i := strings.Index(s, "failed:"); i >= 0 && strings.HasPrefix(s, "attempt ") {
			s = strings.TrimSpace(s[i+len("failed:"):])
		}
		return s
	}
	na, nb := norm(a), norm(b)
	return na != "" && na == nb
}

// localFailureHint classifies a failure message and returns an actionable hint
// without calling any model. It pattern-matches common error shapes (timeout,
// missing file/command, permission, verification, step exhaustion).
func localFailureHint(lastErr string) string {
	e := strings.ToLower(lastErr)
	switch {
	case strings.Contains(e, "returned empty output"):
		return "Your previous attempt ended without a final message. Commands that succeed with no stdout are still results — end with a short summary of what was run and what happened, quoting exit codes or '(no output)' where relevant."
	case strings.Contains(e, "unfinished progress update"):
		return "Your previous attempt ended mid-narration ('let me...', 'I'll...'). Finish the work first, then end with the final result, not a description of what you are about to do."
	case strings.Contains(e, "deliverable verification failed"):
		return "Your previous attempt reported success but the verification check failed — the expected deliverable was missing or invalid. Actually produce the artifact (create/modify the file, make it pass the check) before calling finish; do not claim completion prematurely."
	case strings.Contains(e, "deadline exceeded") || strings.Contains(e, "timed out") || strings.Contains(e, "context deadline"):
		return "The previous attempt timed out. Work in smaller steps, avoid long-running or interactive commands, and prioritize the core of the goal first."
	case strings.Contains(e, "no such file") || strings.Contains(e, "not found") || strings.Contains(e, "enoent"):
		return "A file or command was not found last time. Verify the path exists with ls/glob before using it, and use absolute paths under the workspace."
	case strings.Contains(e, "permission denied") || strings.Contains(e, "not permitted") || strings.Contains(e, "guard rule"):
		return "The previous attempt was blocked by a permission or guard rule. Use only the tools and paths you are allowed; do not retry the exact blocked action — find a permitted alternative."
	case strings.Contains(e, "step") && (strings.Contains(e, "limit") || strings.Contains(e, "count") || strings.Contains(e, "max")):
		return "You ran out of steps last time. Be more direct: skip exploratory actions and go straight to the actions that satisfy the goal."
	case strings.Contains(e, "duplicate"):
		return "This work overlaps with an already-completed task. Reuse the existing result instead of redoing it, or address the part that is genuinely missing."
	default:
		return "The previous attempt failed with: " + utils.TruncateString(strings.TrimSpace(lastErr), 300) + ". Change your approach rather than repeating the same actions."
	}
}
