package team

// Top-level run loops: the orchestrator loop, direct @agent runs, and
// think-mode event emission.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/memory"
	"github.com/anomalyco/hufu/internal/skill"
	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

var directAgentPattern = regexp.MustCompile(`^@(\w[\w-]*)\s+(.+)$`)

func ParseDirectAgent(prompt string) (agentName string, task string, ok bool) {
	m := directAgentPattern.FindStringSubmatch(prompt)
	if m == nil {
		return "", "", false
	}
	return strings.ToLower(m[1]), m[2], true
}

func (c *Coordinator) RunDirectAgent(ctx context.Context, agentName string, task string) (*DirectAgentResult, error) {
	endExecutionRun := c.beginExecutionRun()
	defer endExecutionRun()
	if err := c.ValidateWorkspaceIsolation(); err != nil {
		return nil, err
	}
	if err := c.ValidateResourceLocks(ctx); err != nil {
		return nil, err
	}
	agentDef, _, err := c.AgentPool().ResolveAgentName(agentName)
	if err != nil {
		return nil, err
	}

	c.setAutoLoadedSkills(c.matchSkillsWithSidecar(ctx, task))

	resolvedName := strings.ToLower(agentDef.Name)
	directModel := c.resolveAgentModel(agentDef, "")

	todoItems := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: resolvedName, Desc: task, Model: directModel, Source: TaskSourceCoordinator, ParentID: ""}})
	todoID := todoItems[0].ID
	attemptStarted := time.Now()
	c.recordExecutionEvent(todoID, resolvedName, 1, "in_progress", directModel, 0, ExecutionUsage{})
	c.taskTracker.TodoList().UpdateStatus(todoID, TaskInProgress, "")
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(resolvedName).withMessage(task).withModel(directModel).withTodoID(todoID))
	prevAgent := c.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	prevTask := c.getSnapshotField(func(s *currentSnapshot) string { return s.Task })
	prevTodoID := c.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })
	c.updateSnapshot(func(s *currentSnapshot) { s.Agent = resolvedName })
	c.updateSnapshot(func(s *currentSnapshot) { s.Task = task })
	c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = todoID })
	defer func() {
		c.updateSnapshot(func(s *currentSnapshot) { s.Agent = prevAgent })
		c.updateSnapshot(func(s *currentSnapshot) { s.Task = prevTask })
		c.updateSnapshot(func(s *currentSnapshot) { s.TodoID = prevTodoID })
	}()

	ag, err := c.getOrCreateAgent(ctx, agentDef, "")
	if err != nil {
		c.recordExecutionEvent(todoID, resolvedName, 1, "error", directModel, time.Since(attemptStarted), ExecutionUsage{})
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return nil, fmt.Errorf("failed to create agent %q: %w", resolvedName, err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	taskCtx, cancel := tools.WithInteractiveAwareTimeout(ctx, agentTimeout)
	defer cancel()

	taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
	taskCtx = context.WithValue(taskCtx, modelKey{}, directModel)
	taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, resolvedName)
	taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, resolvedName)
	taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
	taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, task)
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

	timing := &taskTiming{}
	timing.reset()

	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "working", task, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "working", task)

	prompt := c.appendSkillContext(task, agentDef, resolvedName, task, todoID)

	// Phase 2 keeps the legacy prompt path authoritative. Compile a shadow
	// bundle for comparison only; a compiler failure must never affect a task.
	workerInput := WorkerContextInput{TaskGoal: prompt, TaskDef: TaskDef{Agent: resolvedName, Goal: task}, AgentDef: agentDef,
		RawSTM: LoadSTM(c.session.Workspace), RawLTM: LoadLTM(c.session.Workspace, c.session.Config.Name), MemoryStore: c.memoryStore,
		ModelContext: globalRegistry.GetSpec(c.resolveAgentModel(agentDef, "")), MaxAuxChars: maxWorkerAuxContextChars,
		DisableMemory: c.ExecutionProfile().DisableHistoricalMemory}
	if suffix := c.buildMemorySuffix(agentDef.Role); suffix != "" {
		prompt += "\n\n" + suffix
	}
	c.compileShadowWorker(taskCtx, workerInput, prompt)

	output, steps, err := c.runAgentWithStatusAndHistory(taskCtx, ag, resolvedName, prompt, nil, timing)
	duration, modelTime, toolTime := timing.snapshot()
	if err == nil && c.terminalSessionMgr != nil {
		if terminalErr := c.terminalSessionMgr.RequireTaskClosed(todoID); terminalErr != nil {
			err = terminalErr
		}
	}
	if err != nil {
		c.recordExecutionEvent(todoID, resolvedName, 1, "error", directModel, time.Since(attemptStarted), usageFromSteps(steps))
		c.PersistFailure(resolvedName, task, todoID, c.FailureDetail(err, ""))
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(resolvedName).withMessage(err.Error()).withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
		return &DirectAgentResult{AgentName: resolvedName, Error: err, Steps: len(steps)}, nil
	}

	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "done", task, output); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "done", task)
	c.taskTracker.TodoList().UpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output)
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(resolvedName).withOutput(output).withMessage("completed").withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
	c.recordExecutionEvent(todoID, resolvedName, 1, "done", directModel, time.Since(attemptStarted), usageFromSteps(steps))

	return &DirectAgentResult{AgentName: resolvedName, Output: output, Steps: len(steps)}, nil
}

