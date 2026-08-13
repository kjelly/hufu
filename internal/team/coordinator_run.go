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

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/hooks"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
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
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		return nil, fmt.Errorf("direct agent invocation is disabled for runtime workflows; dispatch the active phase through the coordinator")
	}
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
	// Direct-agent invocation creates a real task and must participate in the
	// same run-scoped task budget as coordinator-created work.
	c.recordNoProgressTasks(len(todoItems))
	todoID := todoItems[0].ID
	reconcileDirectStatus := func() {
		if err := c.reconcileProjectedItems(c.taskTracker.TodoList().Items()); err != nil {
			log.Printf("warning: direct-agent status projection failed: %v", err)
		}
	}
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

	ag, exposedToolNames, err := c.getOrCreateAgent(ctx, agentDef, "")
	if err != nil {
		c.recordExecutionEvent(todoID, resolvedName, 1, "error", directModel, time.Since(attemptStarted), ExecutionUsage{})
		c.PersistFailureWithClass(resolvedName, task, todoID, c.FailureDetail(err, FailureSourceDirectAgentFailed), RetryNone, FailureExecution)
		reconcileDirectStatus()
		return nil, fmt.Errorf("failed to create agent %q: %w", resolvedName, err)
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}

	taskCtx, cancel := tools.WithInteractiveAwareTimeout(ctx, agentTimeout)
	defer cancel()
	taskCtx, roundCancel := context.WithCancel(taskCtx)
	c.registerTerminalRound(todoID, roundCancel)

	taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
	taskCtx = context.WithValue(taskCtx, modelKey{}, directModel)
	taskCtx = context.WithValue(taskCtx, llmUsageReceiptExpectedKey{}, true)
	taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, resolvedName)
	taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, resolvedName)
	taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
	taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, task)
	if len(agentDef.Guard) > 0 {
		taskCtx = context.WithValue(taskCtx, tools.GuardRulesKey, agentDef.Guard)
	}
	if allowedPaths := c.runtimeAllowedPaths(agentDef.AllowedPaths); len(allowedPaths) > 0 {
		taskCtx = context.WithValue(taskCtx, tools.AgentAllowedPathsKey, allowedPaths)
	}
	if writePaths := c.runtimeAllowedWritePaths(); len(writePaths) > 0 {
		taskCtx = context.WithValue(taskCtx, tools.AgentAllowedWritePathsKey, writePaths)
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
	taskCtx = c.withEffectiveToolsAllowed(taskCtx, agentDef, exposedToolNames)

	timing := &taskTiming{}
	timing.reset()

	taskTS := time.Now().Format("20060102-150405")
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "working", task, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	reconcileDirectStatus()

	prompt := c.appendSkillContext(task, agentDef, resolvedName, task, todoID)

	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		execCtx := c.phaseWorkflow.executionContext()
		prompt += "\n\n## Execution Context\n\n"
		prompt += fmt.Sprintf("- **Phase**: `%s`\n", c.phaseWorkflow.State())
		prompt += fmt.Sprintf("- **Runtime Workspace**: `%s`\n", execCtx.RuntimeWorkspace.Root)
		prompt += fmt.Sprintf("- **Artifacts Directory**: `%s/artifacts`\n", execCtx.RuntimeWorkspace.Root)
		prompt += fmt.Sprintf("- **Receipts Directory**: `%s/receipts`\n", execCtx.RuntimeWorkspace.Root)
		prompt += "Ensure all durable outputs are written to the artifacts directory, not the project source.\n"
		if len(execCtx.Capabilities.Required) > 0 {
			prompt += fmt.Sprintf("- **Required Capabilities**: `%s`\n", strings.Join(execCtx.Capabilities.Required, ", "))
		}
	}

	// Phase 2 keeps the legacy prompt path authoritative. Compile a shadow
	// bundle for comparison only; a compiler failure must never affect a task.
	workerInput := WorkerContextInput{TaskGoal: prompt, TaskDef: TaskDef{Agent: resolvedName, Goal: task}, AgentDef: agentDef,
		RawSTM: LoadSTM(c.session.Workspace), RawLTM: LoadLTM(c.session.Workspace, c.session.Config.Name), MemoryStore: c.memoryStore,
		ModelContext: globalRegistry.GetSpec(c.resolveAgentModel(agentDef, "")).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(agentDef)), MaxAuxChars: maxWorkerAuxContextChars,
		DisableMemory: c.ExecutionProfile().DisableHistoricalMemory}
	// WP-3: recall per-worker private memory before direct-agent dispatch.
	if memBundle := c.recallWorkerMemory(taskCtx, agentDef, task); memBundle != nil {
		workerInput.WorkerMemory = memBundle
	}
	if suffix := c.buildMemorySuffix(agentDef.Role); suffix != "" {
		prompt += "\n\n" + suffix
	}
	c.compileShadowWorker(taskCtx, workerInput, prompt)

	output, steps, err := c.runAgentWithStatusAndHistory(taskCtx, ag, resolvedName, prompt, nil, timing)
	roundCancel()
	c.unregisterTerminalRound(todoID)
	duration, modelTime, toolTime := timing.snapshot()
	err, terminalBlocked := c.finalizeTaskTerminalResources(ctx, todoID, err)
	if err != nil {
		c.recordExecutionEvent(todoID, resolvedName, 1, "error", directModel, time.Since(attemptStarted), usageFromSteps(steps))
		if terminalBlocked {
			c.PersistFailureWithClassAndStatus(resolvedName, task, todoID, c.FailureDetail(err, FailureSourceDirectAgentFailed), ReconcileOnly, FailureExecution, TaskBlocked)
		} else {
			c.PersistFailureWithClass(resolvedName, task, todoID, c.FailureDetail(err, FailureSourceDirectAgentFailed), RetryNone, FailureExecution)
		}
		c.updateTodoTiming(todoID, modelTime, toolTime)
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		c.report(c.newEvent("error").withAgent(resolvedName).withMessage(err.Error()).withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
		reconcileDirectStatus()
		// Direct-agent execution is a complete run boundary, so enforce the
		// no-progress budget even when the worker itself failed. A worker error
		// remains the primary failure, while a terminal budget stop is already
		// persisted by enforceNoProgressBudget for the next coordinator path.
		c.enforceNoProgressBudget()
		return &DirectAgentResult{AgentName: resolvedName, Error: err, Steps: len(steps)}, nil
	}

	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, resolvedName, taskTS, "done", task, output); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	c.taskTracker.TodoList().UpdateStatusAndOutput(todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output)
	c.updateTodoTiming(todoID, modelTime, toolTime)
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	reconcileDirectStatus()
	c.report(c.newEvent("done").withAgent(resolvedName).withOutput(output).withMessage("completed").withModel(directModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
	c.recordExecutionEvent(todoID, resolvedName, 1, "done", directModel, time.Since(attemptStarted), usageFromSteps(steps))
	// WP-4: ingest worker session memory on direct-agent success. The
	// direct path has no explicit deliverable verification, so verified is
	// false unless a terminal verifier reported success — making this a
	// candidate until run acceptance promotes it. Idempotency: the canonical
	// store deduplicates by execution identity, so if the fast path
	// escalates to the team path and the same task re-runs, the re-ingest
	// is a no-op.
	verified := false
	if vr := verifyResultForTodo(c, todoID); vr != nil && isVerifySuccess(vr) {
		verified = true
	}
	typedRes := c.GetTaskResult(todoID)
	c.ingestWorkerSessionMemory(taskCtx, agentDef, todoID, typedRes, output, verified, 1)
	if stopped, reason := c.enforceNoProgressBudget(); stopped {
		// The worker artifact is complete, but the run is not: returning an
		// error keeps the fast path from reporting successful completion while
		// stopForNoProgress has persisted the canonical partial result and
		// continuation checkpoint.
		return &DirectAgentResult{
			AgentName: resolvedName,
			Output:    output,
			Error:     fmt.Errorf("direct agent stopped: %s", reason),
			Steps:     len(steps),
		}, nil
	}
	if c.noProgressReplanPending() {
		// Direct execution has no coordinator continuation loop of its own.
		// Surface the first-threshold disposition explicitly so callers cannot
		// report a successful direct run without giving the coordinator a chance
		// to replan. The fast path recognizes ReplanRequired and escalates.
		return &DirectAgentResult{
			AgentName:      resolvedName,
			Output:         output,
			Error:          fmt.Errorf("direct agent requires replan: no-progress budget reached"),
			ReplanRequired: true,
			Steps:          len(steps),
		}, nil
	}

	return &DirectAgentResult{AgentName: resolvedName, Output: output, Steps: len(steps)}, nil
}

// coordinatorCoreToolNames are the tools the coordinator is permitted to use
// at runtime, regardless of team.yaml. This must include every tool exposed by
// buildOrchestratorTools: the stream authorization gate fails closed.
// The read-only file tools (view/grep/glob/ls) let it consult files directly
// instead of burning a full delegation round asking a worker to cat a document.
var coordinatorCoreToolNames = map[string]bool{
	"agent":          true,
	"ask_user":       true,
	"finish":         true,
	"load_skill":     true,
	"save_skill":     true,
	"stm_write":      true,
	"ltm_update":     true,
	"view":           true,
	"grep":           true,
	"glob":           true,
	"ls":             true,
	"team_info":      true,
	"reconcile_task": true,
	"approve_plan":   true,
	"modify_plan":    true,
	"reject_plan":    true,
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
	var orchTools []fantasy.AgentTool
	if c.forcePlanFirst {
		orchTools = []fantasy.AgentTool{
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
		return c.restrictInitialCoordinatorTools(c.filterDeniedCoordinatorTools(orchTools))
	}
	orchTools = []fantasy.AgentTool{
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
	return c.restrictInitialCoordinatorTools(c.filterDeniedCoordinatorTools(orchTools))
}

// restrictInitialCoordinatorTools makes a configured first-tool policy
// model-visible as well as runtime-enforced. A fresh session can retain
// non-authoritative history in an upstream provider or other memory layer; it
// must not be able to turn that prose into a terminal out-of-order tool call
// before the canonical initial delegation is created. Once a TODO exists, the
// ordinary coordinator tool set is restored.
//
// This is deliberately generic: it honors whichever coordinator tool a team
// configured as its first tool and does not inspect task goals, providers, or
// project-specific state. Returning no tools for an unavailable configured
// first tool is fail-closed and leaves the existing policy validation error as
// the diagnostic boundary.
func (c *Coordinator) restrictInitialCoordinatorTools(candidate []fantasy.AgentTool) []fantasy.AgentTool {
	want := c.initialCoordinatorToolName()
	if want == "" {
		return candidate
	}
	for _, tool := range candidate {
		if tool != nil && tool.Info().Name == want {
			return []fantasy.AgentTool{tool}
		}
	}
	return nil
}

func (c *Coordinator) runOrchestrator(ctx context.Context, orchDef *agent.AgentDef, prompt string) (string, []fantasy.StepResult, error) {
	// A coordinator turn is one orchestrator model invocation. Count it here
	// rather than in ExecuteTasks: one turn may delegate multiple batches, or
	// may spend its whole budget reasoning without delegating any task.
	// Continuations and test overrides use this same boundary.
	c.metricsMu.Lock()
	c.turnsSinceCriterionProgress++
	c.metricsMu.Unlock()
	if c.runOrchestratorOverride != nil {
		return c.runOrchestratorOverride(ctx, orchDef, prompt)
	}
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
	orchCtx = context.WithValue(orchCtx, modelKey{}, orchModelID)
	if c.providerManager == nil {
		return "", nil, fmt.Errorf("provider manager unavailable")
	}
	orch, err := c.createGatedAgent(orchCtx, c.providerManager.GetProvider(orchModelID), agent.AgentConfig{
		Def:        orchDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   c.stepBudget(orchDef, agent.DefaultCoordinatorMaxSteps),
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

// runOrchestratorWithNoProgressGuard checks a restored continuation budget
// before dispatching the next model turn. This is essential after a restart:
// persisted counters must not be allowed to overshoot once more before the
// normal ensureFinished boundary gets a chance to enforce them.
func (c *Coordinator) runOrchestratorWithNoProgressGuard(ctx context.Context, orchDef *agent.AgentDef, prompt string) (string, []fantasy.StepResult, error) {
	if stopped, reason := c.enforceNoProgressBudget(); stopped {
		if summary := c.summaryFromTodos(fmt.Errorf("%s", reason)); summary != "" {
			return summary, nil, nil
		}
		if last := c.LastRunResult(); last != nil && last.Response != "" {
			return last.Response, nil, nil
		}
		return fmt.Sprintf("Run stopped before another coordinator turn: %s", reason), nil, nil
	}
	return c.runOrchestrator(ctx, orchDef, prompt)
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
	if c.terminalUnresolvedRun() {
		// A terminal worker disposition is evidence, not a request for an
		// additional coordinator model turn. In particular, do not give a
		// coordinator whose available tools were intentionally restricted an
		// opportunity to call a denied finish, team_info, or agent tool while
		// trying to summarize the already-terminal run.
		return c.finalizeTerminalUnresolvedRun(), steps
	}
	if c.noProgressStopPending() {
		// A task-boundary hard stop may arrive here through the coordinator's
		// error path. Preserve its partial result and never spend a recovery
		// LLM turn after the no-progress budget has been exhausted.
		if summary := c.summaryFromTodos(fmt.Errorf("no-progress budget exhausted")); summary != "" {
			result = summary
		}
		return result, steps
	}

	maxContinuationTurns := 5
	if c.session != nil && c.session.Config.MaxCoordinatorTurns > 0 {
		maxContinuationTurns = c.session.Config.MaxCoordinatorTurns
	}
	continuationTurns := 0
	continuationReason := ""
	budgetStopped := false
	noProgressStopped := false
	continuationInterrupted := false
	for turn := 1; turn <= maxContinuationTurns; turn++ {
		if c.finishCalled.Load() || ctx.Err() != nil {
			break
		}
		if exceeded, reason := c.budgetExceeded(); exceeded {
			c.report(c.newEvent("budget_exceeded").withMessage(reason))
			continuationReason = reason
			budgetStopped = true
			break
		}
		// No-progress budget enforcement at the turn boundary (§8.1, WP-12).
		// Each continuation turn is a coordinator turn; check the counters
		// before running another orchestrator turn.
		// The first no-progress boundary already requested a replan. Allow
		// exactly one continuation turn to respond to it; the post-turn check
		// below escalates if no criterion advanced. Other boundaries are checked
		// before dispatch as usual.
		if !c.noProgressReplanPending() {
			if stopped, stopReason := c.enforceNoProgressBudget(); stopped {
				continuationReason = stopReason
				budgetStopped = true
				noProgressStopped = true
				break
			}
		}
		continuationTurns = turn
		c.saveContinuationCheckpoint(turn, maxContinuationTurns, "step_limit", "pending")
		c.report(c.newEvent("coordinator_continuation").withMessage(fmt.Sprintf("step limit reached (continuation turn %d/%d); continuing automatically", turn, maxContinuationTurns)).withTodoID(CoordTodoID))
		c.saveHistoryAndSession(ctx, steps)

		contPrompt := "The step limit for the previous turn was reached. Please continue coordinating and executing the tasks required to satisfy the user's request. When complete, call finish."
		wrapResult, wrapSteps, err := c.runOrchestrator(ctx, orchDef, contPrompt)
		if err != nil || strings.TrimSpace(wrapResult) == "" {
			continuationReason = "continuation turn returned no result"
			if err != nil {
				continuationReason = fmt.Sprintf("continuation turn failed: %v", err)
			}
			continuationInterrupted = true
			break
		}
		result = wrapResult
		steps = wrapSteps
		if !c.finishCalled.Load() {
			if stopped, stopReason := c.enforceNoProgressBudget(); stopped {
				continuationReason = stopReason
				budgetStopped = true
				noProgressStopped = true
				break
			}
		}
	}

	if continuationInterrupted && c.canResumeInterruptedWorkflow() {
		// A coordinator continuation can end because its provider stalls or
		// returns no result. The current workflow phase and completed task
		// receipts are durable state, so this is a resumable interruption—not
		// permission to force finish or run acceptance against an incomplete
		// workflow.
		result = c.recordInterruptedContinuation(continuationTurns, maxContinuationTurns, continuationReason, result)
		return result, steps
	}

	if noProgressStopped {
		// No-progress enforcement is a hard stop: do not spend another LLM
		// turn after the budget has been exhausted. Return the deterministic
		// completed-task summary when one is available.
		if summary := c.summaryFromTodos(fmt.Errorf("%s", continuationReason)); summary != "" {
			result = summary
		}
	} else if !c.finishCalled.Load() && ctx.Err() == nil {
		c.report(c.newEvent("wrap_up_phase").withMessage("coordinator stopped without calling finish after continuation turns; forcing final summary").withTodoID(CoordTodoID))
		c.saveHistoryAndSession(ctx, steps)
		wrapResult, wrapSteps, err := c.runOrchestrator(ctx, orchDef, stepLimitWrapUpPrompt)
		if err == nil && strings.TrimSpace(wrapResult) != "" {
			result = wrapResult
			steps = wrapSteps
		}
		if !c.finishCalled.Load() {
			if stopped, stopReason := c.enforceNoProgressBudget(); stopped {
				continuationReason = stopReason
				budgetStopped = true
			}
		}
	}

	if c.LastRunResult() == nil {
		accRes, accErr := c.runAcceptance(ctx)
		if manifestErr := c.finalizeEvidenceManifest(ctx, accRes); manifestErr != nil {
			c.report(c.newEvent("error").withMessage("evidence manifest finalization failed: " + manifestErr.Error()))
		}
		items := c.taskTracker.TodoList().Items()
		failedTasks := failedTodoItems(items)
		unresolvedPending := pendingTodoItems(items)
		allUnresolved := append(failedTasks, unresolvedPending...)
		acceptanceState := AcceptanceNotConfigured
		if accRes != nil {
			acceptanceState = accRes.State
		}
		if accErr != nil {
			acceptanceState = AcceptanceFailed
		}
		evaluated := EvaluateRunOutcome(RunEvaluationInput{
			UnresolvedTasks: toTaskReferences(allUnresolved),
			Acceptance:      acceptanceState,
			BudgetExceeded:  budgetStopped,
			Cancelled:       ctx.Err() == context.Canceled,
			Response:        strings.TrimPrefix(result, "FINISHED:"),
			Reason:          continuationReason,
			Stats:           SummarizeRunStats(items),
			Metrics:         c.Metrics(),
			GoalMode:        c.GoalMode(),
		})
		evaluated.Acceptance = accRes
		c.lastEvidenceManifestMu.RLock()
		evaluated.EvidenceManifest = c.lastEvidenceManifest
		c.lastEvidenceManifestMu.RUnlock()
		evaluatedPtr := c.applyCompletionGate(ctx, &evaluated, accRes)
		if evaluatedPtr != nil {
			evaluated = *evaluatedPtr
		}
		progress := c.noProgressCounters()
		evaluated.Continuation = &ContinuationInfo{TurnCount: continuationTurns, MaxTurns: maxContinuationTurns, Reason: continuationReason, NoProgress: &progress, NoProgressReplanPending: c.noProgressReplanPending()}
		c.SetLastRunResult(&evaluated)
	}
	if continuationTurns > 0 {
		status := "completed"
		if continuationInterrupted || ctx.Err() != nil || !c.finishCalled.Load() {
			status = "aborted"
		}
		c.saveContinuationCheckpoint(continuationTurns, maxContinuationTurns, continuationReason, status)
	}

	return result, steps
}

// canResumeInterruptedWorkflow reports whether a provider interruption can
// safely leave the runtime workflow open for a later coordinator invocation.
// Non-workflow teams retain their established forced-summary behavior.
func (c *Coordinator) canResumeInterruptedWorkflow() bool {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return false
	}
	switch c.phaseWorkflow.State() {
	case PhaseDone, PhaseFailed:
		return false
	default:
		return true
	}
}

// recordInterruptedContinuation preserves a failed coordinator continuation as
// a resumable partial run. It intentionally does not run acceptance or alter
// workflow/task state: the next invocation resumes from the durable phase
// checkpoint instead of treating this provider interruption as a finish.
func (c *Coordinator) recordInterruptedContinuation(turn, maxTurns int, reason, result string) string {
	if summary := c.summaryFromTodos(errors.New(reason)); summary != "" {
		result = summary
	}
	items := []*TodoItem(nil)
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		items = c.taskTracker.TodoList().Items()
	}
	progress := c.noProgressCounters()
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks:        toTaskReferences(pendingTodoItems(items)),
		CoordinatorInterrupted: true,
		Response:               result,
		Reason:                 reason,
		Stats:                  SummarizeRunStats(items),
		Metrics:                c.Metrics(),
		GoalMode:               c.GoalMode(),
	})
	// EvaluateRunOutcome supplies a default acceptance placeholder. No
	// acceptance command ran on this resumable path, so omit it rather than
	// claiming the workflow has no acceptance contract.
	evaluated.Acceptance = nil
	evaluated.Continuation = &ContinuationInfo{
		TurnCount:  turn,
		MaxTurns:   maxTurns,
		Reason:     reason,
		NoProgress: &progress,
	}
	c.SetLastRunResult(&evaluated)
	c.saveContinuationCheckpoint(turn, maxTurns, reason, "aborted")
	c.continuationInterrupted.Store(true)
	return result
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
	if c.terminalUnresolvedRun() {
		// Do not convert a terminal worker result (or the coordinator-tool
		// boundary it produces) into a summary-model turn. The deterministic
		// result below preserves the failed task IDs and produces a nonzero run
		// outcome without exposing any more coordinator tools.
		return c.finalizeTerminalUnresolvedRun(), nil, true
	}
	// Direct coordinator tool failures are a terminal boundary. In particular,
	// do not convert one into another model turn merely because the error
	// occurred during wrap-up; that would let the coordinator act after an
	// unavailable or failed tool call.
	if errors.Is(runErr, errCoordinatorToolFailure) {
		return "", nil, false
	}
	if c.noProgressStopPending() {
		// ExecuteTasks can surface a no-progress hard stop as an error while
		// the coordinator is already in wrap-up. Return the existing partial
		// evidence without invoking another model turn.
		if summary := c.summaryFromTodos(runErr); summary != "" {
			return summary, nil, true
		}
		if last := c.LastRunResult(); last != nil {
			return last.Response, nil, true
		}
		return "no-progress budget exhausted; no further LLM turn is permitted", nil, true
	}
	c.report(c.newEvent("wrap_up_phase").withMessage(fmt.Sprintf("coordinator stopped before finishing (%v); forcing a final summary turn", runErr)).withTodoID(CoordTodoID))
	result, steps, err := c.runOrchestrator(ctx, orchDef, wrapUpPromptTemplate)
	if err == nil && strings.TrimSpace(result) != "" {
		if !c.finishCalled.Load() {
			if stopped, stopReason := c.enforceNoProgressBudget(); stopped {
				if summary := c.summaryFromTodos(fmt.Errorf("%s", stopReason)); summary != "" {
					return summary, nil, true
				}
				return result, steps, true
			}
		}
		return strings.TrimPrefix(result, "FINISHED:"), steps, true
	}
	summary := c.summaryFromTodos(runErr)
	if summary == "" {
		return "", nil, false
	}
	return summary, nil, true
}

// terminalUnresolvedRun reports the narrow condition in which the coordinator
// must stop using its model entirely: a run has entered wrap-up and still has
// a failed, blocked, or protocol-incomplete task. It deliberately does not
// apply to ordinary retryable failures, acceptance recovery, or successful
// runs. Those paths retain their existing coordinator behavior.
func (c *Coordinator) terminalUnresolvedRun() bool {
	if c == nil || !c.IsWrapUp() || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return false
	}
	return len(failedTodoItems(c.taskTracker.TodoList().Items())) > 0
}

// finalizeTerminalUnresolvedRun creates an LLM-free, canonical failure result
// for a terminal unresolved worker state. It is safe to call from both the
// agent-tool path and finalization paths; each call derives the same result
// from durable todo state and never runs acceptance or another coordinator
// turn.
func (c *Coordinator) finalizeTerminalUnresolvedRun() string {
	const reason = "terminal unresolved worker task; coordinator continuation disabled"
	if c == nil {
		return reason
	}
	items := []*TodoItem(nil)
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		items = c.taskTracker.TodoList().Items()
	}
	summary := c.summaryFromTodos(errors.New(reason))
	if summary == "" {
		summary = reason
	}
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: UnresolvedTaskReferences(items),
		Acceptance:      AcceptanceNotConfigured,
		Response:        summary,
		Reason:          reason,
		Stats:           SummarizeRunStats(items),
		Metrics:         c.Metrics(),
		GoalMode:        c.GoalMode(),
	})
	c.SetLastRunResult(&evaluated)
	c.SetCurrentStage("terminal_unresolved")
	return summary
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
	failed := 0
	for _, item := range items {
		switch item.Status {
		case TaskDone:
			done++
			fmt.Fprintf(&b, "\n### %s: %s\n%s\n", item.Agent, item.Desc, utils.TruncateRunes(item.Output, summaryMaxRunes))
		case TaskError, TaskBlocked, TaskProtocolIncomplete:
			failed++
			fmt.Fprintf(&b, "\n### %s: %s\nFAILED: %s\n", item.Agent, item.Desc, FailureDisplayText(item))
		}
	}
	if done == 0 && failed == 0 {
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
	exitCode := 1
	switch {
	case errors.Is(runErr, context.Canceled):
		reason = "run aborted (cancelled by user)"
		exitCode = 130
	case errors.Is(runErr, context.DeadlineExceeded):
		reason = "run aborted (coordinator timeout)"
		exitCode = 124
	}
	items := []*TodoItem(nil)
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		items = c.taskTracker.TodoList().Items()
	}
	unresolved := append(failedTodoItems(items), pendingTodoItems(items)...)
	response := fmt.Sprintf("%s: %v", reason, runErr)
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: toTaskReferences(unresolved),
		Cancelled:       errors.Is(runErr, context.Canceled),
		RunFailed:       !errors.Is(runErr, context.Canceled),
		ExitCode:        exitCode,
		Response:        response,
		Reason:          reason,
		Stats:           SummarizeRunStats(items),
		Metrics:         c.Metrics(),
		GoalMode:        c.GoalMode(),
	})
	c.SetLastRunResult(&evaluated)
	checkpoint := c.ContinuationCheckpoint()
	if checkpoint == nil {
		c.saveContinuationCheckpoint(0, 0, reason, "aborted")
	} else {
		c.saveContinuationCheckpoint(checkpoint.TurnCount, checkpoint.MaxTurns, reason, "aborted")
	}

	if err := c.reconcileProjectedStatusesWithDetail(AgentStatusError, fmt.Sprintf("%s; error=%v", reason, runErr)); err != nil {
		log.Printf("warning: aborted-run status projection failed: %v", err)
	}

	if c.sessionData == nil {
		return
	}
	entry := fmt.Sprintf("[%s: %v]", reason, runErr)
	if summary := c.summaryFromTodos(runErr); summary != "" {
		entry += "\n\n" + summary
	}
	c.addSessionAssistantMessage(entry)
	if c.sessionData != nil && c.session != nil && c.session.Workspace != "" {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}
}

