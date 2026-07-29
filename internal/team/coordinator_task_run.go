package team

// Single-task execution: building the worker prompt, running the agent with
// status reporting, deliverable verification, and failure reflection.

import (
	"bytes"
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
	if err := ValidateExecutionContract(task); err != nil {
		return "", err
	}
	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}

	agentDef, _, err := c.AgentPool().ResolveAgentName(task.Agent)
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
	if err := c.validateTaskModel(&task); err != nil {
		return "", err
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
	c.reconcileTaskStatusProjection()
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
	if c.workerAgentOverride != nil {
		ag = c.workerAgentOverride
	} else {
		ag, err = c.createTaskAgent(parentCtx, agentDef, task, resolvedModel, todoID, taskDesc, agentName)
		if err != nil {
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
	if task.Verify != "" {
		prompt += completionVerificationInstructions(task.Verify, c.projectDir)
	}
	if taskUsesVerbatimTranscript(task) {
		prompt += "\n\n## Verbatim Output Contract\n\nhufu captures every tool call and tool result into a complete transcript artifact. Do not reproduce raw command output in your final response. Submit a concise structured result; the runner will attach the authoritative transcript manifest."
	}
	if agentDef != nil {
		if note := toolUsageNotes(agentDef.Tools); note != "" {
			prompt += note
		}
	}

	// SSH session tracking is handled by the ssh tool's response hint.
	// No coordinator-level tracking is needed - each SSH call is independent.

	prompt = c.appendSkillContext(prompt, agentDef, agentName, task.Goal, todoID)

	contextFiles := make(map[string]string)
	if len(task.ContextFiles) > 0 {
		for _, f := range task.ContextFiles {
			content, err := readShared(c.session.Workspace, f)
			if err == nil && content != "" {
				contextFiles[f] = content
			}
		}
	}

	var depResults []TaskResult
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		var currentTodo *TodoItem
		for _, item := range c.taskTracker.TodoList().Items() {
			if item.ID == todoID {
				currentTodo = item
				break
			}
		}
		if currentTodo != nil && len(currentTodo.DependsOn) > 0 {
			depSet := make(map[string]bool, len(currentTodo.DependsOn))
			for _, depID := range currentTodo.DependsOn {
				depSet[depID] = true
			}
			for _, item := range c.taskTracker.TodoList().Items() {
				if depSet[item.ID] && item.Status == TaskDone {
					if res := c.GetTaskResult(item.ID); res != nil {
						depResults = append(depResults, *res)
					}
				}
			}
		}
	}

	var modelSpec ModelContextSpec
	if agentDef != nil {
		modelSpec = globalRegistry.GetSpec(c.resolveAgentModel(agentDef, task.Model))
	}

	workerInput := WorkerContextInput{
		TaskGoal:          prompt,
		TaskDef:           task,
		AgentDef:          agentDef,
		RawSTM:            LoadSTM(c.session.Workspace),
		RawLTM:            LoadLTM(c.session.Workspace, c.session.Config.Name),
		ContextFiles:      contextFiles,
		ConcurrentTasks:   c.buildConcurrentTasksContext(todoID),
		DependencyResults: depResults,
		MemoryStore:       c.memoryStore,
		ModelContext:      modelSpec,
		MaxAuxChars:       maxWorkerAuxContextChars,
		DisableMemory:     c.ExecutionProfile().DisableHistoricalMemory,
	}

	// Keep the legacy prompt as the model input during Phase 2. The compiler
	// only records a safe, inspectable shadow comparison.
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
		prompt += aux
	}
	c.compileShadowWorker(parentCtx, workerInput, prompt)

	var conversationHistory []fantasy.Message
	var transcript *taskTranscript
	var lastErr error
	// attemptsMade tracks how many attempts actually ran, since the loop can
	// exit early (sameFailure, an unfixable verify error, or context
	// cancellation) well before reaching maxRetries. The final failure
	// message must report this, not the configured cap, or forensics on a
	// 2-attempt exit reads "failed after 3 attempts" and looks inconsistent
	// with the "repeated failure after 2 attempts" persisted alongside it.
	attemptsMade := 0
	// lastOutput keeps the most recent non-empty agent output so a final
	// failure (e.g. deliverable verification) does not discard findings the
	// coordinator could act on.
	var lastOutput string
	// appliedHint tracks the most recent reflection hint fed into a retry (and
	// the error that triggered it) so a rescued task can persist the lesson.
	var appliedHint, appliedHintTrigger string
	// Plan-first agents carry a submitPlanTool the escalated replacement would
	// lose, so escalation only applies to the normal execution path.
	escalate := taskEscalationEnabled(task, &c.session.Config, len(c.modelList)) &&
		(!task.PlanFirst || task.PlanID != "")
	for attempt := 1; attempt <= maxRetries; attempt++ {
		attemptsMade = attempt
		transcript = nil
		closeTranscript := func() {
			if transcript != nil {
				if closeErr := transcript.Close(); closeErr != nil {
					log.Printf("warning: close task transcript: %v", closeErr)
				}
				transcript = nil
			}
		}
		if taskUsesVerbatimTranscript(task) || task.Execution.RequiresResult {
			// Each attempt gets its own immutable transcript. The repair agent
			// is intentionally given no recorder, so it cannot alter this
			// execution evidence.
			transcript, err = newTaskTranscriptForAttempt(c.session.Workspace, todoID, c.executionRunID, attempt)
			if err != nil {
				return "", err
			}
		}
		if attempt > 1 {
			c.recordRetry(classifyTaskFailure(lastErr))
		}
		attemptStarted := time.Now()
		c.recordExecutionEvent(todoID, agentName, attempt, "in_progress", resolvedModel, 0, ExecutionUsage{})
		currentPrompt := prompt
		if attempt > 1 {
			if statusErr := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskInProgress, fmt.Sprintf("retry %d/%d", attempt, maxRetries), ""); statusErr == nil {
				c.reconcileTaskStatusProjection()
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			}
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
					resultTool := &submitResultTool{coordinator: c, todoID: todoID}
					if escAg, escErr := c.createTaskAgentWithResultTool(parentCtx, agentDef, next, resultTool); escErr == nil {
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
		protocolFailure := false
		func() {
			taskCtx, cancel := tools.WithInteractiveAwareTimeout(parentCtx, agentTimeout)
			defer cancel()
			taskCtx, roundCancel := context.WithCancel(taskCtx)
			defer roundCancel()
			c.registerTerminalRound(todoID, roundCancel)
			defer c.unregisterTerminalRound(todoID)
			taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
			taskCtx = context.WithValue(taskCtx, modelKey{}, resolvedModel)
			taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
			taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, taskDesc)
			if transcript != nil {
				taskCtx = context.WithValue(taskCtx, taskTranscriptKey{}, transcript)
			}
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

			taskCtx = c.withEffectiveToolsAllowed(taskCtx, agentDef)

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
			if err == nil && strings.TrimSpace(output) == "" && len(steps) > 0 {
				// The agent worked but never wrote a final message — almost
				// always the step cap cutting it off mid-diagnosis. Give it one
				// tool-free turn to summarize instead of failing the task and
				// re-running everything from scratch.
				if rescued := c.rescueFinalSummary(taskCtx, ag, agentName, steps, timing); rescued != "" {
					output = rescued
				}
			}
		}()
		runID := c.executionRunID
		if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			runID = c.taskTracker.TodoList().RunID()
		}
		transcriptRef := ""
		if transcript != nil {
			// Capture the worker's original final response before any repair
			// agent is started. The repair context intentionally does not carry
			// this recorder, so its output cannot mutate the original evidence.
			if recordErr := transcript.RecordAssistantOutput(output); recordErr != nil && err == nil {
				err = fmt.Errorf("record original task transcript: %w", recordErr)
			}
			if ref, manifestErr := transcript.Manifest(); manifestErr != nil {
				if err == nil {
					err = fmt.Errorf("create original task transcript manifest: %w", manifestErr)
				}
			} else {
				transcriptRef = ref.Path
			}
		}
		receipt := ExecutionReceipt{
			RunID:         runID,
			TaskID:        todoID,
			Attempt:       attempt,
			StartedAt:     attemptStarted,
			FinishedAt:    time.Now(),
			ProducerID:    agentName,
			TranscriptRef: transcriptRef,
		}
		if err == nil {
			zero := 0
			receipt.ExitCode = &zero
		}
		if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
		}

		if strings.TrimSpace(output) != "" {
			lastOutput = output
		}
		// A terminal attach cancels the active model round and parks this task
		// until the human explicitly detaches. Re-use its recorded conversation
		// history and the same retry budget when control comes back.
		if c.waitForTerminalResume(parentCtx, todoID) {
			if len(conversationHistory) == 0 && len(steps) > 0 {
				conversationHistory = append(conversationHistory, fantasy.NewUserMessage(currentPrompt))
			}
			for _, step := range steps {
				conversationHistory = append(conversationHistory, step.Messages...)
			}
			attempt--
			closeTranscript()
			continue
		}
		terminalBlocked := false
		if c.terminalSessionMgr != nil {
			if terminalErr := c.terminalSessionMgr.RequireTaskClosed(todoID); terminalErr != nil {
				err = terminalErr
				terminalBlocked = true
			}
		}

		if err == nil {
			var typedRes *TaskResult
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
				c.recordExecutionEvent(todoID, agentName, attempt, "planned", resolvedModel, time.Since(attemptStarted), usageFromSteps(steps))
				if c.forcePlanFirst {
					closeTranscript()
					return "", nil
				}
				closeTranscript()
				return planEntry.PlanText, nil
			}
			typedRes = c.GetTaskResult(todoID)
			if typedRes == nil {
				if task.Execution.RequiresResult {
					// Protocol failure: the agent finished execution but omitted submit_result.
					// Classify as FailureProtocol, set task to protocol_incomplete,
					// and attempt single-step, tool-free repair allowing ONLY submit_result.
					protocolFailure = true
					protocolErrMsg := fmt.Sprintf("protocol-only failure for task %s (%s): agent omitted submit_result; entering protocol_incomplete for tool-free repair (class: %s)",
						todoID, agentName, string(FailureProtocol))
					if statusErr := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskProtocolIncomplete, "protocol incomplete: missing required result", output); statusErr == nil {
						c.reconcileTaskStatusProjection()
						c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					}
					c.report(c.newEvent("step").withAgent(agentName).withMessage(protocolErrMsg).withTodoID(todoID))

					repairResultTool := &submitResultTool{coordinator: c, todoID: todoID}
					var repairAg fantasy.Agent
					if c.repairAgentOverride != nil {
						repairAg = c.repairAgentOverride
					} else if c.providerManager != nil {
						var rErr error
						repairAg, rErr = agent.CreateAgent(parentCtx, c.providerManager.GetProvider(resolvedModel), agent.AgentConfig{
							Def:        agentDef,
							TeamConfig: &c.session.Config,
							WorkDir:    c.projectDir,
							MaxSteps:   1,
						}, []fantasy.AgentTool{repairResultTool})
						if rErr != nil {
							c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("failed to create protocol repair agent: %v", rErr)).withTodoID(todoID))
						}
					}

					repairPrompt := fmt.Sprintf("## Goal\n%s\n\n## Execution Output\n%s\n\n## Repair Instructions\nYour execution completed and produced output, but you did not submit a structured result via submit_result as required. Call submit_result now using the output above to supply the required structured result. Do NOT call any other tools.", task.Goal, output)
					if repairAg != nil {
						repairCtx := context.WithValue(parentCtx, tools.AgentToolsAllowedKey, []string{"submit_result"})
						repairCtx = context.WithValue(repairCtx, todoIDKey{}, todoID)
						repairCtx = context.WithValue(repairCtx, modelKey{}, resolvedModel)
						repairCtx = context.WithValue(repairCtx, tools.AgentNameKey, agentName)
						repairCtx = context.WithValue(repairCtx, hooks.AgentNameKey, agentName)
						repairCtx = context.WithValue(repairCtx, hooks.TeamNameKey, c.session.Config.Name)
						repairCtx = context.WithValue(repairCtx, hooks.TaskDescKey, taskDesc)

						_, _, _ = c.runAgentWithStatusAndHistory(repairCtx, repairAg, agentName, repairPrompt, nil, timing, fantasy.StepCountIs(1))
						typedRes = c.GetTaskResult(todoID)
					}

					repairSuccess := typedRes != nil && typedRes.Source == "submitted" && validateSubmittedTaskResult(typedRes) == nil
					receipt.FinishedAt = time.Now()
					receipt.RepairProvenance = &RepairProvenance{
						Attempted:       true,
						Success:         repairSuccess,
						Prompt:          repairPrompt,
						SubmittedResult: typedRes,
					}
					if repairSuccess {
						c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("protocol repair succeeded for task %s", todoID)).withTodoID(todoID))
					} else {
						typedRes = nil
						err = fmt.Errorf("protocol failure (class: %s) for task %s (%s): agent produced output but failed protocol repair to submit_result",
							string(FailureProtocol), todoID, agentName)
						receipt.RepairProvenance.Error = err.Error()
					}
					if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
						_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
					}
				} else if strings.TrimSpace(output) != "" {
					// Non-protocol task: recover free text as default summary
					recovered := ParseFreeTextResult(output)
					recovered.TaskID = todoID
					recovered.Agent = agentName
					recovered.Source = "recovered_protocol"
					c.storeSubmittedTaskResult(todoID, recovered)
					typedRes = recovered
				}
				if err == nil && typedRes != nil && typedRes.Source == "submitted" {
					if resultErr := validateSubmittedTaskResult(typedRes); resultErr != nil {
						err = resultErr
					}
				}
			}
			if err == nil {
				if verr := validateTaskOutput(task, output); verr != nil {
					err = fmt.Errorf("task completion validation failed: %w", verr)
					c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("completion validation failed: %v", verr)).withTodoID(todoID))
				}
			}
			coordinatorOutput := output
			// A receipt transcript is also captured for summary-mode tasks that
			// require a typed result, but that must not change the task's output
			// contract. Only an explicit verbatim task is reduced to a manifest.
			if err == nil && taskUsesVerbatimTranscript(task) && transcript != nil {
				coordinatorOutput, err = finalizeVerbatimTaskResult(transcript, typedRes)
				if err != nil {
					err = fmt.Errorf("verbatim transcript validation failed: %w", err)
				} else if typedRes != nil {
					c.storeSubmittedTaskResult(todoID, typedRes)
				}
			}
			// Deliverable verification: run an objective check before accepting
			// the agent's claim of success. A non-zero exit converts this into a
			// failure that flows into the normal retry path below.
			if err == nil && task.Verify != "" {
				if statusErr := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskVerifying, "running objective verification", ""); statusErr != nil {
					err = fmt.Errorf("enter verifying state: %w", statusErr)
				} else {
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					c.report(c.newEvent("verify_start").withAgent(agentName).withMessage(task.Verify).withTodoID(todoID))
				}
				if err == nil {
					verification, verr := c.verifyTaskDeliverableWithMode(parentCtx, agentDef, task.Verify, task.VerifyMode)
					if verification != nil {
						_ = c.taskTracker.TodoList().SetVerificationResult(todoID, verification)
					}
					if verr != nil {
						err = fmt.Errorf("deliverable verification failed (command %q): %w", task.Verify, verr)
						c.report(c.newEvent("verify_error").withAgent(agentName).withMessage(fmt.Sprintf("verification failed: %v", verr)).withTodoID(todoID))
					} else {
						c.report(c.newEvent("verify_done").withAgent(agentName).withMessage("objective verification passed").withTodoID(todoID))
					}
				}
			}
			// Adversarial verification: skeptic votes try to refute the result.
			// A refutation flows into the same retry path as a failed verify.
			if err == nil && task.AdversarialVerify > 0 && c.AgentPool().Sidecar() != nil {
				if averr := c.adversarialVerify(parentCtx, task, output); averr != nil {
					err = averr
					c.report(c.newEvent("skeptic").withAgent(agentName).withMessage(averr.Error()).withTodoID(todoID))
				} else {
					c.report(c.newEvent("skeptic").withAgent(agentName).withMessage(fmt.Sprintf("confirmed by %d skeptic vote(s)", len(skepticLenses(task.AdversarialVerify)))).withTodoID(todoID))
				}
			}
			if err == nil {
				if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "done", taskDesc, coordinatorOutput); err != nil {
					log.Printf("warning: failed to write task file: %v", err)
				}
				duration, modelTime, toolTime := timing.snapshot()
				if statusErr := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(coordinatorOutput, summaryMaxRunes), coordinatorOutput); statusErr != nil {
					closeTranscript()
					return "", fmt.Errorf("mark task done: %w", statusErr)
				}
				c.reconcileTaskStatusProjection()
				c.updateTodoTiming(todoID, modelTime, toolTime)
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("done").withAgent(agentName).withOutput(coordinatorOutput).withMessage("completed").withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
				c.recordExecutionEvent(todoID, agentName, attempt, "done", resolvedModel, time.Since(attemptStarted), usageFromSteps(steps))
				if task.Summarize {
					coordinatorOutput = c.summarizeOutput(parentCtx, coordinatorOutput)
				}
				c.autoWriteSTMASync(agentName, taskDesc, coordinatorOutput, "", true)
				if appliedHint != "" {
					c.persistReflexionLessonAsync(agentName, task.Goal, appliedHintTrigger, appliedHint, true, false)
				}
				closeTranscript()
				return coordinatorOutput, nil
			}
		}

		c.recordExecutionEvent(todoID, agentName, attempt, "error", resolvedModel, time.Since(attemptStarted), usageFromSteps(steps))
		if terminalBlocked {
			lastErr = err
			c.report(c.newEvent("step").withAgent(agentName).withMessage("stopping retries: an owned terminal session remains active or unknown").withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(err, "error"))
			closeTranscript()
			break
		}
		// A protocol-only failure after a non-replayable task (side effect or
		// AllowsReplay=false), or one whose recovery policy disallows retry, has
		// no safe automatic replay semantics. Preserve the transcript/evidence
		// and block for reconciliation rather than invoking worker tools again.
		if protocolFailure && (!IsTaskReplayable(task) || !c.protocolRepairAllowsRetry(task)) {
			lastErr = err
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(err, "protocol"))
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskBlocked, fmt.Sprintf("protocol result missing; automatic replay is not allowed (allows_replay=%v, side_effect=%s, recovery=%s); reconcile before retry: %v", task.Execution.AllowsReplay != nil && *task.Execution.AllowsReplay, task.SideEffect, task.Recovery, err))
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("step").withAgent(agentName).withMessage("stopping retries: protocol-only failure cannot be automatically replayed; reconciliation required").withTodoID(todoID))
			closeTranscript()
			break
		}

		// Step messages start at the first assistant turn; without re-adding
		// the prompt, a retry's history opens with an assistant message and
		// the model sees its past actions but never the original instruction
		// (some providers also reject histories that do not start with user).
		if len(conversationHistory) == 0 && len(steps) > 0 {
			conversationHistory = append(conversationHistory, fantasy.NewUserMessage(currentPrompt))
		}
		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		// Unfixable-verify detection: a wrong-polarity verify command (see
		// verifyTaskDeliverable) is set once by the coordinator at task
		// assignment and the worker has no way to edit its own task's verify
		// field — so it fails identically no matter what the worker does.
		// Retrying just burns an attempt re-running real (sometimes
		// destructive) work to reproduce a verification failure that was
		// never about the deliverable. Stop after the first occurrence
		// instead of waiting for sameFailure to catch the repeat.
		if attempt < maxRetries && isUnfixableVerifyFailure(err) {
			lastErr = err
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("stopping retries: attempt %d hit a verify command that cannot be fixed by retrying (wrong exit-code polarity)", attempt)).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(fmt.Errorf("verify command has unfixable wrong polarity after %d attempt(s): %w", attempt, err), "error"))
			closeTranscript()
			break
		}

		// Repeated-failure detection: if this attempt failed with the same error
		// as the previous one, retrying is unproductive — the agent is stuck
		// repeating the same action. Stop early instead of burning the remaining
		// retry budget on identical failures.
		if lastErr != nil && attempt < maxRetries && sameFailure(lastErr.Error(), err.Error()) {
			lastErr = err
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("stopping retries: attempt %d repeated the same failure", attempt)).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(fmt.Errorf("repeated failure after %d attempts: %w", attempt, err), "error"))
			closeTranscript()
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
			closeTranscript()
			break
		}
		closeTranscript()
	}

	_, modelTime, toolTime := timing.snapshot()
	c.updateTodoTiming(todoID, modelTime, toolTime)
	// No PersistFailure here: every failure path inside the loop has already
	// persisted this error; persisting again wrote duplicate journal/status
	// records for the same failure.
	c.autoWriteSTMASync(agentName, taskDesc, "", lastErr.Error(), false)
	if maxRetries > 1 {
		c.persistReflexionLessonAsync(agentName, task.Goal, lastErr.Error(), appliedHint, false, isUnfixableVerifyFailure(lastErr))
	}
	failErr := fmt.Errorf("agent %q failed after %d attempt(s) (model: %s): %w", agentName, attemptsMade, resolvedModel, lastErr)
	if strings.TrimSpace(lastOutput) != "" {
		failErr = fmt.Errorf("%w\n\nLast agent output before failure (may contain useful findings):\n%s", failErr, utils.TruncateRunes(lastOutput, 2000))
	}
	return "", failErr
}

