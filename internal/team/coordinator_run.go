package team

// Top-level run loops: the orchestrator loop, direct @agent runs, and
// think-mode event emission.

import (
	"context"
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
	agentDef, _, err := c.resolveAgentName(agentName)
	if err != nil {
		return nil, err
	}

	c.setAutoLoadedSkills(c.matchSkillsWithSidecar(ctx, task))

	resolvedName := strings.ToLower(agentDef.Name)
	directModel := c.resolveAgentModel(agentDef, "")

	todoItems := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: resolvedName, Desc: task, Model: directModel, Source: TaskSourceCoordinator, ParentID: ""}})
	todoID := todoItems[0].ID
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
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		return nil, fmt.Errorf("failed to create agent %q: %w", resolvedName, err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	taskCtx, cancel := context.WithTimeout(ctx, agentTimeout)
	defer cancel()
	taskCtx = tools.AskUserAwareDeadline(taskCtx)

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
	}

	timing := &taskTiming{}
	timing.reset()

	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "working", task, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "working", task)

	prompt := c.appendSkillContext(task, agentDef, resolvedName, task, todoID)

	if suffix := c.buildMemorySuffix(agentDef.Role); suffix != "" {
		prompt = prompt + "\n\n" + suffix
	}

	output, err := c.runAgentWithStatus(taskCtx, ag, resolvedName, prompt, timing)
	duration, modelTime, toolTime := timing.snapshot()
	if err != nil {
		if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "error", task, ""); err != nil {
			log.Printf("warning: failed to write task file: %v", err)
		}
		_ = writeStatus(c.session.Workspace, resolvedName, "error", task)
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(resolvedName).withMessage(err.Error()).withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
		return &DirectAgentResult{AgentName: resolvedName, Error: err}, nil
	}

	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "done", task, output); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, resolvedName, "done", task)
	c.taskTracker.TodoList().UpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output)
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(resolvedName).withOutput(output).withMessage("completed").withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))

	return &DirectAgentResult{AgentName: resolvedName, Output: output}, nil
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
			name := t.Info().Name
			if name == "ask_user" || name == "stm_write" || name == "ltm_update" {
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
		name := t.Info().Name
		if name == "ask_user" || name == "stm_write" || name == "ltm_update" {
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

	orchCtx, cancel := context.WithTimeout(ctx, coordinatorTimeout)
	defer cancel()
	orchCtx = tools.AskUserAwareDeadline(orchCtx)
	orchCtx = context.WithValue(orchCtx, todoIDKey{}, CoordTodoID)
	if c.unattended {
		orchCtx = context.WithValue(orchCtx, tools.UnattendedKey, true)
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

func (c *Coordinator) finalizeRemainingTasks() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskInProgress, TaskPaused:
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
// this point since ExecuteTasks waits for all goroutines, but we handle them as
// done out of caution.
func (c *Coordinator) finalizeNormalCompletion() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskPending:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "")
			changed = true
		case TaskInProgress, TaskPaused:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "still in progress at completion")
			changed = true
		}
	}
	if changed {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
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

func (c *Coordinator) Run(ctx context.Context, userPrompt string) (string, error) {
	c.lastStmWrite = time.Time{}

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

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", userPrompt)
	}

	systemPrompt := c.expandOrchestratorTemplate(orchDef.System)
	if systemPrompt == "" {
		systemPrompt = c.expandOrchestratorTemplate(defaultOrchestratorSystem)
	}
	matchedSkills := c.matchSkillsWithSidecar(ctx, userPrompt)
	c.setAutoLoadedSkills(matchedSkills)
	c.computeWorkerSummaries(ctx)

	if c.think {
		c.emitThinkSkills(matchedSkills)
	}

	systemPrompt += "\n\n" + c.BuildOrchestratorPrompt(matchedSkills...)

	if c.think {
		c.emitThinkAgents()
	}

	if agentsMD := c.loadProjectContext(); agentsMD != "" {
		if s := c.Sidecar(); s != nil && len(agentsMD) > 4000 {
			if c.think {
				c.emitThinkSidecar("Compact", "compacting AGENTS.md for coordinator prompt")
			}
			compacted, err := s.Compact(ctx, agentsMD, "Compress this project context while preserving all key facts, patterns, conventions, and instructions.")
			if err == nil && compacted != "" {
				agentsMD = compacted
			}
		}
		systemPrompt += "\n\n---\n## Project Context (AGENTS.md)\n\n" + agentsMD
	}

	if c.memoryStore != nil {
		var compactFn memory.CompactFunc
		if s := c.Sidecar(); s != nil {
			compactFn = s.Compact
		}
		memCtx, err := memory.AutoQuery(ctx, c.memoryStore, userPrompt, compactFn)
		if err == nil && memCtx != "" {
			systemPrompt += "\n\n---\n" + memCtx
		}
	}

	if c.sessionData != nil && len(c.sessionData.Entries) > 1 && len(c.conversationHistory) == 0 {
		contextSummary := c.sessionData.ContextSummary()
		if contextSummary != "" {
			systemPrompt += "\n\n---\n## Session Context\n\n" + contextSummary
		}
	}

	if suffix := c.buildMemorySuffix("coordinator"); suffix != "" {
		systemPrompt += "\n\n" + suffix
	}

	if reminder := c.buildCoreReminder(orchDef); reminder != "" {
		systemPrompt += "\n\n" + reminder
	}

	if c.think {
		c.emitThinkPrompt(systemPrompt)
	}

	// Apply the computed system prompt to a copy so shared state is not mutated.
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("coordinator starting").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestrator(ctx, &orchDefCopy, userPrompt)
	if err != nil {
		c.finalizeRemainingTasks()
		c.saveHistoryAndSession(ctx, steps)
		orchModel := c.resolveAgentModel(orchDef, "")
		c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator failed").withTodoID(CoordTodoID))
		return "", fmt.Errorf("coordinator failed (model: %s): %w", orchModel, err)
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		_ = SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator finished").withTodoID(CoordTodoID))
	c.SetCurrentStage("idle")
	return finalResult, nil
}

func (c *Coordinator) ContinueWithPrompt(ctx context.Context, additionalPrompt string) (string, error) {
	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	memorySuffix := c.buildMemorySuffix("coordinator")

	var continuationPrompt string
	if c.IsWrapUp() {
		continuationPrompt = wrapUpPromptTemplate + "\n\n" + memorySuffix
		additionalPrompt = "wrap up now"
		c.report(c.newEvent("wrap_up_phase").withMessage("coordinator summarizing").withTodoID(CoordTodoID))
	} else {
		continuationPrompt = fmt.Sprintf(continuationPromptTemplate, additionalPrompt) + "\n\n" + memorySuffix
		c.report(c.newEvent("step").withMessage("coordinator preparing").withTodoID(CoordTodoID))
	}

	if c.sessionData != nil {
		c.sessionData.AddEntry("user", additionalPrompt)
	}

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("continuing with additional input").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestrator(ctx, orchDef, continuationPrompt)
	if err != nil {
		c.finalizeRemainingTasks()
		c.saveHistoryAndSession(ctx, steps)
		orchModel := c.resolveAgentModel(orchDef, "")
		c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator continuation failed").withTodoID(CoordTodoID))
		return "", fmt.Errorf("coordinator continuation failed (model: %s): %w", orchModel, err)
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	if c.sessionData != nil {
		c.sessionData.AddEntry("assistant", finalResult)
		c.sessionData.Rounds = c.round
		_ = SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("continuation finished").withTodoID(CoordTodoID))
	return finalResult, nil
}