func (c *Coordinator) finalizeRemainingTasks() {
	items := c.taskTracker.TodoList().Items()
	changed := false
	for _, item := range items {
		switch item.Status {
		case TaskInProgress, TaskPaused, TaskVerifying, TaskProtocolIncomplete:
			c.PersistFailureWithClassAndStatus(item.Agent, item.Desc, item.ID, "coordinator ended unexpectedly", RetryNone, FailureExecution, TaskError)
			changed = true
		case TaskPending:
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "")
			changed = true
		}
	}
	if changed {
		c.reconcileTaskStatusProjection()
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
		case TaskInProgress, TaskPaused, TaskVerifying, TaskProtocolIncomplete:
			c.PersistFailureWithClassAndStatus(item.Agent, item.Desc, item.ID, "coordinator finished before task completed", RetryNone, FailureExecution, TaskError)
			changed = true
		}
	}
	if changed {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}

	// Rebuild projections from canonical task/session state. This also repairs
	// stale files left by a retry, cancellation, or restored interrupted run.
	c.reconcileProjectedStatuses(AgentStatusIdle)
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
	if err := os.WriteFile(dumpPath, []byte(utils.RedactSecrets(systemPrompt)), 0o644); err == nil {
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

func (c *Coordinator) buildSystemPrompt(ctx context.Context, orchDef *agent.AgentDef, prompt string, isContinuation bool) (string, error) {
	prompt = utils.RedactSecrets(prompt)
	var systemPrompt string
	if orchDef != nil {
		systemPrompt = utils.RedactSecrets(c.expandOrchestratorTemplate(orchDef.System))
	}
	if systemPrompt == "" {
		systemPrompt = c.expandOrchestratorTemplate(defaultOrchestratorSystem)
	}
	systemPrompt = c.filterDeniedPromptLines(systemPrompt)

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

	// A fresh exact-initial phase has no canonical task result yet. --new may
	// retain STM/LTM/history archives for later learning, but those records
	// cannot decide whether this run reached a later phase. Keep every
	// historical source out of the coordinator's first-turn context until the
	// initial batch is accepted; the phase prompt and narrowed agent schema are
	// then the sole normative dispatch inputs.
	allowHistoricalMemory := !c.ExecutionProfile().DisableHistoricalMemory && !c.initialDelegationPending()
	var contextSummary string
	if !isContinuation && allowHistoricalMemory && c.sessionData != nil && len(c.sessionData.Entries) > 1 && len(c.conversationHistory) == 0 {
		contextSummary = c.sessionData.ContextSummary()
	}

	var modelSpec ModelContextSpec
	if orchDef != nil {
		modelSpec = globalRegistry.GetSpec(c.resolveAgentModel(orchDef, "")).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(orchDef))
	}

	coordInput := CoordinatorContextInput{
		Goal:           prompt,
		SessionContext: contextSummary,
		RawSTM: func() string {
			if allowHistoricalMemory {
				return utils.RedactSecrets(LoadSTM(c.session.Workspace))
			}
			return ""
		}(),
		RawLTM: func() string {
			if allowHistoricalMemory {
				return utils.RedactSecrets(LoadLTM(c.session.Workspace, c.session.Config.Name))
			}
			return ""
		}(),
		MemoryStore: func() *memory.MemoryStore {
			if allowHistoricalMemory {
				return c.memoryStore
			}
			return nil
		}(),
		SidecarCompacter: c.AgentPool().Sidecar(),
		ModelContext:     modelSpec,
		Role:             "coordinator",
		IsContinuation:   isContinuation,
		DisableMemory:    !allowHistoricalMemory,
		ProjectContext:   utils.RedactSecrets(c.loadProjectContext()),
	}
	if c.continuationResume != nil {
		resume := c.continuationResume
		checkpointText := fmt.Sprintf("\n\n---\n## Resumed Continuation Checkpoint\n\nThis run is resuming an interrupted coordinator continuation. Continue from turn %d/%d. Previous reason: %s. Reuse completed task results and reconcile in-progress work; do not duplicate completed tasks.\n", resume.TurnCount, resume.MaxTurns, resume.Reason)
		systemPrompt += checkpointText
		coreText.WriteString(checkpointText)
	}

	// The compiler is intentionally shadow-only until canonical mode. Retain
	// the exact legacy assembly below so model-visible prompts do not change.
	if agentsMD := coordInput.ProjectContext; agentsMD != "" {
		agentsMD = compactLegacyProjectContext(ctx, c.AgentPool().Sidecar(), agentsMD)
		systemPrompt += "\n\n---\n## Project Context (AGENTS.md)\n\n" + agentsMD
		projectText.WriteString(agentsMD)
	}
	if c.memoryStore != nil && prompt != "" && allowHistoricalMemory {
		var compactFn memory.CompactFunc
		if sidecar := c.AgentPool().Sidecar(); sidecar != nil {
			compactFn = sidecar.Compact
		}
		if memCtx, err := memory.AutoQuery(ctx, c.memoryStore, prompt, compactFn); err == nil && memCtx != "" {
			systemPrompt += "\n\n---\n" + memCtx
			memoryText.WriteString(memCtx + "\n")
		}
	}
	if !isContinuation && allowHistoricalMemory && contextSummary != "" {
		systemPrompt += "\n\n---\n## Session Context\n\n" + contextSummary
		memoryText.WriteString(contextSummary + "\n")
	}
	if suffix := func() string {
		if allowHistoricalMemory {
			return c.buildMemorySuffix("coordinator")
		}
		return ""
	}(); suffix != "" {
		systemPrompt += "\n\n" + suffix
		memoryText.WriteString(suffix + "\n")
		coreText.WriteString("\n\n" + suffix)
	}
	if reminder := c.buildCoreReminder(orchDef); reminder != "" {
		systemPrompt += "\n\n" + reminder
		coreText.WriteString("\n\n" + reminder)
	}
	systemPrompt = utils.RedactSecrets(systemPrompt)
	coordInput.CorePrompt = utils.RedactSecrets(coreText.String())
	compiled, compileErr := c.ContextCompiler().CompileCoordinatorContext(ctx, coordInput)
	c.recordShadowTrace(ctx, "coordinator", systemPrompt, coordInput.ModelContext, compiled, compileErr)
	if compileErr != nil {
		return "", fmt.Errorf("coordinator context preflight failed: %w", compileErr)
	}
	if strings.TrimSpace(compiled.Prompt) == "" {
		return "", fmt.Errorf("coordinator context preflight failed: compiled prompt is empty")
	}
	systemPrompt = compiled.Prompt

	if c.think && !isContinuation {
		c.emitThinkPrompt(systemPrompt)
	}

	// Record the model-aware token breakdown for the execution report (§5.4).
	c.recordContextBreakdown(ctx, c.resolveAgentModel(orchDef, ""),
		utils.RedactSecrets(coreText.String()), utils.RedactSecrets(projectText.String()), utils.RedactSecrets(memoryText.String()))

	return utils.RedactSecrets(systemPrompt), nil
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
	defer func() { c.continuationResume = nil }()
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

	// Start the silent-stall watchdog: a run that goes quiet — no step, tool,
	// or output event at all — for the configured threshold gets a goroutine
	// dump and a run_stall_detected event, so a stall like a sidecar/LLM call
	// with no deadline is visible instead of showing up only as elapsed time.
	c.startStallWatchdog(ctx)

	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		return "", fmt.Errorf("no coordinator agent found in team")
	}

	_ = EnsureWorkspaceDirs(c.session.Workspace)
	if c.phaseWorkflow != nil {
		if err := c.phaseWorkflow.Start(); err != nil {
			c.saveCheckpoint()
			return "", err
		}
		c.saveCheckpoint()
	}
	// Status files are projections, so rebuild them from the restored canonical
	// todo/session state before crash-resume can start new work.
	c.reconcileTaskStatusProjection()

	// Replay the persistent task journal (crash-safe complement to the
	// session.json checkpoint) and start appending to it for this run.
	c.initTaskJournal()
	// Restore the no-progress continuation baseline before re-driving any
	// interrupted workers. Their resumed LLM usage must accumulate on top of
	// the persisted counters, not be overwritten by a later restore.
	c.ResumeContinuationCheckpoint()

	c.report(c.newEvent("step").withMessage("coordinator preparing"))

	// Crash-resume: before delegating new work, re-drive any worker tasks that a
	// previous interrupted run left in-flight (restored from the checkpoint).
	// No-op on a fresh run (empty todo list) or with --new (fresh session).
	if n, err := c.ResumeInterruptedTasks(ctx); err != nil {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s), with errors: %v", n, err)))
	} else if n > 0 {
		c.report(c.newEvent("step").withMessage(fmt.Sprintf("resume: re-drove %d interrupted task(s) from checkpoint", n)))
	}
	c.continuationResume = nil

	c.addSessionUserMessage(userPrompt)

	systemPrompt, promptErr := c.buildSystemPrompt(ctx, orchDef, userPrompt, false)
	if promptErr != nil {
		c.recordRunAborted(promptErr)
		return "", promptErr
	}

	// Apply the computed system prompt to a copy so shared state is not mutated.
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("coordinator starting").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestratorWithNoProgressGuard(ctx, &orchDefCopy, userPrompt)
	if err != nil {
		if recovered, recoveredSteps, ok := c.attemptWrapUpRecovery(ctx, &orchDefCopy, err); ok {
			result = recovered
			steps = append(steps, recoveredSteps...)
		} else {
			if cleanupErr := c.cleanupRunTerminalResources(TerminalCleanupRunShutdown); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
			c.finalizeRemainingTasks()
			c.saveHistoryAndSession(ctx, steps)
			c.recordRunAborted(err)
			orchModel := c.resolveAgentModel(orchDef, "")
			c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator failed").withData(runResultStatusData(c.LastRunResult())).withTodoID(CoordTodoID))
			return "", fmt.Errorf("coordinator failed (model: %s): %w", orchModel, err)
		}
	}

	result, steps = c.ensureFinished(ctx, &orchDefCopy, result, steps)
	if c.continuationInterrupted.Load() {
		return c.returnResumableContinuation(ctx, result, steps, "coordinator continuation interrupted; workflow checkpointed for resume")
	}
	if ctx.Err() != nil && !c.finishCalled.Load() {
		if cleanupErr := c.cleanupRunTerminalResources(TerminalCleanupRunCancelled); cleanupErr != nil {
			c.report(c.newEvent("error").withMessage("terminal cleanup error: " + cleanupErr.Error()))
		}
		c.finalizeRemainingTasks()
		c.saveHistoryAndSession(ctx, steps)
		c.recordRunAborted(ctx.Err())
		return "", ctx.Err()
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	c.addSessionAssistantMessage(finalResult)
	if c.sessionData != nil && c.session != nil && c.session.Workspace != "" {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator finished").withData(runResultStatusData(c.LastRunResult())).withTodoID(CoordTodoID))
	c.SetCurrentStage("idle")
	if lastRes := c.LastRunResult(); lastRes != nil && !IsRunOutcomeSuccess(lastRes.Outcome) {
		return finalResult, fmt.Errorf("%w: %s", ErrTasksUnresolved, lastRes.Response)
	}
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

	systemPrompt, promptErr := c.buildSystemPrompt(ctx, orchDef, additionalPrompt, true)
	if promptErr != nil {
		c.recordRunAborted(promptErr)
		return "", promptErr
	}
	orchDefCopy := *orchDef
	orchDefCopy.System = systemPrompt

	c.addSessionUserMessage(additionalPrompt)

	c.report(c.newEvent("start").withAgent(orchDef.Name).withMessage("continuing with additional input").withModel(c.resolveAgentModel(orchDef, "")).withTodoID(CoordTodoID))

	result, steps, err := c.runOrchestratorWithNoProgressGuard(ctx, &orchDefCopy, continuationPrompt)
	if err != nil {
		if recovered, recoveredSteps, ok := c.attemptWrapUpRecovery(ctx, &orchDefCopy, err); ok {
			result = recovered
			steps = append(steps, recoveredSteps...)
		} else {
			if cleanupErr := c.cleanupRunTerminalResources(TerminalCleanupRunShutdown); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
			c.finalizeRemainingTasks()
			c.saveHistoryAndSession(ctx, steps)
			c.recordRunAborted(err)
			orchModel := c.resolveAgentModel(orchDef, "")
			c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("coordinator continuation failed").withData(runResultStatusData(c.LastRunResult())).withTodoID(CoordTodoID))
			return "", fmt.Errorf("coordinator continuation failed (model: %s): %w", orchModel, err)
		}
	}

	result, steps = c.ensureFinished(ctx, &orchDefCopy, result, steps)
	if c.continuationInterrupted.Load() {
		return c.returnResumableContinuation(ctx, result, steps, "coordinator continuation interrupted; workflow checkpointed for resume")
	}
	if ctx.Err() != nil && !c.finishCalled.Load() {
		if cleanupErr := c.cleanupRunTerminalResources(TerminalCleanupRunCancelled); cleanupErr != nil {
			c.report(c.newEvent("error").withMessage("terminal cleanup error: " + cleanupErr.Error()))
		}
		c.finalizeRemainingTasks()
		c.saveHistoryAndSession(ctx, steps)
		c.recordRunAborted(ctx.Err())
		return "", ctx.Err()
	}

	c.saveHistoryAndSession(ctx, steps)

	finalResult := strings.TrimPrefix(result, "FINISHED:")

	c.addSessionAssistantMessage(finalResult)
	if c.sessionData != nil && c.session != nil && c.session.Workspace != "" {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}

	c.finalizeNormalCompletion()
	c.report(c.newEvent("done").withAgent(orchDef.Name).withMessage("continuation finished").withData(runResultStatusData(c.LastRunResult())).withTodoID(CoordTodoID))
	if lastRes := c.LastRunResult(); lastRes != nil && !IsRunOutcomeSuccess(lastRes.Outcome) {
		return finalResult, fmt.Errorf("%w: %s", ErrTasksUnresolved, lastRes.Response)
	}
	return finalResult, nil
}