// coordinatorCoreToolNames are the core tools the coordinator always gets,
// regardless of team.yaml. The read-only file tools (view/grep/glob/ls) let it
// consult files directly instead of burning a full delegation round asking a
// worker to cat a document.
var coordinatorCoreToolNames = map[string]bool{
	"ask_user":   true,
	"stm_write":  true,
	"ltm_update": true,
	"view":       true,
	"grep":       true,
	"glob":       true,
	"ls":         true,
}

// coordinatorAllowedToolNames returns the permission allowlist matching
// coordinatorCoreToolNames for the orchestrator context.
func coordinatorAllowedToolNames() []string {
	names := make([]string, 0, len(coordinatorCoreToolNames))
	for name := range coordinatorCoreToolNames {
		names = append(names, name)
	}
	return names
}

func (c *Coordinator) buildOrchestratorTools() []fantasy.AgentTool {
	if c.forcePlanFirst {
		orchTools := []fantasy.AgentTool{
			c.RunAgentsTool(),
			&finishTool{coordinator: c},
			&loadSkillTool{coordinator: c},
			&saveSkillTool{coordinator: c},
		}
		for _, t := range c.coreTools {
			if t.Info().Name == "stm_write" {
				orchTools = append(orchTools, &stmWriteTool{coordinator: c, allowReplace: true})
				continue
			}
			if coordinatorCoreToolNames[t.Info().Name] {
				orchTools = append(orchTools, t)
			}
		}
		return orchTools
	}
	orchTools := []fantasy.AgentTool{
		c.RunAgentsTool(),
		&finishTool{coordinator: c},
		&approvePlanTool{coordinator: c},
		&modifyPlanTool{coordinator: c},
		&rejectPlanTool{coordinator: c},
		&loadSkillTool{coordinator: c},
		&saveSkillTool{coordinator: c},
	}
	for _, t := range c.coreTools {
		if t.Info().Name == "stm_write" {
			orchTools = append(orchTools, &stmWriteTool{coordinator: c, allowReplace: true})
			continue
		}
		if coordinatorCoreToolNames[t.Info().Name] {
			orchTools = append(orchTools, t)
		}
	}
	return orchTools
}