func classifyTaskFailure(err error) TaskFailureClass {
	if err == nil {
		return FailureExecution
	}
	if isTaskTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "protocol") || strings.Contains(msg, "empty output") {
		return FailureProtocol
	}
	if strings.Contains(msg, "verification") || strings.Contains(msg, "deliverable") {
		return FailureVerify
	}
	if strings.Contains(msg, "policy") || strings.Contains(msg, "blocked") {
		return FailurePolicy
	}
	return FailureExecution
}

// rescueFinalSummary gives an agent that stopped without a final message one
// tool-free turn (its full step history attached) to summarize what it did.
// Returns "" when the rescue itself fails or produces nothing.
func (c *Coordinator) rescueFinalSummary(ctx context.Context, ag fantasy.Agent, agentName string, steps []fantasy.StepResult, timing *taskTiming) string {
	if ctx.Err() != nil {
		return ""
	}
	var history []fantasy.Message
	for _, step := range steps {
		history = append(history, step.Messages...)
	}
	c.report(c.newEvent("step").withAgent(agentName).withMessage("agent stopped without a final message; requesting a summary turn"))
	summary, _, err := c.runAgentWithStatusAndHistory(ctx, ag, agentName,
		"You stopped before writing a final message (the step limit was likely reached). Do NOT call any tools. Based on the work above, write your final report now: what you did, what you found (including partial results and errors), and what remains to be done.",
		history, timing, fantasy.StepCountIs(1))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(summary)
}