// returnResumableContinuation records the partial response while deliberately
// leaving task and workflow state untouched for the next coordinator run.
func (c *Coordinator) returnResumableContinuation(ctx context.Context, result string, steps []fantasy.StepResult, message string) (string, error) {
	c.saveHistoryAndSession(ctx, steps)
	finalResult := strings.TrimPrefix(result, "FINISHED:")
	c.addSessionAssistantMessage(finalResult)
	if c.sessionData != nil && c.session != nil && c.session.Workspace != "" {
		c.sessionData.Rounds = c.totalRounds()
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}
	c.report(c.newEvent("done").withAgent(c.GetOrchestratorDef().Name).withMessage(message).withData(runResultStatusData(c.LastRunResult())).withTodoID(CoordTodoID))
	if lastRes := c.LastRunResult(); lastRes != nil {
		return finalResult, fmt.Errorf("%w: %s", ErrTasksUnresolved, lastRes.Response)
	}
	return finalResult, ErrTasksUnresolved
}

func runResultStatusData(result *RunResult) map[string]any {
	if result == nil {
		return nil
	}
	data := map[string]any{
		"outcome":          result.Outcome,
		"goal_satisfied":   result.GoalSatisfied,
		"goal_mode":        result.GoalMode,
		"stop_reason":      result.StopReason,
		"status":           FormatCanonicalStatus(result),
		"stats":            result.Stats,
		"tasks_unresolved": result.Stats.TasksUnresolved,
		"attempts_total":   result.Stats.AttemptsTotal,
		"attempts_failed":  result.Stats.AttemptsFailed,
		"metrics":          result.Metrics,
	}
	if result.Acceptance != nil {
		data["acceptance_state"] = result.Acceptance.EffectiveState()
		data["acceptance_passed"] = result.Acceptance.IsPassed()
	}
	data["exit_code"] = result.ExitCode
	return data
}