func (c *Coordinator) runOrchestrator(ctx context.Context, orchDef *agent.AgentDef, prompt string) (string, []fantasy.StepResult, error) {
	coordinatorTimeout := time.Duration(c.session.Config.Timeout) * time.Second * time.Duration(c.session.Config.MaxRounds+1)
	if orchDef.Timeout > 0 {
		coordinatorTimeout = time.Duration(orchDef.Timeout) * time.Second
	}

	orchCtx, cancel := tools.WithInteractiveAwareTimeout(ctx, coordinatorTimeout)
	defer cancel()
	orchCtx = context.WithValue(orchCtx, todoIDKey{}, CoordTodoID)
	// The coordinator's built-in tools are always permitted, independent of
	// team.yaml: without this the permission gate denies the forced read-only
	// tools and the coordinator is back to delegating every file read.
	orchCtx = context.WithValue(orchCtx, tools.AgentToolsAllowedKey, coordinatorAllowedToolNames())
	if c.unattended {
		orchCtx = context.WithValue(orchCtx, tools.UnattendedKey, true)
		orchCtx = context.WithValue(orchCtx, tools.AskUserChoiceSelectorKey, tools.AskUserChoiceSelector(func(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
			return c.chooseAskUserResponse(ctx, question, qtype, opts, allowAny)
		}))
	}
	if c.autoApprove {
		orchCtx = context.WithValue(orchCtx, tools.AutoApproveKey, true)
	}

	orchModelID := c.resolveAgentModel(orchDef, "")
	orch, err := agent.CreateAgent(orchCtx, c.providerManager.GetProvider(orchModelID), agent.AgentConfig{
		Def:        orchDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultCoordinatorMaxSteps,
	}, c.buildOrchestratorTools())
	if err != nil {
		return "", nil, fmt.Errorf("failed to create coordinator: %w", err)
	}

	c.conversationHistoryMu.Lock()
	historySnapshot := make([]fantasy.Message, len(c.conversationHistory))
	copy(historySnapshot, c.conversationHistory)
	c.conversationHistoryMu.Unlock()

	return c.runAgentWithStatusAndHistory(orchCtx, orch, orchDef.Name, prompt, historySnapshot, &taskTiming{})
}

// ensureFinished forces one wrap-up turn when the orchestrator stream ended
// without the finish tool — typically the per-turn step cap ran out
// mid-coordination. Without this the last narration text is silently returned
// as the "final" answer and tool results delivered on the final step are never
// read (a real run once idled ~10h this way, with the TUI showing "waiting for
// reboot" while the verifier had already reported success).
//
// The original steps are appended to history here so the wrap-up turn can see
// them; on success only the wrap-up steps are returned so the caller's
// saveHistoryAndSession does not append the originals twice.
func (c *Coordinator) ensureFinished(ctx context.Context, orchDef *agent.AgentDef, result string, steps []fantasy.StepResult) (string, []fantasy.StepResult) {
	if c.finishCalled.Load() || ctx.Err() != nil {
		return result, steps
	}
	c.report(c.newEvent("wrap_up_phase").withMessage("coordinator stopped without calling finish (step limit likely reached); forcing a final summary turn").withTodoID(CoordTodoID))
	c.saveHistoryAndSession(ctx, steps)
	wrapResult, wrapSteps, err := c.runOrchestrator(ctx, orchDef, stepLimitWrapUpPrompt)
	if err != nil || strings.TrimSpace(wrapResult) == "" {
		return result, nil // original steps already saved above
	}
	return wrapResult, wrapSteps
}

// attemptWrapUpRecovery converts a mid-run coordinator failure into a final
// summary when wrap-up was already pending (max rounds or budget exceeded).
// The limits only ask the model to call finish via a tool error; a model that
// keeps delegating instead gets aborted, which previously turned a budget
// limit into a hard run failure with no output. This backstop runs one
// constrained summary turn, and if even that fails, assembles a deterministic
// summary from completed task outputs so the run never ends empty-handed.
func (c *Coordinator) attemptWrapUpRecovery(ctx context.Context, orchDef *agent.AgentDef, runErr error) (string, []fantasy.StepResult, bool) {
	if !c.IsWrapUp() || ctx.Err() != nil {
		return "", nil, false
	}
	c.report(c.newEvent("wrap_up_phase").withMessage(fmt.Sprintf("coordinator stopped before finishing (%v); forcing a final summary turn", runErr)).withTodoID(CoordTodoID))
	result, steps, err := c.runOrchestrator(ctx, orchDef, wrapUpPromptTemplate)
	if err == nil && strings.TrimSpace(result) != "" {
		return strings.TrimPrefix(result, "FINISHED:"), steps, true
	}
	summary := c.summaryFromTodos(runErr)
	if summary == "" {
		return "", nil, false
	}
	return summary, nil, true
}