func isTaskTimeout(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded")
}

func (c *Coordinator) executeSidecarTask(ctx context.Context, task TaskDef, todoID string) (string, error) {
	s := c.AgentPool().Sidecar()
	if s == nil {
		// Sidecar not configured: gracefully fall back to normal agent execution
		log.Printf("[INFO] sidecar not configured for task %q, falling back to normal agent execution", task.Goal)
		return c.executeTask(ctx, task, todoID)
	}

	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}
	attemptStarted := time.Now()
	c.recordExecutionEvent(todoID, task.Agent, 1, "in_progress", c.sidecarModel, 0, ExecutionUsage{})

	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.reconcileTaskStatusProjection()
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
		c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
		fmt.Fprintf(os.Stderr, "warning: sidecar execute failed for agent %q: %v\n", task.Agent, err)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.reconcileTaskStatusProjection()
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return "", fmt.Errorf("sidecar execution failed (model: %s): %w", c.sidecarModel, err)
	}
	if verr := validateTaskOutput(task, result); verr != nil {
		c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, verr.Error())
		c.reconcileTaskStatusProjection()
		return "", fmt.Errorf("task completion validation failed: %w", verr)
	}
	if task.Verify != "" {
		if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskVerifying, "running objective verification", ""); err != nil {
			c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
			return "", err
		}
		c.report(c.newEvent("verify_start").withAgent(task.Agent).withMessage(task.Verify).withTodoID(todoID))
		verification, verifyErr := c.verifyTaskDeliverableWithMode(ctx, nil, task.Verify, task.VerifyMode)
		if verification != nil {
			_ = c.taskTracker.TodoList().SetVerificationResult(todoID, verification)
		}
		if verifyErr != nil {
			c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, verifyErr.Error())
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("verify_error").withAgent(task.Agent).withMessage(verifyErr.Error()).withTodoID(todoID))
			return "", fmt.Errorf("deliverable verification failed (command %q): %w", task.Verify, verifyErr)
		}
		c.report(c.newEvent("verify_done").withAgent(task.Agent).withMessage("objective verification passed").withTodoID(todoID))
	}

	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(result, summaryMaxRunes), result); err != nil {
		c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
		return "", err
	}
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(task.Agent).withOutput(result).withMessage("sidecar completed").withTodoID(todoID))
	c.recordExecutionEvent(todoID, task.Agent, 1, "done", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
	return result, nil
}