// summaryFromTodos builds an LLM-free run summary from the todo list. Used as
// the last-resort wrap-up when the coordinator model cannot produce one.
func (c *Coordinator) summaryFromTodos(runErr error) string {
	items := c.taskTracker.TodoList().Items()
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The run stopped early (%v). Results of completed tasks:\n", runErr)
	done := 0
	for _, item := range items {
		switch item.Status {
		case TaskDone:
			done++
			fmt.Fprintf(&b, "\n### %s: %s\n%s\n", item.Agent, item.Desc, utils.TruncateRunes(item.Output, summaryMaxRunes))
		case TaskError, TaskBlocked:
			fmt.Fprintf(&b, "\n### %s: %s\nFAILED: %s\n", item.Agent, item.Desc, item.Detail)
		}
	}
	if done == 0 {
		return ""
	}
	return b.String()
}

// recordRunAborted persists an honest trace of a run that died before
// producing a final answer: a session entry (so chat_history.md and a later
// `continue` show what happened instead of a user message with no reply) and
// a coordinator status line naming the abort. A real aborted run left
// nothing — the next session had no idea the previous one ended mid-flight.
func (c *Coordinator) recordRunAborted(runErr error) {
	reason := "run ended before completion"
	switch {
	case errors.Is(runErr, context.Canceled):
		reason = "run aborted (cancelled by user)"
	case errors.Is(runErr, context.DeadlineExceeded):
		reason = "run aborted (coordinator timeout)"
	}

	if c.session != nil && c.session.Workspace != "" {
		_ = writeStatusWithDetail(c.session.Workspace, "coordinator", "error", reason, fmt.Sprintf("error=%v", runErr))
	}

	if c.sessionData == nil {
		return
	}
	entry := fmt.Sprintf("[%s: %v]", reason, runErr)
	if summary := c.summaryFromTodos(runErr); summary != "" {
		entry += "\n\n" + summary
	}
	c.addSessionAssistantMessage(entry)
	if c.sessionData != nil {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}
}

func (c *Coordinator) finalizeRemainingTasks() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskInProgress, TaskPaused, TaskVerifying:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "coordinator ended unexpectedly")
			changed = true
		case TaskPending:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "")
			changed = true
		}
	}
	if changed {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
}

// finalizeNormalCompletion marks tasks that are still pending as skipped when
// the coordinator finishes successfully. Tasks in progress should not exist at
// this point since ExecuteTasks waits for all goroutines. Do not mark them done
// defensively: that would turn an incomplete task into a false success.
func (c *Coordinator) finalizeNormalCompletion() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskPending:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "")
			changed = true
		case TaskInProgress, TaskPaused, TaskVerifying:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskError, "coordinator finished before task completed")
			changed = true
		}
	}
	if changed {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}

	// Converge per-agent status files: after a successful finish, a worker's
	// status/<agent>.yml otherwise keeps its last mid-run state ("working",
	// "error" from a retried attempt), making a completed run look failed to
	// anyone inspecting the workspace afterwards.
	if c.session != nil && c.session.Workspace != "" {
		for _, def := range c.uniqueWorkerDefs() {
			_ = writeStatus(c.session.Workspace, strings.ToLower(def.Name), "idle", "run finished")
		}
		_ = writeStatus(c.session.Workspace, "coordinator", "idle", "run finished")
	}
}

func (c *Coordinator) emitThinkSkills(matched []*skill.SkillDef) {
	if len(matched) == 0 {
		c.report(c.newEvent("think_skills").withMessage("no skills matched (keyword fallback used)"))
		return
	}
	names := make([]string, len(matched))
	for i, s := range matched {
		names[i] = s.Name
	}
	c.report(c.newEvent("think_skills").withMessage(fmt.Sprintf("matched %d skills: %s", len(matched), strings.Join(names, ", "))))
	for _, s := range matched {
		c.report(c.newEvent("think_skill_detail").withAgent(s.Name).withMessage(s.Description))
	}
}

func (c *Coordinator) emitThinkAgents() {
	workers := c.uniqueWorkerDefs()
	var b strings.Builder
	fmt.Fprintf(&b, "available agents (%d): ", len(workers))
	for _, def := range workers {
		fmt.Fprintf(&b, "%s(%s), ", def.Name, def.Description)
	}
	msg := strings.TrimSuffix(b.String(), ", ")
	c.report(c.newEvent("think_agents").withMessage(msg))
}

func (c *Coordinator) emitThinkPrompt(systemPrompt string) {
	n := utf8.RuneCountInString(systemPrompt)
	c.report(c.newEvent("think_prompt").withMessage(fmt.Sprintf("system prompt assembled (%d chars)", n)))

	dumpPath := filepath.Join(c.session.Workspace, "think-prompt.md")
	if err := os.WriteFile(dumpPath, []byte(systemPrompt), 0o644); err == nil {
		c.report(c.newEvent("think_prompt_dump").withMessage("saved to " + dumpPath))
	}
}

func (c *Coordinator) emitThinkDelegation(agent, task, model string) {
	msg := fmt.Sprintf("delegating → %s ← %q [model: %s]", agent, task, model)
	c.report(c.newEvent("think_delegation").withAgent(agent).withMessage(msg))
}

func (c *Coordinator) emitThinkSidecar(action, detail string) {
	msg := fmt.Sprintf("%s: %s", action, detail)
	c.report(c.newEvent("think_sidecar").withMessage(msg))
}

func (c *Coordinator) buildSystemPrompt(ctx context.Context, orchDef *agent.AgentDef, prompt string, isContinuation bool) string {
	var systemPrompt string
	if orchDef != nil {
		systemPrompt = c.expandOrchestratorTemplate(orchDef.System)
	}
	if systemPrompt == "" {
		systemPrompt = c.expandOrchestratorTemplate(defaultOrchestratorSystem)
	}

	// Capture prompt-component texts for the §5.4 context budget breakdown.
	// coreText holds the orchestrator's own instructions (+ skills + reminder);
	// projectText and memoryText are tracked separately so the report can
	// attribute tokens to each context subsystem rather than one opaque blob.
	var coreText, projectText, memoryText strings.Builder
	coreText.WriteString(systemPrompt)

	var matchedSkills []*skill.SkillDef
	if isContinuation {
		matchedSkills = c.getAutoLoadedSkills()
		if c.autoSkillsEnabled && prompt != "" {
			matchedSkills = c.matchSkillsWithSidecar(ctx, prompt)
			c.setAutoLoadedSkills(matchedSkills)
		}
	} else {
		matchedSkills = c.matchSkillsWithSidecar(ctx, prompt)
		c.setAutoLoadedSkills(matchedSkills)
	}

	c.computeWorkerSummaries(ctx)

	if c.think && !isContinuation {
		c.emitThinkSkills(matchedSkills)
	}

	orchPrompt := c.BuildOrchestratorPrompt(matchedSkills...)
	systemPrompt += "\n\n" + orchPrompt
	coreText.WriteString("\n\n" + orchPrompt)

	if c.think && !isContinuation {
		c.emitThinkAgents()
	}

	var contextSummary string
	if !isContinuation && !c.ExecutionProfile().DisableHistoricalMemory && c.sessionData != nil && len(c.sessionData.Entries) > 1 && len(c.conversationHistory) == 0 {
		contextSummary = c.sessionData.ContextSummary()
	}

	var modelSpec ModelContextSpec
	if orchDef != nil {
		modelSpec = globalRegistry.GetSpec(c.resolveAgentModel(orchDef, ""))
	}

	coordInput := CoordinatorContextInput{
		Goal:             prompt,
		SessionContext:   contextSummary,
		RawSTM:           LoadSTM(c.session.Workspace),
		RawLTM:           LoadLTM(c.session.Workspace, c.session.Config.Name),
		MemoryStore:      c.memoryStore,
		SidecarCompacter: c.AgentPool().Sidecar(),
		ModelContext:     modelSpec,
		Role:             "coordinator",
		IsContinuation:   isContinuation,
		DisableMemory:    c.ExecutionProfile().DisableHistoricalMemory,
		ProjectContext:   c.loadProjectContext(),
	}

	// The compiler is intentionally shadow-only until canonical mode. Retain
	// the exact legacy assembly below so model-visible prompts do not change.
	if agentsMD := coordInput.ProjectContext; agentsMD != "" {
		agentsMD = compactLegacyProjectContext(ctx, c.AgentPool().Sidecar(), agentsMD)
		systemPrompt += "\n\n---\n## Project Context (AGENTS.md)\n\n" + agentsMD
		projectText.WriteString(agentsMD)
	}
	if c.memoryStore != nil && prompt != "" && !c.ExecutionProfile().DisableHistoricalMemory {
		var compactFn memory.CompactFunc
		if sidecar := c.AgentPool().Sidecar(); sidecar != nil {
			compactFn = sidecar.Compact
		}
		if memCtx, err := memory.AutoQuery(ctx, c.memoryStore, prompt, compactFn); err == nil && memCtx != "" {
			systemPrompt += "\n\n---\n" + memCtx
			memoryText.WriteString(memCtx + "\n")
		}
	}
	if !isContinuation && !c.ExecutionProfile().DisableHistoricalMemory && contextSummary != "" {
		systemPrompt += "\n\n---\n## Session Context\n\n" + contextSummary
		memoryText.WriteString(contextSummary + "\n")
	}
	if suffix := c.buildMemorySuffix("coordinator"); suffix != "" {
		systemPrompt += "\n\n" + suffix
		memoryText.WriteString(suffix + "\n")
	}
	if reminder := c.buildCoreReminder(orchDef); reminder != "" {
		systemPrompt += "\n\n" + reminder
		coreText.WriteString("\n\n" + reminder)
	}
	c.compileShadowCoordinator(ctx, coordInput, systemPrompt)

	if c.think && !isContinuation {
		c.emitThinkPrompt(systemPrompt)
	}

	// Record the model-aware token breakdown for the execution report (§5.4).
	c.recordContextBreakdown(ctx, c.resolveAgentModel(orchDef, ""),
		coreText.String(), projectText.String(), memoryText.String())

	return systemPrompt
}