// lastToolCallEntry tracks the most recent tool call for deadloop detection.
// Used by runAgentWithStatusAndHistory to detect stuck agents repeating the
// same failing tool call.
type lastToolCallEntry struct {
	toolName string
	input    string
}

// withEffectiveToolsAllowed attaches the tools permitted for a worker run.
// Team-level tools and the agent's declared tools are both explicit grants.
// Leaving an empty policy unset preserves deny-by-default behavior.
func (c *Coordinator) withEffectiveToolsAllowed(ctx context.Context, def *agent.AgentDef) context.Context {
	if c == nil || c.session == nil {
		return ctx
	}

	allowed := append([]string(nil), c.session.Config.ToolsAllowed...)
	if def != nil {
		for _, name := range strings.Split(def.Tools, ",") {
			if name = strings.TrimSpace(name); name != "" {
				allowed = append(allowed, name)
			}
		}
	}
	if len(allowed) == 0 {
		return ctx
	}
	return context.WithValue(ctx, tools.AgentToolsAllowedKey, allowed)
}

// protocolRepairAllowsRetry applies the task's existing recovery policy to a
// protocol-only failure. An omitted policy is resolved using the same policy
// engine as interrupted-task recovery, so protocol repair cannot silently
// turn manual/reconcile/never policies into worker replays.
func (c *Coordinator) protocolRepairAllowsRetry(task TaskDef) bool {
	policy := ResolveRecoveryPolicy(task.Recovery, task.SideEffect, c != nil && c.unattended, c.ExecutionProfile())
	return policy == RecoveryRetry
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
	// Delta-logging state for llm.log: how many messages this stream has
	// already logged, and the approximate size of the latest request (used to
	// estimate tokens when the provider reports none).
	var llmLogMu sync.Mutex
	loggedMsgs := 0
	lastReqBytes := 0
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
			// Keep this request within the context model token budget.
			modelID := opts.Model.Model()
			spec := globalRegistry.GetSpec(modelID)
			budget := c.ContextCompiler().CalculateBudget(spec, 0, 0)
			if capped := CapStepMessagesWithCounter(ctx, defaultCounter, modelID, opts.Messages, budget.Available); capped != nil {
				opts.Messages = capped
				llmLogMu.Lock()
				loggedMsgs, lastReqBytes = llmLogRequest(logWrite, opts, capped, loggedMsgs)
				llmLogMu.Unlock()
				return ctx, fantasy.PrepareStepResult{Messages: capped}, nil
			}
			llmLogMu.Lock()
			loggedMsgs, lastReqBytes = llmLogRequest(logWrite, opts, opts.Messages, loggedMsgs)
			llmLogMu.Unlock()
			return ctx, fantasy.PrepareStepResult{}, nil
		},
		OnStepStart: func(stepNumber int) error {
			reportFn(c.newEvent("step").withAgent(agentName).withTodoID(todoID).withStep(stepNumber).withMessage(fmt.Sprintf("step %d", stepNumber)))
			c.SetCurrentStep(stepNumber)
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			timing.beginTool()
			if transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript); transcript != nil {
				if err := transcript.RecordToolCall(tc.ToolCallID, tc.ToolName, tc.Input); err != nil {
					return err
				}
			}
			argsPreview := tc.Input
			if len(argsPreview) > 10000 {
				r := []rune(argsPreview)
				if len(r) > 10000 {
					argsPreview = string(r[:10000]) + "..."
				}
			}
			reportFn(c.newEvent("tool_call").withAgent(agentName).withTodoID(todoID).withTool(tc.ToolName, argsPreview))
			llmLogStreamEvent(logWrite, "tool_call", formatToolCallContent(tc))
			audit.LogToolCall(agentName, tc.ToolName, tc.Input, tc.ToolCallID)
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
			if transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript); transcript != nil {
				if err := transcript.RecordToolResult(tr.ToolCallID, tr.ToolName, resultPreview, isErrResult); err != nil {
					return err
				}
			}
			audit.LogToolResult(agentName, tr.ToolName, resultPreview, isErrResult, tr.ToolCallID)

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
			llmLogMu.Lock()
			reqBytes := lastReqBytes
			llmLogMu.Unlock()
			llmLogStreamFinish(logWrite, finishReason, usage, reqBytes)

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
	if err != nil && IsContextOverflowError(err) {
		reportFn(c.newEvent("text").withAgent(agentName).withTodoID(todoID).withMessage("context overflow detected; triggering emergency history compaction and retrying step"))
		modelID, _ := ctx.Value(modelKey{}).(string)
		if capped := CapStepMessagesWithCounter(ctx, defaultCounter, modelID, streamCall.Messages, 15000); capped != nil {
			streamCall.Messages = capped
		}
		result, err = ag.Stream(ctx, streamCall)
	}
	c.SetCurrentStage("idle")
	if result != nil {
		c.addStepTokens(result.Steps)
	}
	if err != nil {
		return "", nil, err
	}
	return result.Response.Content.Text(), result.Steps, nil
}