// textCompacter is intentionally the sidecar's plain-text compaction API.
// CompactStructured summarizes conversations into JSON and must never be used
// for model-visible project instructions in the legacy prompt path.
type textCompacter interface {
	Compact(context.Context, string, string) (string, error)
}

func compactLegacyProjectContext(ctx context.Context, compacter textCompacter, projectContext string) string {
	if compacter == nil || len(projectContext) <= 4000 {
		return projectContext
	}
	compacted, err := compacter.Compact(ctx, projectContext, "Compress this project context while preserving all key facts, patterns, conventions, and instructions.")
	if err != nil || compacted == "" {
		return projectContext
	}
	return compacted
}

func (c *Coordinator) Run(ctx context.Context, userPrompt string) (string, error) {
	endExecutionRun := c.beginExecutionRun()
	defer endExecutionRun()
	if err := c.ValidateWorkspaceIsolation(); err != nil {
		return "", err
	}
	if err := c.ValidateResourceLocks(ctx); err != nil {
		return "", err
	}
	c.resetRoundState()
	c.lastStmWrite = time.Time{}
	if c.initialPrompt == "" {
		c.initialPrompt = userPrompt
	}

	// Validate configured model names once per coordinator. This is advisory:
	// we warn on mismatches but keep running so a stale provider list does not
	// block the whole team.
	c.validateModelsOnce.Do(func() {
		c.validateModelsErr = c.ValidateConfiguredModels(ctx)
	})
	if c.validateModelsErr != nil {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("model validation warning: %v", c.validateModelsErr)))
	}

	// Start cleanup daemon for idle SSH sessions on first Run call (30 minute timeout, check every 5 minutes)
	if c.sshSessionMgr != nil {
		c.sshSessionMgr.StartCleanupDaemon(ctx, 5*time.Minute, 30*time.Minute)
	}

	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	_ = EnsureWorkspaceDirs(c.session.Workspace)

	// Replay the persistent task journal (crash-safe complement to the
	// session.json checkpoint) and start appending to it for this run.
	c.initTaskJournal()

	c.report(c.newEvent("step").withMessage("coordinator preparing"))

	// Crash-resume: before delegating new work, re-drive any worker tasks that a
	// previous interrupted run left in-flight (restored from the checkpoint).
	// No-op on a fresh run (empty todo list) or with --new (fresh session).
	if n, err := c.ResumeInterruptedTasks(ctx); err != nil {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s), with errors: %v", n, err)))
	} else if n > 0 {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s) from checkpoint", n)))
	}

	c.addSessionUserMessage(userPrompt)

	systemPrompt := c.buildSystemPrompt(ctx, orchDef, userPrompt, false)

	// Apply the computed system prompt to a copy so shared state is not mutated.
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("coordinator starting").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestrator(ctx, &orchDefCopy, userPrompt)
	if err != nil {
		if recovered, recoveredSteps, ok := c.attemptWrapUpRecovery(ctx, &orchDefCopy, err); ok {
			result = recovered
			steps = append(steps, recoveredSteps...)
		} else {
			c.finalizeRemainingTasks()
			c.saveHistoryAndSession(ctx, steps)
			c.recordRunAborted(err)
			orchModel := c.resolveAgentModel(orchDef, "")
			c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator failed").withTodoID(CoordTodoID))
			return "", fmt.Errorf("coordinator failed (model: %s): %w", orchModel, err)
		}
	}

	result, steps = c.ensureFinished(ctx, &orchDefCopy, result, steps)

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	c.addSessionAssistantMessage(finalResult)
	if c.sessionData != nil {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator finished").withTodoID(CoordTodoID))
	c.SetCurrentStage("idle")
	return finalResult, nil
}