// toolUsageNotes builds a "Tool Notes" prompt section warning agents away
// from tool-usage mistakes real runs showed they repeat every task: prefixing
// bash commands with sudo/ssh (rejected by a guardrail, wasting a round-trip
// each time) and hand-rolled sleep+recheck polling loops (each poll is a full
// LLM round-trip; wait_for collapses the wait into one tool call).
func toolUsageNotes(toolNames string) string {
	has := func(name string) bool {
		if toolNames == "" || toolNames == "all" {
			return true
		}
		for _, t := range strings.Split(toolNames, ",") {
			if strings.TrimSpace(t) == name {
				return true
			}
		}
		return false
	}
	var notes []string
	if has("bash") {
		var privileged []string
		if has("sudo") {
			privileged = append(privileged, "sudo")
		}
		if has("ssh") {
			privileged = append(privileged, "ssh")
		}
		if len(privileged) > 0 {
			notes = append(notes, fmt.Sprintf("- The bash tool REJECTS %s commands. Run privileged/remote commands through the dedicated %s tool(s) directly (no prefix needed there).",
				strings.Join(privileged, " and "), strings.Join(privileged, "/")))
		}
	}
	if has("wait_for") && (has("bash") || has("sudo")) {
		notes = append(notes, "- When waiting for a state change (VM boot, service ready, async job completion), call `wait_for` once instead of looping bash sleep + status checks — it polls internally and returns when the condition is met, with the last output on timeout.")
	}
	if len(notes) == 0 {
		return ""
	}
	return "\n\n## Tool Notes\n\n" + strings.Join(notes, "\n")
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
func (c *Coordinator) verifyTaskDeliverable(parentCtx context.Context, agentDef *agent.AgentDef, command string) (*VerificationResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, nil
	}

	shell := "sh"
	if agentDef != nil && agentDef.Shell != "" {
		shell = agentDef.Shell
	} else if c != nil && c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}

	// A task that spent its whole budget (e.g. waiting on consent) reaches
	// here with parentCtx already expired; running the command on that
	// context kills it at 0s and reports a misleading "verification timed
	// out after 0s" that masks the real failure. Say what actually happened.
	if err := parentCtx.Err(); err != nil {
		return nil, fmt.Errorf("task deadline exceeded before the verify command could run: %w", err)
	}

	timeout := c.verifyTaskTimeout()
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, shell, "-c", command)
	cmd.Env = utils.SanitizeSubprocessEnv(os.Environ())
	if c.projectDir != "" {
		cmd.Dir = c.projectDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()
	result := &VerificationResult{
		Command:  command,
		WorkDir:  c.verificationWorkDir(),
		ExitCode: 0,
		Stdout:   utils.TruncateString(strings.TrimSpace(stdout.String()), 2000),
		Stderr:   utils.TruncateString(strings.TrimSpace(stderr.String()), 2000),
		Duration: time.Since(started),
		TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
	if err != nil {
		result.ExitCode = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		}
		detail := strings.TrimSpace(strings.Join([]string{result.Stdout, result.Stderr}, "\n"))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		if result.TimedOut {
			// Distinguish the verify command being slow from the task's own
			// deadline expiring mid-verify: only the former should be blamed
			// on the verify command.
			if parentErr := parentCtx.Err(); parentErr != nil && result.Duration < timeout {
				return result, fmt.Errorf("task deadline exceeded while the verify command was running (killed after %s): %w", result.Duration.Round(time.Millisecond), parentErr)
			}
			return result, fmt.Errorf("verification timed out after %s%s", result.Duration.Round(time.Millisecond), detail)
		}
		// CommandContext returns an ExitError when it kills a process after the
		// parent context is cancelled. That is not an observed non-zero command
		// exit: callers must be able to distinguish it from a real result.
		if parentErr := parentCtx.Err(); parentErr != nil {
			return result, fmt.Errorf("task context ended while the verify command was running: %w", parentErr)
		}
		if result.ExitCode == 127 {
			// A coordinator model once filled verify with a natural-language
			// sentence, which sh dutifully failed with "command not found".
			return result, fmt.Errorf("%v%s — exit 127 means the command was not found: the verify field must be a runnable shell command (e.g. 'test -f report.md'), not a natural-language description of the expected outcome", err, detail)
		}
		// Detect verify field filled with natural-language text (e.g. Chinese
		// sentences): these produce exit 1 with an "unexpected data" error from
		// the tool being invoked. Identify this by non-ASCII characters in the
		// verify command itself.
		if containsNonASCII(command) {
			return result, fmt.Errorf("%w%s — the verify field appears to contain non-ASCII text (possibly natural language). The verify field must be a runnable shell command, e.g. 'test -f report.md' or 'virsh list --all | grep -c running', not a description of the expected outcome", err, detail)
		}
		// Detect wrong-polarity verify for cleanup/delete tasks: the command
		// used grep/grep-c to assert a resource EXISTS, but the task deleted
		// it, so grep exits 1 with stdout "0" (or empty). A successful cleanup
		// should use `! grep -q ...` or `! grep -c ...` so that absence → exit 0.
		if result.ExitCode == 1 && strings.TrimSpace(result.Stdout) == "0" &&
			(strings.Contains(command, "grep -c") || strings.Contains(command, "grep-c")) {
			return result, fmt.Errorf("%w%s — wrong polarity: the verify command checked that a resource EXISTS (grep-c returned 0 = not found), but this looks like a cleanup task where success means the resource is GONE. Use '!' negation for delete/cleanup verify, e.g. '! ovs-vsctl show 2>&1 | grep -q br-verify'", err, detail)
		}
		return result, fmt.Errorf("%w%s", err, detail)
	}
	return result, nil
}

func (c *Coordinator) verifyTaskDeliverableWithMode(ctx context.Context, agentDef *agent.AgentDef, command, mode string) (*VerificationResult, error) {
	result, err := c.verifyTaskDeliverable(ctx, agentDef, command)
	switch mode {
	case "", "success":
		return result, err
	case "expected_failure":
		if isExpectedVerificationExit(err, result) {
			return result, nil
		}
		if err == nil {
			return result, fmt.Errorf("verification expected a non-zero exit but succeeded")
		}
		return result, err
	case "observation":
		// Observation captures a command's normal output regardless of its exit
		// status, but a timeout, cancellation, or failure to launch the command
		// produced no valid observation and must still fail the task.
		if err == nil || isExpectedVerificationExit(err, result) {
			return result, nil
		}
		return result, err
	default:
		return result, fmt.Errorf("invalid verify_mode %q", mode)
	}
}

// isExpectedVerificationExit reports the one failure class that verification
// modes may intentionally accept: a command that started and exited non-zero.
// In particular, an ExitCode alone is insufficient: commands killed by a
// deadline can also leave one behind.
func isExpectedVerificationExit(err error, result *VerificationResult) bool {
	if err == nil || result == nil || result.TimedOut || result.ExitCode == 0 {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

func (c *Coordinator) verificationWorkDir() string {
	if c != nil && c.projectDir != "" {
		return c.projectDir
	}
	return "the hufu process working directory"
}

// containsNonASCII reports whether s contains any character outside the
// printable ASCII range (0x20-0x7E). Used to detect when the coordinator
// model accidentally filled the verify field with a natural-language sentence
// (e.g. Chinese text) instead of a runnable shell command.
func containsNonASCII(s string) bool {
	for _, r := range s {
		if r > 0x7E || (r < 0x20 && r != '\t' && r != '\n') {
			return true
		}
	}
	return false
}

func completionVerificationInstructions(command, workDir string) string {
	return fmt.Sprintf("\n\n## Completion Verification\n\nYour work is not accepted until this exact command succeeds. Create the deliverable at the exact path it checks, then run the command yourself before your final response.\n\n- Command: `%s`\n- Runs from: `%s`\n- If it fails, fix the deliverable instead of only describing the intended result.\n", strings.TrimSpace(command), workDir)
}

func (c *Coordinator) verifyTaskTimeout() time.Duration {
	if c == nil || c.session == nil {
		return 120 * time.Second
	}
	timeout := time.Duration(c.session.Config.VerifyTimeout) * time.Second
	if timeout <= 0 {
		return 120 * time.Second
	}
	return timeout
}

func (c *Coordinator) reflectOnFailure(ctx context.Context, agentName, goal, lastErr string) string {
	s := c.AgentPool().Sidecar()
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

// isUnfixableVerifyFailure reports whether err comes from the "wrong
// polarity" verify-command detection in verifyTaskDeliverable (a
// grep/grep-c-based cleanup check that asserts a resource EXISTS instead of
// asserting it's GONE). The task.Verify command is fixed by the coordinator
// at task-assignment time — the worker executing the task has no way to
// edit its own verify field — so this failure is guaranteed to recur
// identically on every retry regardless of what the worker does.
func isUnfixableVerifyFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "wrong polarity")
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
		if strings.Contains(e, "non-ascii") || strings.Contains(e, "natural language") {
			return "The verify field in your task contained natural-language text (possibly Chinese) instead of a runnable shell command. Fix the verify field to be a shell command, e.g. 'test -f workspace/report.md' or 'virsh list --all | grep -c running'. Do NOT put human-readable descriptions in the verify field."
		}
		if strings.Contains(e, "wrong polarity") {
			return "The verify field used the wrong polarity for a cleanup/delete task. When a task DELETES a resource, success means the resource is GONE — use '!' negation so absence exits 0: e.g. '! ovs-vsctl show 2>&1 | grep -q br-verify' or '! virsh dominfo vm-name 2>&1 | grep -q running'. Without '!', grep-c returning 0 (not found) makes the shell exit 1 and falsely fails a successful cleanup."
		}
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

// validateTaskModel enforces the team-level model-list against a task.
// With no model-list configured, the task's Model is cleared so the agent's
// own model selection wins. With one, only listed model IDs are accepted.
func (c *Coordinator) validateTaskModel(task *TaskDef) error {
	if len(c.modelList) == 0 {
		task.Model = ""
		return nil
	}
	if task.Model == "" {
		return nil
	}
	var validIDs []string
	for _, m := range c.modelList {
		if m.ID == task.Model {
			return nil
		}
		validIDs = append(validIDs, m.ID)
	}
	return fmt.Errorf("unknown model %q for agent %q (valid models: %v)", task.Model, task.Agent, validIDs)
}

// createTaskAgent builds the fantasy.Agent for a task, branching on plan-first
// mode. Plan-first tasks get a fresh agent with a submit_plan tool; normal
// tasks reuse the cached agent. All failure paths report and persist before
// returning the error so the caller does not have to.
func (c *Coordinator) createTaskAgent(parentCtx context.Context, agentDef *agent.AgentDef, task TaskDef, resolvedModel, todoID, taskDesc, agentName string) (fantasy.Agent, error) {
	resultTool := &submitResultTool{coordinator: c, todoID: todoID}
	if task.PlanFirst && task.PlanID == "" {
		planAg, planErr := agent.CreateAgent(parentCtx, c.providerManager.GetProvider(resolvedModel), agent.AgentConfig{
			Def:        agentDef,
			TeamConfig: &c.session.Config,
			WorkDir:    c.projectDir,
			MaxSteps:   agent.DefaultMaxSteps,
		}, append(agent.SelectTools(c.coreTools, agentDef.Tools), &submitPlanTool{coordinator: c, todoID: todoID}, resultTool))
		if planErr != nil {
			c.report(c.newEvent("error").withAgent(agentName).withMessage(planErr.Error()).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(planErr, ""))
			return nil, planErr
		}
		return planAg, nil
	}
	ag, err := c.createTaskAgentWithResultTool(parentCtx, agentDef, task.Model, resultTool)
	if err != nil {
		c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()).withTodoID(todoID))
		c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(err, ""))
		return nil, err
	}
	return ag, nil
}