func (c *Coordinator) ContinueWithPrompt(ctx context.Context, additionalPrompt string) (string, error) {
	endExecutionRun := c.beginExecutionRun()
	defer endExecutionRun()
	// Capture before resetRoundState clears the flag, or the wrap-up branch
	// below can never trigger and wrap-up requests silently degrade into an
	// ordinary (empty-prompt) continuation turn.
	wasWrapUp := c.IsWrapUp()
	c.resetRoundState()

	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	var continuationPrompt string
	if wasWrapUp {
		continuationPrompt = wrapUpPromptTemplate
		additionalPrompt = "wrap up now"
		c.report(c.newEvent("wrap_up_phase").withMessage("coordinator summarizing").withTodoID(CoordTodoID))
	} else {
		continuationPrompt = fmt.Sprintf(continuationPromptTemplate, additionalPrompt)
		c.report(c.newEvent("step").withMessage("coordinator preparing").withTodoID(CoordTodoID))
	}

	systemPrompt := c.buildSystemPrompt(ctx, orchDef, additionalPrompt, true)
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.addSessionUserMessage(additionalPrompt)

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("continuing with additional input").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestrator(ctx, &orchDefCopy, continuationPrompt)
	if err != nil {
		if recovered, recoveredSteps, ok := c.attemptWrapUpRecovery(ctx, &orchDefCopy, err); ok {
			result = recovered
			steps = append(steps, recoveredSteps...)
		} else {
			c.finalizeRemainingTasks()
			c.saveHistoryAndSession(ctx, steps)
			c.recordRunAborted(err)
			orchModel := c.resolveAgentModel(orchDef, "")
			c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator continuation failed").withTodoID(CoordTodoID))
			return "", fmt.Errorf("coordinator continuation failed (model: %s): %w", orchModel, err)
		}
	}

	result, steps = c.ensureFinished(ctx, &orchDefCopy, result, steps)

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	c.addSessionAssistantMessage(finalResult)
	if c.sessionData != nil {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("continuation finished").withTodoID(CoordTodoID))
	return finalResult, nil
}