func (c *Coordinator) createTaskAgentWithResultTool(ctx context.Context, def *agent.AgentDef, overrideModel string, resultTool *submitResultTool) (fantasy.Agent, error) {
	agentDef := def
	if overrideModel != "" {
		overriddenDef := *def
		overriddenDef.Generation.Model = overrideModel
		agentDef = &overriddenDef
	}

	agentDef = c.injectWorkerContext(ctx, agentDef)
	ctx = tools.SetSSHSessionManager(ctx, c.sshSessionMgr)

	agentTools := agent.SelectTools(c.coreTools, agentDef.Tools)
	if c.mcpManager != nil {
		agentTools = append(agentTools, c.mcpManager.AsAgentTools()...)
		if len(agentDef.MCPTools) > 0 {
			err := c.mcpManager.LoadAgentMCPServer(agentDef.Name, agentDef.MCPTools, agentDef.Shell)
			if err != nil {
				return nil, fmt.Errorf("failed to load MCP server for agent %s: %w", agentDef.Name, err)
			}
			mcpTools := c.mcpManager.GetAgentMCPTools(agentDef.Name, agentDef.Shell)
			if len(mcpTools) > 0 {
				agentTools = append(agentTools, mcpTools...)
			}
		}
	}
	if resultTool != nil {
		agentTools = append(agentTools, resultTool)
	}

	getAgModelID := c.resolveAgentModel(agentDef, "")
	return agent.CreateAgent(ctx, c.providerManager.GetProvider(getAgModelID), agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultMaxSteps,
	}, agentTools)
}
