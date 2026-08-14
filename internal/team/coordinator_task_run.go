package team

// Single-task execution: building the worker prompt, running the agent with
// status reporting, deliverable verification, and failure reflection.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/audit"
	"github.com/kjelly/hufu/internal/hooks"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

// errCoordinatorToolFailure marks a direct coordinator tool error as a hard
// orchestration boundary. It is intentionally distinct from worker failures:
// workers can still report bounded partial results after collecting failure
// evidence, while coordinators must not continue making decisions from it.
var errCoordinatorToolFailure = errors.New("coordinator direct tool failure")

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (string, error) {
	// A checkpointed protocol-incomplete task has already run its worker. It
	// may only re-enter through the result-only repair gate; never recreate the
	// worker agent or replay its tools from this status.
	if c != nil && c.taskTracker != nil && todoID != "" {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil && item.ID == todoID && item.Status == TaskProtocolIncomplete {
				return c.resumeProtocolIncompleteTask(parentCtx, task, item)
			}
		}
	}
	if err := c.validateContractStructural(task, todoID); err != nil {
		return "", err
	}
	if task.Action != nil {
		if c == nil || c.phaseWorkflow == nil {
			return "", fmt.Errorf("structured action %q has no runtime workflow", task.Action.Type)
		}
		return c.executeRuntimeAction(parentCtx, task, todoID)
	}
	if len(task.Execution.Steps) > 0 {
		return c.executeStructuredCoordinatorTask(parentCtx, task, todoID)
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

	// Model selection is a runtime capability service so workers, local
	// providers, and tests share the same allowlist and precedence rules.
	resolvedModel, err := c.ModelRuntime().ResolveTaskModel(agentDef, task)
	if err != nil {
		return "", err
	}

	agentTimeout := time.Duration(c.session.Config.Timeout) * time.Second
	if agentDef.Timeout > 0 {
		agentTimeout = time.Duration(agentDef.Timeout) * time.Second
	}
	if agentTimeout <= 0 {
		agentTimeout = 600 * time.Second
	}

	maxRetries := c.effectiveWorkerMaxAttempts(agentDef)

	if err := c.CommitTaskTransition(parentCtx, todoID, TaskPending, TaskInProgress, "", "", nil); err != nil {
		return "", fmt.Errorf("mark task started: %w", err)
	}
	c.reconcileTaskStatusProjection()
	if agentDef.Skills != "" {
		skills := strings.Split(agentDef.Skills, ",")
		for i, s := range skills {
			skills[i] = strings.TrimSpace(s)
		}
		c.taskTracker.TodoList().SetSkills(todoID, skills)
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

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
	var exposedToolNames []string
	var resolvedTools ResolvedWorkerTools
	if c.workerAgentOverride != nil {
		ag = c.workerAgentOverride
	} else {
		extras := []fantasy.AgentTool{&submitResultTool{coordinator: c, todoID: todoID}}
		if task.PlanFirst && task.PlanID == "" {
			extras = append(extras, &submitPlanTool{coordinator: c, todoID: todoID})
		}
		resolvedTools, err = c.ToolResolver().ResolveTaskTools(parentCtx, agentDef, task, extras)
		if err != nil {
			c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()).withTodoID(todoID))
			c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(err, ""))
			return "", err
		}
		exposedToolNames = resolvedTools.Names
	}

	// Instructions must only name tools this worker can actually call. The
	// stream gate is fail-closed, so inviting the worker to use an ungranted
	// tool converts an obedient worker into a dead attempt.
	granted := toolNameSet(exposedToolNames)

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
		prompt += "\n\n## Instructions\n\nExecute the approved plan above. You have already planned — now implement each step.\n"
		prompt += c.sharedKnowledgeInstructions(granted)
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
		prompt += "\n\n## Instructions\n\nYou are a domain expert. Determine your own implementation approach based on the goal above.\n"
		prompt += c.sharedKnowledgeInstructions(granted)
	}
	if task.Verify != "" {
		prompt += completionVerificationInstructions(task.Verify, c.projectDir)
	}
	// The result protocol is enforced for every non-sidecar task, so it has to
	// be stated. A worker that ends its turn with prose fails the contract, and
	// the failure is indistinguishable from real non-completion.
	if !task.PlanFirst || task.PlanID != "" {
		prompt += resultProtocolInstructions(task, granted)
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
		modelSpec = globalRegistry.GetSpec(c.resolveAgentModel(agentDef, task.Model)).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(agentDef))
	}

	rawSTM, rawLTM := "", ""
	memoryStore := (*memory.MemoryStore)(nil)
	var canonicalMemory *CanonicalContextBundle
	canonical := false
	// The retrieval query is the raw task goal, not the expanded prompt. The
	// manifest must hash the same value so explain-memory can bind the actual
	// retrieval query to its retrieval ID (spec §5.1, §7 HF-MEM4-005).
	retrievalQuery := task.Goal
	if !c.ExecutionProfile().DisableHistoricalMemory {
		bundle, foundCanonical, memoryErr := c.canonicalContextBundleForQuery(parentCtx, retrievalQuery)
		if memoryErr != nil {
			return "", fmt.Errorf("worker canonical memory preflight failed: %w", memoryErr)
		}
		canonical, canonicalMemory = foundCanonical, bundle
	}
	if !c.ExecutionProfile().DisableHistoricalMemory && !canonical {
		rawSTM, rawLTM, memoryStore = LoadSTM(c.session.Workspace), LoadLTM(c.session.Workspace, c.session.Config.Name), c.memoryStore
	}
	workerInput := WorkerContextInput{
		TaskGoal:          prompt,
		TaskDef:           task,
		AgentDef:          agentDef,
		RawSTM:            rawSTM,
		RawLTM:            rawLTM,
		ContextFiles:      contextFiles,
		ConcurrentTasks:   c.buildConcurrentTasksContext(todoID),
		DependencyResults: depResults,
		MemoryStore:       memoryStore,
		CanonicalMemory:   canonicalMemory,
		ModelContext:      modelSpec,
		MaxAuxChars:       maxWorkerAuxContextChars,
		DisableMemory:     c.ExecutionProfile().DisableHistoricalMemory,
	}

	// WP-3: recall per-worker private memory before dispatch. The bundle is
	// injected as a typed section in the compiled context, not raw Markdown.
	if memBundle := c.recallWorkerMemory(parentCtx, agentDef, task.Goal); memBundle != nil {
		workerInput.WorkerMemory = memBundle
	}

	// Canonical typed compilation is the only model-visible context assembly.
	// Context files, STM/LTM, concurrent tasks, memory, and dependency results
	// are already represented separately in workerInput; duplicating a
	// char-truncated legacy bundle here would bypass authority/conflict checks.
	workerInput.TaskGoal = prompt
	compiled, compileErr := c.ContextCompiler().CompileWorkerContext(parentCtx, workerInput)
	c.recordShadowTrace(parentCtx, "worker", prompt, workerInput.ModelContext, compiled, compileErr)
	if compileErr != nil {
		return "", fmt.Errorf("worker context preflight failed: %w", compileErr)
	}
	if strings.TrimSpace(compiled.Prompt) == "" {
		return "", fmt.Errorf("worker context preflight failed: compiled prompt is empty")
	}
	prompt = compiled.Prompt

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
	// lastFingerprint tracks the previous attempt's failure fingerprint
	// so DecideRecovery can detect repeated failures via normalised digest
	// comparison (§6.1) rather than raw err.Error() equality.
	var lastFingerprint string
	// Retry-context tracking (§6.1: retry prompt must include class,
	// evidence, prior command/exit, and explicit mutable fields).
	var lastClass TaskFailureClass // previous attempt's failure class
	var lastTranscriptRef string   // previous attempt's opaque transcript artifact reference
	var lastVerifyCmd string       // previous attempt's verify command
	var lastVerifyExit int         // previous attempt's verify exit code (-1 = unknown)
	var lastExitCode *int          // previous attempt's worker exit code (nil = errored)
	var lastToolCall string        // previous attempt's last tool call name (if any)
	var lastToolInput string       // previous attempt's last tool call input (actual command)
	var lastToolResult string      // previous attempt's last tool result preview
	var lastToolResultErr bool     // previous attempt's last tool result was an error
	var lastPartialOutput string   // previous attempt's partial output (evidence)
retryLoop:
	for attempt := 1; attempt <= maxRetries; attempt++ {
		attemptsMade = attempt
		c.setCurrentTaskAttempt(todoID, attempt)
		// Per-attempt tool call evidence — reset at the start of each
		// attempt to prevent stale data from a prior attempt or task.
		attemptEvidence := &toolCallEvidence{}
		var attemptSequence *taskToolSequence
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
			// §5.3: cancelled failures are excluded from retry, fingerprint
			// and anti-thrashing statistics. Classify via structured inputs
			// (parent context error carries the cancellation signal; the
			// stored verification result supplies the verify exit code so a
			// non-zero verify-command exit classifies as FailureVerify via
			// objective evidence rather than error-text matching, §5/§5.2)
			// and skip recordRetry for cancelled classes.
			//
			// The verify result read here is the *previous* attempt's result
			// (the failure being classified). Before clearing the todo-wide
			// slot, attach it to the previous attempt's ExecutionReceipt so
			// the verification evidence (command, exit code, stdout, stderr)
			// remains accessible for forensics (§5, §9). The slot is then
			// cleared so the new attempt starts without a stale todo-wide
			// verify result — otherwise an attempt that fails before reaching
			// verification would inherit the prior attempt's exit code and
			// misclassify (§5, §5.1).
			verifyResult := verifyResultForTodo(c, todoID)
			class := ClassifyTaskFailureStructured(FailureClassificationInput{
				Err:            lastErr,
				ContextErr:     parentCtx.Err(),
				ExitCode:       exitCodeFromVerifyResult(verifyResult),
				ExitCodeSource: ExitCodeSourceVerify,
				// §5.1: environment evidence (command not found / executable
				// unresolved) in the verify result must take precedence over
				// the exit code, so a verify command that itself fails to
				// resolve is not misclassified as a verification failure.
				ResolveFindings: environmentFindingsFromVerifyResult(verifyResult),
			})
			if !IsCancelledClass(class) {
				c.recordRetry(class)
			}
			// Retain the prior attempt's verification evidence on its
			// ExecutionReceipt before clearing the todo-wide slot, so
			// forensics can still access it (§5, §9 evidence retention).
			// Match on (runID, attempt) so a crash-resumed run does not
			// overwrite a prior run's receipt that shares the attempt
			// number.
			if verifyResult != nil {
				priorRunID := c.executionRunID
				if priorRunID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
					priorRunID = c.taskTracker.TodoList().RunID()
				}
				c.attachVerifyResultToReceipt(priorRunID, todoID, attempt-1, verifyResult)
				_ = c.taskTracker.TodoList().SetVerificationResult(todoID, nil)
			}
			// A submitted partial/failed/blocked result belongs to the prior
			// execution attempt. Preserve it in that attempt's receipt, but do
			// not let it satisfy RequiresResult before the new worker runs.
			c.clearSubmittedTaskResult(todoID)
		}
		attemptStarted := time.Now()
		runID := c.executionRunID
		if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			runID = c.taskTracker.TodoList().RunID()
		}
		attemptManifest := buildMemoryInjectionManifest(compiled, runID, todoID, attempt, agentName, retrievalQuery, c.session.Config.MemoryLearning)
		if err := c.persistMemoryManifest(attemptManifest); err != nil {
			closeTranscript()
			return "", fmt.Errorf("worker memory manifest preflight failed: %w", err)
		}
		c.recordExecutionEvent(todoID, agentName, attempt, "in_progress", resolvedModel, 0, ExecutionUsage{})
		currentPrompt := prompt
		if attempt > 1 {
			detail := fmt.Sprintf("retry %d/%d", attempt, maxRetries)
			if err := c.commitTaskTransitionFromCurrent(parentCtx, todoID, TaskInProgress, detail, "", nil); err != nil {
				closeTranscript()
				return "", fmt.Errorf("mark retry task started: %w", err)
			}
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("retry %d/%d — continuing from previous progress", attempt, maxRetries)))
			// §6.1: Build structured retry context with failure class,
			// evidence reference, prior command/exit, and explicit mutable
			// fields. This is appended to the prompt so the worker knows
			// exactly what failed and what it can change.
			if lastErr != nil {
				retryCtx := buildRetryContext(lastClass, lastErr, lastTranscriptRef, lastVerifyCmd, lastVerifyExit, lastExitCode, lastToolCall, lastToolInput, lastToolResult, lastToolResultErr, lastPartialOutput, task)
				currentPrompt += retryCtx
				failureEvidence := redactRetryText(lastErr.Error(), 500)
				if hint := c.reflectOnFailure(parentCtx, agentName, task.Goal, failureEvidence); hint != "" {
					currentPrompt += hint
					appliedHint = strings.TrimPrefix(hint, reflectionHeader)
					appliedHintTrigger = failureEvidence
					c.rememberDiagnosticHint(todoID, appliedHint)
				}
			}
			if escalate {
				if next := nextStrongerModel(c.modelList, resolvedModel); next != "" {
					if _, escErr := c.ModelRuntime().ProviderFor(next); escErr == nil {
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
		// attemptTokens is assigned inside the closure below and read after it
		// returns, so its growth-based snapshot (see attempt_budget.go) can
		// feed the no-progress budget without re-summing steps and
		// double-counting resent conversation history the way usageFromSteps
		// legitimately does for cost/receipt reporting.
		var attemptTokens *attemptBudget
		protocolFailure := false
		// A worker that consumed its entire step budget was cut off; it did not
		// choose to stop. That distinction decides whether the follow-up turn
		// should ask it to finalize what it has or to change its approach.
		stepBudget := c.stepBudget(agentDef, agent.DefaultMaxSteps)
		func() {
			taskCtx, cancel := tools.WithInteractiveAwareTimeout(parentCtx, agentTimeout)
			defer cancel()
			taskCtx, roundCancel := context.WithCancel(taskCtx)
			defer roundCancel()
			c.registerTerminalRound(todoID, roundCancel)
			defer c.unregisterTerminalRound(todoID)
			taskCtx = context.WithValue(taskCtx, todoIDKey{}, todoID)
			taskCtx = context.WithValue(taskCtx, executionAttemptKey{}, attempt)
			taskCtx = context.WithValue(taskCtx, modelKey{}, resolvedModel)
			// Per-attempt tool call evidence (§6.1, P1b: per-attempt, not
			// coordinator-global, to prevent cross-task leakage).
			taskCtx = context.WithValue(taskCtx, toolCallEvidenceKey{}, attemptEvidence)
			taskCtx = context.WithValue(taskCtx, llmUsageReceiptExpectedKey{}, true)
			// The run-level budget is only observed at coordinator boundaries.
			// Give every worker attempt its own live guard so one TUI/debugging
			// loop cannot burn the entire run before returning control here.
			attemptTokens = newAttemptBudget(c.reliabilityConfig().MaxTokensPerAttempt)
			taskCtx = context.WithValue(taskCtx, attemptBudgetKey{}, attemptTokens)
			taskCtx = context.WithValue(taskCtx, tools.AgentNameKey, agentName)
			// Let a poller see whether the process it is waiting on is still
			// alive. Without this a wait on a terminal that has already exited
			// runs to its full timeout: one real run lost 110 minutes that way.
			taskCtx = tools.WithTerminalLiveness(taskCtx, c.terminalLivenessProbe())
			taskCtx = context.WithValue(taskCtx, hooks.AgentNameKey, agentName)
			taskCtx = context.WithValue(taskCtx, hooks.TeamNameKey, c.session.Config.Name)
			taskCtx = context.WithValue(taskCtx, hooks.TaskDescKey, taskDesc)
			if transcript != nil {
				taskCtx = context.WithValue(taskCtx, taskTranscriptKey{}, transcript)
			}
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

			taskCtx = c.withEffectiveToolsAllowedForTask(taskCtx, agentDef, exposedToolNames, task)
			if sequence := newTaskToolSequenceWithBindings(task.Execution.ToolSequence, task.Execution.ToolInputSequence, task.Execution.ToolInputField, task.Execution.ToolInputValueSequence, task.Execution.ToolExpectedExitCodes, task.Execution.ToolInputCanonicalSequence, task.Execution.ToolInputTransformSequence); sequence != nil {
				attemptSequence = sequence
				taskCtx = context.WithValue(taskCtx, taskToolSequenceKey{}, sequence)
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

			if c.workerAgentOverride != nil {
				output, steps, err = c.runAgentWithStatusAndHistory(taskCtx, ag, agentName, currentPrompt, conversationHistory, timing)
			} else {
				provider, providerErr := c.SubagentRegistry().Resolve(localSubagentProviderName)
				if providerErr != nil {
					err = providerErr
				} else {
					attemptResult, runErr := provider.RunAttempt(taskCtx, AttemptRequest{
						RunID:    runID,
						BranchID: c.activeBranchID(),
						TaskID:   todoID,
						Attempt:  attempt,
						Agent:    agentDef,
						Task:     task,
						Prompt:   currentPrompt,
						ModelID:  resolvedModel,
						MaxSteps: stepBudget,
						Tools:    resolvedTools,
						History:  conversationHistory,
						timing:   timing,
					})
					output, steps, err = attemptResult.Output, attemptResult.steps, runErr
					ag = attemptResult.agent
				}
			}
			if err == nil && !task.Execution.RequiresResult && strings.TrimSpace(output) == "" && len(steps) > 0 {
				// The agent worked but never wrote a final message — almost
				// always the step cap cutting it off mid-diagnosis. Give it one
				// tool-free turn to summarize instead of failing the task and
				// re-running everything from scratch. Requires-result workers
				// instead receive the dedicated result-only finalization turn
				// below, where submit_result is the sole exposed tool.
				if rescued := c.rescueFinalSummary(taskCtx, ag, agentName, steps, timing); rescued != "" {
					output = rescued
				}
			}
		}()
		transcriptRef := ""
		var transcriptArtifact *ArtifactRef
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
				transcriptArtifact = ref
				transcriptRef = ref.ID
			}
		}
		receipt := ExecutionReceipt{
			RunID:          runID,
			TaskID:         todoID,
			Attempt:        attempt,
			StartedAt:      attemptStarted,
			FinishedAt:     time.Now(),
			ProducerID:     agentName,
			TranscriptRef:  transcriptRef,
			MemoryManifest: cloneMemoryInjectionManifest(attemptManifest),
			StepBudget: &StepBudgetUsage{
				Used:      len(steps),
				Limit:     stepBudget,
				Exhausted: stepBudget > 0 && len(steps) >= stepBudget,
			},
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
		err, terminalBlocked := c.finalizeTaskTerminalResources(parentCtx, todoID, err)

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
				if transitionErr := c.commitTaskTransitionFromCurrent(parentCtx, todoID, TaskPlanned, "", "", nil); transitionErr != nil {
					return "", transitionErr
				}
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("step").withAgent(agentName).withMessage("plan submitted").withTodoID(todoID))
				c.report(c.newEvent("done").withAgent(agentName).withMessage("plan submitted").withTodoID(todoID))
				c.recordExecutionEvent(todoID, agentName, attempt, "planned", resolvedModel, time.Since(attemptStarted), usageWithProgressTokens(steps, attemptTokens))
				if c.forcePlanFirst {
					closeTranscript()
					return "", nil
				}
				closeTranscript()
				return planEntry.PlanText, nil
			}
			typedRes = c.GetTaskResult(todoID)
			// A worker that already reported partial/failed/blocked via
			// submit_result has nothing left for the checks below to
			// contradict — it already told us the task is not complete. Fail
			// the attempt on that honest report now, before validateTaskOutput,
			// deliverable/adversarial verification, or terminalTaskFailure run,
			// so the retry hint reflects what the worker actually said instead
			// of whichever unrelated check happens to trip first. This mirrors
			// the check the protocol-repair recovery paths below already apply
			// to a recovered result; only the plain submit_result path lacked it.
			if typedRes != nil && typedRes.Source == "submitted" {
				if resultErr := validateSubmittedTaskResult(typedRes); resultErr != nil {
					err = resultErr
					failedExit := 1
					receipt.ExitCode = &failedExit
					if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
						_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
					}
				}
			}
			if typedRes == nil {
				if task.Execution.RequiresResult {
					// Protocol failure: the agent finished execution but omitted submit_result.
					// Classify as FailureProtocol, set task to protocol_incomplete,
					// and attempt single-step, tool-free repair allowing ONLY submit_result.
					protocolFailure = true
					// §8: the step budget covers work; result finalization gets its
					// own turn outside it. Distinguishing exhaustion from a genuine
					// protocol violation keeps the retry hint honest — telling a
					// truncated worker to "change your approach" is what turns a
					// nearly-finished task into a thrashing loop.
					budgetExhausted := receipt.StepBudget != nil && receipt.StepBudget.Exhausted
					protocolErrMsg := fmt.Sprintf("protocol-only failure for task %s (%s): agent omitted submit_result; entering protocol_incomplete for tool-free repair (class: %s)",
						todoID, agentName, string(FailureProtocol))
					protocolDetail := "protocol incomplete: missing required result"
					if budgetExhausted {
						protocolDetail = fmt.Sprintf("protocol incomplete: step budget exhausted (%d/%d steps) before submit_result", len(steps), stepBudget)
						protocolErrMsg = fmt.Sprintf("step budget exhausted for task %s (%s) after %d/%d steps; finalizing result from execution evidence",
							todoID, agentName, len(steps), stepBudget)
					}
					protocolFailureDetail := c.FailureDetail(errors.New(protocolDetail), FailureSourceError)
					c.PersistFailureWithClassAndStatusAndOutput(agentName, taskDesc, todoID, protocolFailureDetail, ReconcileOnly, FailureProtocol, TaskProtocolIncomplete, output)
					c.reconcileTaskStatusProjection()
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					c.report(c.newEvent("step").withAgent(agentName).withMessage(protocolErrMsg).withTodoID(todoID))

					repairResultTool := &submitResultTool{coordinator: c, todoID: todoID}
					repairPrompt := fmt.Sprintf("## Goal\n%s\n\n## Execution Output\n%s\n\n## Repair Instructions\nYour execution completed and produced output, but you did not submit a structured result via submit_result as required. Call submit_result now using the output above to supply the required structured result. Include a concise summary and put any complete plan, analysis, review, or report body in `details`. For `open_questions`, use strings or objects with `question` and optional string `context`/`detail` fields. Do NOT call any other tools or emit a prose final response.\n", task.Goal, output)
					if budgetExhausted {
						repairPrompt = fmt.Sprintf("## Goal\n%s\n\n## Execution Output\n%s\n\n## Finalization Instructions\nYou ran out of steps (%d/%d) before submitting a result. The work above is your evidence; this turn is only for reporting it. Call submit_result now, and do NOT call any other tools or emit a prose final response. Put any complete plan, analysis, review, or report body in `details`. For `open_questions`, use strings or objects with `question` and optional string `context`/`detail` fields.\n\nReport truthfully against the goal: use status `success` only if the goal is fully met without a known target limitation; use `completed_with_gaps` when the assigned work is complete but it discovered such a limitation; otherwise use `partial` (say exactly what is done and what remains) or `blocked`. A truthful `partial` lets the next attempt continue your work; a false completion claim destroys it.", task.Goal, output, len(steps), stepBudget)
					}
					// §7 requires the transcript to survive as evidence, and the
					// finalization turn is where that evidence gets converted into a
					// result. Handing it only the final prose made it guess; the
					// attempt's own tool calls and results are what it needs to
					// report accurately.
					var repairHistory []fantasy.Message
					for _, step := range steps {
						repairHistory = append(repairHistory, step.Messages...)
					}
					if len(repairHistory) > 0 {
						repairHistory = append([]fantasy.Message{fantasy.NewUserMessage(currentPrompt)}, repairHistory...)
					}
					repairAttempts := make([]RepairAttemptProvenance, 0, 2)
					runRepair := func(prompt string) []fantasy.StepResult {
						var repairAg fantasy.Agent
						if c.repairAgentOverride != nil {
							repairAg = c.repairAgentOverride
						} else if c.providerManager != nil {
							var rErr error
							repairAg, rErr = c.createGatedAgent(parentCtx, c.providerManager.GetProvider(resolvedModel), agent.AgentConfig{
								Def:        agentDef,
								TeamConfig: &c.session.Config,
								WorkDir:    c.projectDir,
								MaxSteps:   1,
							}, []fantasy.AgentTool{repairResultTool})
							if rErr != nil {
								c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("failed to create protocol repair agent: %v", rErr)).withTodoID(todoID))
							}
						}
						if repairAg == nil {
							return nil
						}
						repairCtx := context.WithValue(parentCtx, tools.AgentToolsAllowedKey, []string{"submit_result"})
						// Protocol repair has no separate execution receipt. The
						// parent worker context is receipt-backed, so override its
						// marker before accounting this auxiliary LLM stream.
						repairCtx = context.WithValue(repairCtx, llmUsageReceiptExpectedKey{}, false)
						repairCtx = context.WithValue(repairCtx, todoIDKey{}, todoID)
						repairCtx = context.WithValue(repairCtx, modelKey{}, resolvedModel)
						repairCtx = context.WithValue(repairCtx, tools.AgentNameKey, agentName)
						repairCtx = context.WithValue(repairCtx, hooks.AgentNameKey, agentName)
						repairCtx = context.WithValue(repairCtx, hooks.TeamNameKey, c.session.Config.Name)
						repairCtx = context.WithValue(repairCtx, hooks.TaskDescKey, taskDesc)
						repairCtx = context.WithValue(repairCtx, taskToolSequenceKey{}, attemptSequence.protocolRepairSequence())

						_, repairSteps, _ := c.runAgentWithStatusAndHistory(repairCtx, repairAg, agentName, prompt, repairHistory, timing, fantasy.StepCountIs(1))
						typedRes = c.GetTaskResult(todoID)
						return repairSteps
					}

					repairSteps := runRepair(repairPrompt)
					repairSuccess := typedRes != nil && typedRes.Source == "submitted" && validateSubmittedTaskResult(typedRes) == nil
					// §7: classify the repair failure sub-reason so the next-step
					// disposition is driven by evidence rather than a generic
					// "protocol failed" message. progress_not_final reclassifies
					// the task as an execution failure (the worker reported a
					// progress update, not a final outcome) and must not count
					// toward protocol repair statistics.
					repairReason, reclassifyExecution := classifyRepairFailure(repairSteps, typedRes)
					repairAttempts = append(repairAttempts, RepairAttemptProvenance{
						Attempt:         1,
						Success:         repairSuccess,
						Prompt:          repairPrompt,
						SubmittedResult: typedRes,
						FailureReason:   repairReason,
					})

					// An invalid schema is the one protocol failure allowed a second
					// time. This turn is schema-only, still allows only submit_result,
					// and never replays the worker execution (§7).
					if !repairSuccess && repairReason == RepairFailureInvalidSchema {
						typedRes = nil
						schemaRepairPrompt := fmt.Sprintf("## Goal\n%s\n\n## Schema-only repair\nThe previous submit_result call was rejected because its arguments did not match the result schema. This is the final repair attempt. Call submit_result exactly once with corrected schema and preserve the execution facts below. Do NOT execute work, call any other tools, or emit a prose final response. The call must include both required fields: `status` (one of `success`, `completed_with_gaps`, `partial`, `failed`, or `blocked`) and a non-empty `summary`; put any complete textual deliverable in `details`. For `open_questions`, use strings or objects with `question` and optional string `context`/`detail` fields.\n\n## Execution Output\n%s", task.Goal, output)
						schemaRepairSteps := runRepair(schemaRepairPrompt)
						repairSuccess = typedRes != nil && typedRes.Source == "submitted" && validateSubmittedTaskResult(typedRes) == nil
						repairReason, reclassifyExecution = classifyRepairFailure(schemaRepairSteps, typedRes)
						repairAttempts = append(repairAttempts, RepairAttemptProvenance{
							Attempt:         2,
							Success:         repairSuccess,
							Prompt:          schemaRepairPrompt,
							SubmittedResult: typedRes,
							FailureReason:   repairReason,
						})
					}

					receipt.FinishedAt = time.Now()
					receipt.RepairProvenance = &RepairProvenance{
						Attempted:       true,
						Success:         repairSuccess,
						Prompt:          repairAttempts[len(repairAttempts)-1].Prompt,
						SubmittedResult: typedRes,
						RepairAttempts:  len(repairAttempts),
						History:         repairAttempts,
					}
					if repairSuccess {
						c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("protocol repair succeeded for task %s after %d attempt(s)", todoID, len(repairAttempts))).withTodoID(todoID))
					} else {
						receipt.RepairProvenance.FailureReason = repairReason
						// Capture the submitted status (if any) before preserving the
						// result as evidence for the error message.
						submittedStatus := ""
						if typedRes != nil {
							submittedStatus = typedRes.Status
						}
						if reclassifyExecution {
							// §7: progress_not_final — the submitted result was a
							// progress update (partial/failed/blocked), not a final
							// outcome. Reclassify as FailureExecution so the retry
							// loop may re-dispatch the worker (subject to the
							// replay policy), and do not count this attempt toward
							// protocol repair statistics.
							protocolFailure = false
							err = withFailureClassOverride(
								fmt.Errorf("execution failure (reclassified from protocol repair: worker reported status %q via submit_result; task is not complete) for task %s (%s)", submittedStatus, todoID, agentName),
								FailureExecution,
							)
							receipt.RepairProvenance.Error = err.Error()
						} else {
							// Preserve the worker's original output as a low-confidence,
							// provisional result. It is evidence for reconciliation, not
							// a successful terminal result and never marks the task done.
							recovered := ParseFreeTextResult(output)
							if strings.TrimSpace(recovered.Summary) == "" {
								recovered.Summary = "No final worker output was available; reconcile using the execution transcript."
							}
							recovered.TaskID = todoID
							recovered.Agent = agentName
							recovered.Source = "recovered_protocol"
							if transcriptArtifact != nil {
								copyRef := *transcriptArtifact
								recovered.RawOutputRef = &copyRef
							}
							c.storeSubmittedTaskResult(todoID, recovered)
							typedRes = recovered
							receipt.RepairProvenance.SubmittedResult = recovered
							err = fmt.Errorf("protocol failure (class: %s, reason: %s) for task %s (%s): agent produced output but failed protocol repair to submit_result",
								string(FailureProtocol), string(repairReason), todoID, agentName)
							receipt.RepairProvenance.Error = err.Error()
						}
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
			// For requires-result tasks the typed submission is the terminal
			// handoff. A final-action-only worker is correctly instructed not to
			// add prose after submit_result, so validate the authoritative typed
			// handoff rather than rejecting an otherwise valid empty final text.
			coordinatorOutput := coordinatorTaskOutput(output, typedRes)
			completionOutput := coordinatorOutput
			if strings.TrimSpace(completionOutput) == "" && typedRes != nil && typedRes.Source == "submitted" {
				completionOutput = typedRes.FormatForContext()
			}
			if err == nil {
				if verr := validateTaskOutput(task, completionOutput); verr != nil {
					err = fmt.Errorf("task completion validation failed: %w", verr)
					c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("completion validation failed: %v", verr)).withTodoID(todoID))
				}
			}
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
			if err == nil && (task.Verify != "" || task.VerifySpec != nil) {
				if statusErr := c.commitTaskTransitionFromCurrent(parentCtx, todoID, TaskVerifying, "running objective verification", "", nil); statusErr != nil {
					err = fmt.Errorf("enter verifying state: %w", statusErr)
				} else {
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					vMsg := task.Verify
					if vMsg == "" && task.VerifySpec != nil {
						vMsg = string(task.VerifySpec.Type)
					}
					c.report(c.newEvent("verify_start").withAgent(agentName).withMessage(vMsg).withTodoID(todoID))
				}
				if err == nil {
					verification, verr := c.verifyTaskDeliverableWithSpec(parentCtx, agentDef, task)
					if verification != nil {
						c.noteObjectiveVerifierResult(todoID, verr == nil && isVerifySuccess(verification))
						_ = c.taskTracker.TodoList().SetVerificationResult(todoID, verification)
					}
					if verr != nil {
						err = fmt.Errorf("deliverable verification failed: %w", verr)
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
					c.recordMemoryOutcomeSignalForTaskID(todoID, "skeptic_passed", "positive", 0.8)
				}
			}
			// A terminal child is an independent source of truth. Do this even
			// when the task has no objective verifier: a worker success claim must
			// not turn a failed terminal command into a successful task.
			if err == nil {
				if terminalErr := c.terminalTaskFailure(parentCtx, todoID); terminalErr != nil {
					err = terminalErr
					c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("terminal evidence rejected completion: %v", terminalErr)).withTodoID(todoID))
				}
			}
			if err == nil {
				if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "done", taskDesc, coordinatorOutput); err != nil {
					log.Printf("warning: failed to write task file: %v", err)
				}
				duration, modelTime, toolTime := timing.snapshot()
				if statusErr := c.commitTaskTransitionFromCurrent(parentCtx, todoID, TaskDone, utils.TruncateRunes(coordinatorOutput, summaryMaxRunes), coordinatorOutput, nil); statusErr != nil {
					closeTranscript()
					return "", fmt.Errorf("mark task done: %w", statusErr)
				}
				c.recordTerminalTypedTaskResult(todoID)
				c.reconcileTaskStatusProjection()
				for _, item := range c.taskTracker.TodoList().Items() {
					if item.ID == todoID {
						c.reEvaluateAffectedCriteria(parentCtx, item)
						break
					}
				}
				c.updateTodoTiming(todoID, modelTime, toolTime)
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("done").withAgent(agentName).withOutput(coordinatorOutput).withMessage("completed").withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
				c.recordExecutionEvent(todoID, agentName, attempt, "done", resolvedModel, time.Since(attemptStarted), usageWithProgressTokens(steps, attemptTokens))
				if task.Summarize {
					coordinatorOutput = c.summarizeOutput(parentCtx, coordinatorOutput)
				}
				c.autoWriteSTMASync(agentName, taskDesc, coordinatorOutput, "", true)
				// WP-4: ingest verified session memory into the worker's private
				// store. This is best-effort: a failure never inverts the task
				// success (the helper logs + emits an event + queues for repair).
				// Idempotency: the canonical store deduplicates by execution
				// identity, so a fast-path-to-team upgrade re-ingest is a no-op.
				verified := false
				if vr := verifyResultForTodo(c, todoID); vr != nil && isVerifySuccess(vr) {
					verified = true
				}
				c.reduceTaskResultToSharedMemory(parentCtx, TaskResultMemoryInput{
					TodoID: todoID, Agent: agentDef, Result: typedRes, Output: coordinatorOutput, Verified: verified, Attempt: attempt,
				})
				c.ingestWorkerSessionMemory(parentCtx, agentDef, todoID, typedRes, coordinatorOutput, verified, attempt)
				if appliedHint != "" {
					c.persistReflexionLessonAsync(agentName, todoID, task.Goal, appliedHintTrigger, appliedHint, true, false)
				}
				closeTranscript()
				return coordinatorOutput, nil
			}
		}

		c.recordExecutionEvent(todoID, agentName, attempt, "error", resolvedModel, time.Since(attemptStarted), usageWithProgressTokens(steps, attemptTokens))
		// Classify the current attempt's failure using structured inputs
		// (§5: the verify result supplies the exit code; environment findings
		// supply command-not-found signals that take precedence over exit
		// codes per §5.1).
		verifyResult := verifyResultForTodo(c, todoID)
		currentClass := ClassifyTaskFailureStructured(FailureClassificationInput{
			Err:             err,
			ContextErr:      parentCtx.Err(),
			ExitCode:        exitCodeFromVerifyResult(verifyResult),
			ExitCodeSource:  ExitCodeSourceVerify,
			ResolveFindings: environmentFindingsFromVerifyResult(verifyResult),
		})

		// Compute the current attempt's failure fingerprint for §6.1
		// anti-thrashing repeat detection. The fingerprint uses the
		// normalised error digest (via NewFailureFingerprint) so that
		// differently formatted errors with the same underlying failure
		// are detected as repeats.
		currentFingerprint := NewFailureFingerprint(
			todoID, agentName, stableOperationFromTask(task), currentClass, err.Error(),
		).Digest

		// Single decision point: DecideRecovery replaces the five separate
		// early-break if-statements that previously lived here (WP-08).
		// The five paths (terminalBlocked, protocolFailure, replayable,
		// unfixableVerify, sameFailure) are now inputs to one function that
		// prescribes retry / break / block.
		resolvedPolicy := ResolveRecoveryPolicy(task.Recovery, task.SideEffect, c != nil && c.unattended, c.ExecutionProfile())
		recoveryInput := RecoveryDecisionInput{
			FailureClass:        currentClass,
			SideEffect:          task.SideEffect,
			RecoveryPolicy:      resolvedPolicy,
			Attempt:             attempt,
			MaxRetries:          maxRetries,
			EvidenceComplete:    computeEvidenceComplete(task, transcriptRef, steps, output),
			FailureFingerprint:  currentFingerprint,
			PreviousFingerprint: lastFingerprint,
			TerminalBlocked:     terminalBlocked,
			ProtocolFailure:     protocolFailure,
			UnfixableVerify:     isUnfixableVerifyFailure(err),
			SameFailureRepeated: lastErr != nil && sameFailure(lastErr.Error(), err.Error()),
			ContextCancelled:    parentCtx.Err() != nil,
			Replayable:          CanAutomaticallyReplay(task),
			ProtocolRepairRetry: c.protocolRepairAllowsRetry(task),
		}
		disposition, reason := DecideRecovery(recoveryInput)
		if isAttemptBudgetExceeded(err) {
			// Re-running an attempt that exhausted its own budget without a
			// materially different plan is precisely the expensive thrashing
			// this guard is intended to stop. Hand the coordinator a bounded
			// partial result and require an explicit replan instead.
			disposition = ReplanRequired
			reason = "per-attempt token budget exhausted; replan before retry"
		}
		// RepairController is the phase-3 safety gate for the legacy
		// disposition engine. Keep the existing disposition as the source of
		// detailed failure-class behavior, but never allow it to replay a task
		// that the centralized side-effect policy blocks.
		repairDecision := c.RepairController().Decide(RepairRequest{
			Task:            task,
			Attempt:         attempt,
			MaxAttempts:     maxRetries,
			BudgetExhausted: parentCtx.Err() != nil,
		})
		_ = c.emitEvent("repair_decision", "repair_controller", todoID, map[string]interface{}{
			"action": string(repairDecision.Action), "reason": repairDecision.Reason, "attempt": attempt,
		})
		if disposition == RetryWorker && repairDecision.Action == RepairBlock {
			disposition = ReconcileOnly
			reason = repairDecision.Reason
		}
		// Permission/capability denial is a deterministic human gate. Keep this
		// operational decision ahead of the retry switch; packet persistence must
		// not merely record a block after a worker has already been re-dispatched.
		if isPermissionBlockedFailureDetail(c.FailureDetail(err, "error")) {
			disposition = NeedsHuman
			reason = "capability or permission is unavailable"
		}
		// Make a deliberately avoided worker replay distinct from another
		// failed worker attempt in the event timeline and run metrics. Repair
		// tasks defer repeated-fingerprint reporting to persistFailure, where
		// anti-thrashing can distinguish a rejected recovery strategy.
		if task.Kind != TaskKindRepair {
			if suppressionReason, suppressed := retrySuppressionReason(recoveryInput, disposition); suppressed {
				c.recordRetrySuppression(todoID, currentFingerprint, disposition, suppressionReason)
			}
		}

		// Capture the current attempt's evidence for the next iteration's
		// retry context (§6.1: retry prompt must include class, evidence,
		// prior command/exit). These are saved before the switch so they
		// survive the break/continue decision.
		lastClass = currentClass // nolint:staticcheck,ineffassign
		priorTranscriptRef := transcriptRef
		priorVerifyCmd := ""
		priorVerifyExit := -1
		if vr := verifyResultForTodo(c, todoID); vr != nil {
			priorVerifyCmd = vr.Command
			priorVerifyExit = vr.ExitCode
		}
		priorExitCode := receipt.ExitCode
		priorToolCall := attemptEvidence.toolName
		priorToolInput := attemptEvidence.toolInput
		priorToolResult := attemptEvidence.resultText
		priorToolResultErr := attemptEvidence.resultErr
		priorPartialOutput := retryPartialOutput(output, lastOutput)

		// Build conversation history for a potential retry. This is harmless
		// when the disposition stops the loop; the history is simply unused.
		if len(conversationHistory) == 0 && len(steps) > 0 {
			conversationHistory = append(conversationHistory, fantasy.NewUserMessage(currentPrompt))
		}
		for _, step := range steps {
			conversationHistory = append(conversationHistory, step.Messages...)
		}

		switch disposition {
		case NeedsHuman:
			c.saveCheckpoint()
			// Path 1: an owned terminal session is still active. Persist
			// the failure and stop — retrying is unsafe.
			lastErr = err
			c.report(c.newEvent("step").withAgent(agentName).withMessage("stopping retries: " + reason).withTodoID(todoID))
			c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(err, "error"), NeedsHuman, currentClass)
			closeTranscript()
			break retryLoop

		case ReconcileOnly:
			c.saveCheckpoint()
			// Paths 2/3 (protocol-only non-replayable, or non-replayable
			// task) or class-based protocol/timeout disposition: block the
			// task for reconciliation. Worker tools must not be replayed
			// (§6.1: protocol 只允許 result-only repair；不得重放工具).
			lastErr = err
			source := "error"
			blockedMsg := fmt.Sprintf("automatic replay is not allowed (allows_replay=%v, side_effect=%s); reconcile before retry", task.Execution.AllowsReplay != nil && *task.Execution.AllowsReplay, task.SideEffect)
			if protocolFailure && (!IsTaskReplayable(task) || !c.protocolRepairAllowsRetry(task)) {
				source = "protocol"
				blockedMsg = fmt.Sprintf("protocol result missing; automatic replay is not allowed (allows_replay=%v, side_effect=%s, recovery=%s); reconcile before retry: %v", task.Execution.AllowsReplay != nil && *task.Execution.AllowsReplay, task.SideEffect, task.Recovery, err)
			} else if currentClass == FailureProtocol {
				source = "protocol"
				blockedMsg = fmt.Sprintf("protocol failure; worker tools must not be replayed (side_effect=%s, recovery=%s); reconcile before retry: %v", task.SideEffect, task.Recovery, err)
			}
			failureDetail := c.FailureDetail(err, source) + " | " + blockedMsg
			c.PersistFailureWithClassAndStatus(agentName, taskDesc, todoID, failureDetail, ReconcileOnly, currentClass, TaskBlocked)
			c.report(c.newEvent("step").withAgent(agentName).withMessage("stopping retries: " + reason).withTodoID(todoID))
			closeTranscript()
			break retryLoop

		case ReplanRequired:
			c.saveCheckpoint()
			// Paths 4/5 (unfixable verify, same failure repeated) or
			// class-based contract/environment/policy: stop retrying. The
			// coordinator must produce a new recovery hypothesis before
			// any further dispatch (§5, §6.1).
			// Save the pre-decision previous error before overwriting
			// lastErr so the repeated-failure check compares the prior
			// attempt's error with the current one, not the current with
			// itself (reviewer P2).
			prevErr := lastErr
			lastErr = err
			if isUnfixableVerifyFailure(err) && attempt < maxRetries {
				c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("stopping retries: attempt %d hit a verify command that cannot be fixed by retrying (wrong exit-code polarity)", attempt)).withTodoID(todoID))
				c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(fmt.Errorf("verify command has unfixable wrong polarity after %d attempt(s): %w", attempt, err), "error"), ReplanRequired, currentClass)
			} else if prevErr != nil && sameFailure(prevErr.Error(), err.Error()) && attempt < maxRetries {
				c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("stopping retries: attempt %d repeated the same failure", attempt)).withTodoID(todoID))
				c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(fmt.Errorf("repeated failure after %d attempts: %w", attempt, err), "error"), ReplanRequired, currentClass)
			} else {
				c.report(c.newEvent("step").withAgent(agentName).withMessage("stopping retries: " + reason).withTodoID(todoID))
				c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(err, ""), ReplanRequired, currentClass)
			}
			closeTranscript()
			break retryLoop

		case RetryNone:
			// Cancelled (§5.3) or retry budget exhausted. Persist the
			// failure and stop.
			lastErr = err
			if isTaskTimeout(err) {
				duration, modelTime, toolTime := timing.snapshot()
				c.report(c.newEvent("task_timeout").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d timed out after %s", attempt, duration.Round(time.Second))).withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
			}
			c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)).withModel(resolvedModel).withTodoID(todoID))
			c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(err, ""), RetryNone, currentClass)
			closeTranscript()
			break retryLoop

		default: // RetryWorker
			c.saveCheckpoint()
			// Normal retry: persist the failure and continue to the next
			// attempt. A final context check guards against a race where
			// the context is cancelled between DecideRecovery and here.
			lastErr = err
			lastFingerprint = currentFingerprint
			lastClass = currentClass // nolint:staticcheck,ineffassign
			// lastTranscriptRef is consumed via buildRetryContext → currentPrompt → ag.Generate
			lastTranscriptRef, lastVerifyCmd, lastVerifyExit, lastExitCode, lastToolCall, lastToolInput, lastToolResult, lastToolResultErr, lastPartialOutput = priorTranscriptRef, priorVerifyCmd, priorVerifyExit, priorExitCode, priorToolCall, priorToolInput, priorToolResult, priorToolResultErr, priorPartialOutput //nolint:staticcheck
			if isTaskTimeout(err) {
				duration, modelTime, toolTime := timing.snapshot()
				c.report(c.newEvent("task_timeout").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d timed out after %s", attempt, duration.Round(time.Second))).withModel(resolvedModel).withTiming(duration, modelTime, toolTime).withTodoID(todoID))
			}
			c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)).withModel(resolvedModel).withTodoID(todoID))
			c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(err, ""), RetryWorker, currentClass)
			if parentCtx.Err() != nil {
				closeTranscript()
				break retryLoop
			}
			closeTranscript()
		}
	}

	_, modelTime, toolTime := timing.snapshot()
	c.updateTodoTiming(todoID, modelTime, toolTime)
	// No PersistFailure here: every failure path inside the loop has already
	// persisted this error; persisting again wrote duplicate journal/status
	// records for the same failure.
	if c.contextRepo != nil {
		// Canonical mode: a failed verification/task is reduced to a typed
		// ContextError item (with task + verification/receipt evidence), never
		// to generic ContextProgress. autoWriteSTMASync is kept for the
		// non-canonical Markdown path.
		c.recordVerificationFailure(parentCtx, VerificationFailureInput{
			TodoID:      todoID,
			Agent:       agentDef,
			Attempt:     attemptsMade,
			Err:         lastErr,
			Verify:      verifyResultForTodo(c, todoID),
			ReceiptIDs:  receiptIDsForTodo(c, todoID),
			ArtifactIDs: artifactIDsForTodo(c, todoID),
		})
	} else {
		c.autoWriteSTMASync(agentName, taskDesc, "", lastErr.Error(), false)
	}
	if maxRetries > 1 {
		c.persistReflexionLessonAsync(agentName, todoID, task.Goal, lastErr.Error(), appliedHint, false, isUnfixableVerifyFailure(lastErr))
	}
	failErr := fmt.Errorf("agent %q failed after %d attempt(s) (model: %s): %w", agentName, attemptsMade, resolvedModel, lastErr)
	if strings.TrimSpace(lastOutput) != "" {
		failErr = fmt.Errorf("%w\n\nLast agent output before failure (may contain useful findings):\n%s", failErr, utils.TruncateRunes(lastOutput, 2000))
	}
	return "", failErr
}

func (c *Coordinator) effectiveWorkerMaxAttempts(agentDef *agent.AgentDef) int {
	maxRetries := c.session.Config.MaxRetries
	if agentDef != nil && agentDef.MaxRetries >= 0 {
		maxRetries = agentDef.MaxRetries
	}
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.policies.FailFast {
		return 1
	}
	if maxRetries < 1 {
		return 1
	}
	return maxRetries
}

// executeRuntimeAction gives provider-backed actions the same durable TODO
// lifecycle as worker tasks. In particular, a successful provider response
// must mark its static contract done so the phase state machine can advance.
func (c *Coordinator) executeRuntimeAction(ctx context.Context, task TaskDef, todoID string) (string, error) {
	startedAt := time.Now().UTC()
	c.emitRuntimeActionEvent("action_started", task, todoID, "started", startedAt, time.Time{}, "", nil)
	if err := c.taskTracker.TodoList().SetRuntimeError(todoID, nil); err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("clear structured action error: %w", err)
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskInProgress, "executing structured action", "", nil); err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("mark structured action in progress: %w", err)
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	output, err := c.phaseWorkflow.executeAction(ctx, *task.Action)
	if err != nil {
		runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
		_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
		c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
		c.emitRuntimeActionEvent("action_failed", task, todoID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", err
	}
	if task.Verify != "" || task.VerifySpec != nil {
		if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskVerifying, "running objective verification", output, nil); err != nil {
			c.emitRuntimeActionEvent("action_failed", task, todoID, "failure", startedAt, time.Now().UTC(), "", err)
			return "", fmt.Errorf("enter structured action verification: %w", err)
		}
		verification, verifyErr := c.verifyTaskDeliverableWithSpec(ctx, nil, task)
		if verification != nil {
			_ = c.taskTracker.TodoList().SetVerificationResult(todoID, verification)
		}
		if verifyErr != nil {
			err := fmt.Errorf("structured action verification failed: %w", verifyErr)
			c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
			c.emitRuntimeActionEvent("action_failed", task, todoID, "failure", startedAt, time.Now().UTC(), "", err)
			return "", err
		}
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output, nil); err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("mark structured action done: %w", err)
	}
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(task.Agent).withOutput(output).withMessage("structured action completed").withTodoID(todoID))
	c.emitRuntimeActionEvent("action_completed", task, todoID, "success", startedAt, time.Now().UTC(), output, nil)
	return output, nil
}

type runtimeActionReceipt struct {
	Version    int       `json:"version"`
	RunID      string    `json:"run_id,omitempty"`
	TaskID     string    `json:"task_id"`
	Agent      string    `json:"agent,omitempty"`
	ActionID   string    `json:"action_id"`
	Capability string    `json:"capability"`
	Type       string    `json:"type"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Output     string    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (c *Coordinator) emitRuntimeActionEvent(eventType string, task TaskDef, todoID, status string, startedAt, finishedAt time.Time, output string, actionErr error) {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || task.Action == nil {
		return
	}
	actionID := "structured-action-" + safeNameRegex.ReplaceAllString(todoID, "-") + "-" + fmt.Sprintf("%d", startedAt.UnixNano())
	if strings.TrimSpace(todoID) == "" {
		actionID = "structured-action-unknown-" + fmt.Sprintf("%d", startedAt.UnixNano())
	}
	capability := normalizeCapability(task.Action.Capability)
	providerName := c.phaseWorkflow.providerName(capability)
	refs := []ArtifactRef{}
	failureSignature := ""
	if actionErr != nil {
		failureSignature = c.phaseWorkflow.actionExecutionError(task, actionErr).Signature().String()
	}
	if eventType != "action_started" {
		ref, err := c.writeRuntimeActionReceipt(runtimeActionReceipt{
			Version: 1, RunID: c.executionRunID, TaskID: todoID, Agent: task.Agent, ActionID: actionID,
			Capability: capability, Type: task.Action.Type, Status: status,
			StartedAt: startedAt, FinishedAt: finishedAt,
			Output: utils.TruncateString(utils.RedactSecrets(output), 1000),
			Error: func() string {
				if actionErr == nil {
					return ""
				}
				return utils.TruncateString(utils.RedactSecrets(actionErr.Error()), 1000)
			}(),
		}, actionID)
		if err != nil {
			failureSignature = "runtime_action_receipt_failed: " + utils.TruncateString(utils.RedactSecrets(err.Error()), 300)
			_ = c.emitEvent("observability_degraded", "runtime", todoID, LifecycleEventPayload{
				Phase: string(c.phaseWorkflow.State()), Agent: task.Agent, Provider: providerName,
				Capability: capability, ActionID: actionID, ToolName: task.Action.Type,
				FailureSignature: failureSignature, Artifacts: []ArtifactRef{},
			})
		} else {
			refs = append(refs, ref)
		}
	}
	_ = c.emitEvent(eventType, "runtime", todoID, LifecycleEventPayload{
		Phase: string(c.phaseWorkflow.State()), Agent: task.Agent, Provider: providerName,
		Capability: capability, ActionID: actionID, ToolName: task.Action.Type,
		ActionStatus: status, FailureSignature: failureSignature, Artifacts: refs,
	})
}

func (c *Coordinator) writeRuntimeActionReceipt(receipt runtimeActionReceipt, actionID string) (ArtifactRef, error) {
	path, err := c.phaseWorkflow.executionContext().RuntimeWorkspace.Resolve(filepath.Join("receipts", actionID+".json"))
	if err != nil {
		return ArtifactRef{}, err
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return ArtifactRef{}, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return ArtifactRef{}, err
	}
	return ArtifactRef{
		ID: actionID, Kind: "receipt", Type: "runtime_action_receipt",
		Path:        filepath.ToSlash(filepath.Join("runtime", "receipts", actionID+".json")),
		Description: "structured ActionProvider execution receipt", RunID: receipt.RunID,
		TaskID: receipt.TaskID, Agent: receipt.Agent, CreatedAt: receipt.FinishedAt,
	}, nil
}

// materializeCheckpointedProtocolRepair fills the receipt gap between a
// successful submit_result checkpoint and the protocol repair's terminal
// status update. It preserves prior schema-repair history and records the
// checkpointed result as the successful attempt so a later restart has a
// complete forensic record without replaying a repair model turn.
func (c *Coordinator) materializeCheckpointedProtocolRepair(item *TodoItem, agentName string, result *TaskResult) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil || item == nil || result == nil {
		return fmt.Errorf("protocol repair receipt requires a task and submitted result")
	}

	receipt := ExecutionReceipt{
		RunID:      c.executionRunID,
		TaskID:     item.ID,
		Attempt:    1,
		ProducerID: agentName,
		StartedAt:  time.Now(),
		FinishedAt: time.Now(),
	}
	if item.ExecutionReceipt != nil {
		receipt = cloneExecutionReceipt(item.ExecutionReceipt)
	}
	if receipt.RunID == "" {
		receipt.RunID = c.executionRunID
		if receipt.RunID == "" {
			receipt.RunID = c.taskTracker.TodoList().RunID()
		}
	}
	if receipt.TaskID == "" {
		receipt.TaskID = item.ID
	}
	if receipt.Attempt < 1 {
		receipt.Attempt = 1
	}
	if receipt.ProducerID == "" {
		receipt.ProducerID = agentName
	}
	if receipt.StartedAt.IsZero() {
		receipt.StartedAt = time.Now()
	}
	receipt.FinishedAt = time.Now()

	var prior *RepairProvenance
	if receipt.RepairProvenance != nil {
		copyPrior := *receipt.RepairProvenance
		copyPrior.History = append([]RepairAttemptProvenance(nil), receipt.RepairProvenance.History...)
		prior = &copyPrior
	}
	provenance := &RepairProvenance{
		Attempted:       true,
		Success:         true,
		SubmittedResult: result,
	}
	if prior != nil {
		provenance.History = append(provenance.History, prior.History...)
		provenance.RepairAttempts = prior.RepairAttempts
		for _, attempt := range prior.History {
			if attempt.Attempt > provenance.RepairAttempts {
				provenance.RepairAttempts = attempt.Attempt
			}
		}
		if prior.Success && prior.SubmittedResult != nil && validateSubmittedTaskResult(prior.SubmittedResult) == nil {
			provenance.Prompt = prior.Prompt
		}
	}
	if provenance.RepairAttempts == 0 {
		provenance.RepairAttempts = 1
	} else if prior == nil || !prior.Success {
		provenance.RepairAttempts++
	}
	if prior == nil || !prior.Success {
		provenance.History = append(provenance.History, RepairAttemptProvenance{
			Attempt:         provenance.RepairAttempts,
			Success:         true,
			SubmittedResult: result,
		})
	}
	receipt.RepairProvenance = provenance
	return c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &receipt)
}

// resumeProtocolIncompleteTask repairs a checkpointed protocol failure using
// only the result submission tool. The original worker already ran before the
// checkpoint was written, so this path must never call executeTask's worker
// setup, reset the todo for retry, or expose the worker's configured tools.
// The persisted Output is the authoritative execution evidence supplied to
// the repair model.
func (c *Coordinator) resumeProtocolIncompleteTask(parentCtx context.Context, task TaskDef, item *TodoItem) (string, error) {
	if c == nil || item == nil {
		return "", fmt.Errorf("protocol repair requires a task checkpoint")
	}
	output := item.Output
	if strings.TrimSpace(output) == "" {
		err := errors.New("protocol-incomplete checkpoint has no worker output for result-only repair")
		detail := c.FailureDetail(err, FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, TaskBlocked, output)
		return "", err
	}

	// submit_result checkpoints TypedResult before the caller can persist the
	// repair receipt and terminal status. A crash in that interval leaves a
	// protocol_incomplete task with a valid result but no provenance. Treat
	// that result as the durable repair boundary: record the missing receipt and
	// finish locally, without clearing it or making another repair-agent call.
	if item.TypedResult != nil && item.TypedResult.Source == "submitted" && validateSubmittedTaskResult(item.TypedResult) == nil {
		agentName := strings.ToLower(strings.TrimSpace(item.Agent))
		if agentName == "" {
			agentName = strings.ToLower(strings.TrimSpace(task.Agent))
		}
		if agentName == "" {
			agentName = "worker"
		}
		resolvedModel := task.Model
		if resolvedModel == "" && c.session != nil {
			resolvedModel = c.session.Config.Generation.Model
		}
		if err := c.materializeCheckpointedProtocolRepair(item, agentName, item.TypedResult); err != nil {
			return "", err
		}
		c.storeSubmittedTaskResult(item.ID, item.TypedResult)
		return c.finishProtocolRepair(parentCtx, item, task, agentName, resolvedModel, item.TypedResult, output)
	}

	agentDef, _, err := c.AgentPool().ResolveAgentName(task.Agent)
	if err != nil {
		detail := c.FailureDetail(err, FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, TaskBlocked, output)
		return "", err
	}
	agentName := strings.ToLower(agentDef.Name)
	resolvedModel := c.resolveAgentModel(agentDef, task.Model)
	resultTool := &submitResultTool{coordinator: c, todoID: item.ID}

	// If a successful repair was checkpointed before the status transition,
	// finalize it locally. This avoids spending another repair turn and still
	// never replays worker execution.
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RepairProvenance != nil {
		provenance := item.ExecutionReceipt.RepairProvenance
		if provenance.Success && provenance.SubmittedResult != nil && validateSubmittedTaskResult(provenance.SubmittedResult) == nil {
			c.storeSubmittedTaskResult(item.ID, provenance.SubmittedResult)
			return c.finishProtocolRepair(parentCtx, item, task, agentName, resolvedModel, provenance.SubmittedResult, output)
		}
		if provenance.Attempted && provenance.RepairAttempts >= 2 {
			err := fmt.Errorf("protocol result-only repair exhausted after %d attempt(s)", provenance.RepairAttempts)
			detail := c.FailureDetail(err, FailureSourceError)
			c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, TaskBlocked, output)
			return "", err
		}
		if provenance.Attempted && provenance.FailureReason != RepairFailureInvalidSchema {
			err := fmt.Errorf("protocol result-only repair cannot be retried after %s", provenance.FailureReason)
			detail := c.FailureDetail(err, FailureSourceError)
			c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, TaskBlocked, output)
			return "", err
		}
	}

	var repairAgent fantasy.Agent
	if c.repairAgentOverride != nil {
		repairAgent = c.repairAgentOverride
	} else if c.providerManager != nil {
		repairAgent, err = c.createGatedAgent(parentCtx, c.providerManager.GetProvider(resolvedModel), agent.AgentConfig{
			Def:        agentDef,
			TeamConfig: &c.session.Config,
			WorkDir:    c.projectDir,
			MaxSteps:   1,
		}, []fantasy.AgentTool{resultTool})
	}
	if err != nil {
		c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("failed to create protocol repair agent: %v", err)).withTodoID(item.ID))
	}
	if repairAgent == nil {
		err = errors.New("protocol result-only repair agent unavailable")
		detail := c.FailureDetail(err, FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, TaskBlocked, output)
		return "", err
	}

	priorAttempts := 0
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RepairProvenance != nil {
		priorAttempts = item.ExecutionReceipt.RepairProvenance.RepairAttempts
	}
	repairCtx := context.WithValue(parentCtx, tools.AgentToolsAllowedKey, []string{"submit_result"})
	repairCtx = context.WithValue(repairCtx, llmUsageReceiptExpectedKey{}, false)
	repairCtx = context.WithValue(repairCtx, todoIDKey{}, item.ID)
	repairCtx = context.WithValue(repairCtx, modelKey{}, resolvedModel)
	repairCtx = context.WithValue(repairCtx, tools.AgentNameKey, agentName)
	repairCtx = context.WithValue(repairCtx, hooks.AgentNameKey, agentName)
	repairCtx = context.WithValue(repairCtx, hooks.TeamNameKey, c.session.Config.Name)
	repairCtx = context.WithValue(repairCtx, hooks.TaskDescKey, task.Goal)
	timing := &taskTiming{}
	timing.reset()
	c.report(c.newEvent("step").withAgent(agentName).withMessage("resuming protocol task through result-only repair").withTodoID(item.ID))
	repairPrompt := fmt.Sprintf("## Goal\n%s\n\n## Execution Output\n%s\n\n## Repair Instructions\nThe worker execution is already complete and produced the output above, but it did not submit a structured result. Call submit_result now using only those execution facts. Do NOT execute work, inspect files, or call any other tool.", task.Goal, output)
	var typedRes *TaskResult
	var repairReason RepairFailureReason
	var repairSuccess bool
	var runErr error
	var repairHistory []RepairAttemptProvenance
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RepairProvenance != nil {
		repairHistory = append(repairHistory, item.ExecutionReceipt.RepairProvenance.History...)
	}
	for attempt := priorAttempts + 1; attempt <= 2; attempt++ {
		c.clearSubmittedTaskResult(item.ID)
		if attempt > priorAttempts+1 {
			repairPrompt = fmt.Sprintf("## Goal\n%s\n\n## Schema-only repair\nThe previous result-only repair call did not match the submit_result schema. This is the final repair attempt. Call submit_result exactly once with corrected schema and preserve the execution facts below. Do NOT execute work, inspect files, call any other tool, or emit a prose final response. The call must include both required fields: `status` (one of `success`, `completed_with_gaps`, `partial`, `failed`, or `blocked`) and a non-empty `summary`; put any complete textual deliverable in `details`.\n\n## Execution Output\n%s", task.Goal, output)
		}
		_, steps, callErr := c.runAgentWithStatusAndHistory(repairCtx, repairAgent, agentName, repairPrompt, nil, timing, fantasy.StepCountIs(1))
		runErr = callErr
		typedRes = c.GetTaskResult(item.ID)
		repairReason, _ = classifyRepairFailure(steps, typedRes)
		repairSuccess = typedRes != nil && typedRes.Source == "submitted" && validateSubmittedTaskResult(typedRes) == nil
		repairHistory = append(repairHistory, RepairAttemptProvenance{
			Attempt:         attempt,
			Success:         repairSuccess,
			Prompt:          repairPrompt,
			SubmittedResult: typedRes,
			FailureReason:   repairReason,
		})
		if repairSuccess || repairReason != RepairFailureInvalidSchema {
			break
		}
	}

	receipt := ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, ProducerID: agentName, StartedAt: time.Now()}
	if item.ExecutionReceipt != nil {
		receipt = cloneExecutionReceipt(item.ExecutionReceipt)
	}
	if receipt.RunID == "" {
		receipt.RunID = c.executionRunID
		if receipt.RunID == "" {
			receipt.RunID = c.taskTracker.TodoList().RunID()
		}
	}
	if receipt.TaskID == "" {
		receipt.TaskID = item.ID
	}
	if receipt.Attempt < 1 {
		receipt.Attempt = 1
	}
	receipt.ProducerID = agentName
	receipt.FinishedAt = time.Now()
	repairAttempts := priorAttempts
	for _, attempt := range repairHistory {
		if attempt.Attempt > repairAttempts {
			repairAttempts = attempt.Attempt
		}
	}
	receipt.RepairProvenance = &RepairProvenance{
		Attempted:       true,
		Success:         repairSuccess,
		Prompt:          repairHistory[len(repairHistory)-1].Prompt,
		SubmittedResult: typedRes,
		RepairAttempts:  repairAttempts,
		FailureReason:   repairReason,
		History:         repairHistory,
	}
	if runErr != nil && !repairSuccess {
		receipt.RepairProvenance.Error = runErr.Error()
	}
	_ = c.taskTracker.TodoList().SetExecutionReceipt(item.ID, &receipt)

	if repairSuccess {
		return c.finishProtocolRepair(parentCtx, item, task, agentName, resolvedModel, typedRes, output)
	}
	if repairReason == "" {
		repairReason = RepairFailureNoToolCall
	}
	err = fmt.Errorf("protocol result-only repair failed for task %s (%s): %s", item.ID, agentName, repairReason)
	detail := c.FailureDetail(err, FailureSourceError)
	c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, TaskBlocked, output)
	return "", err
}

func (c *Coordinator) finishProtocolRepair(ctx context.Context, item *TodoItem, task TaskDef, agentName, resolvedModel string, result *TaskResult, output string) (string, error) {
	if err := c.terminalTaskFailure(ctx, item.ID); err != nil {
		detail := c.FailureDetail(err, FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, FailureVerify, TaskBlocked, output)
		return "", err
	}
	if task.Verify != "" || task.VerifySpec != nil {
		if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskVerifying, "running objective verification", output, nil); err != nil {
			return "", err
		}
		verification, err := c.verifyTaskDeliverableWithSpec(ctx, nil, task)
		if verification != nil {
			_ = c.taskTracker.TodoList().SetVerificationResult(item.ID, verification)
		}
		if err != nil {
			detail := c.FailureDetail(fmt.Errorf("deliverable verification failed after protocol repair: %w", err), FailureSourceError)
			c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, FailureVerify, TaskBlocked, output)
			return "", err
		}
	}
	summary := strings.TrimSpace(result.Summary)
	if summary == "" {
		summary = "protocol result-only repair succeeded"
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskDone, summary, output, nil); err != nil {
		return "", err
	}
	c.recordTerminalTypedTaskResult(item.ID)
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(agentName).withOutput(output).withMessage("protocol result-only repair completed").withModel(resolvedModel).withTodoID(item.ID))
	return output, nil
}

// rescueFinalSummary gives an agent that stopped without a final message one
// tool-free turn (its full step history attached) to summarize what it did.
// Returns "" when the rescue itself fails or produces nothing.
func (c *Coordinator) rescueFinalSummary(ctx context.Context, ag fantasy.Agent, agentName string, steps []fantasy.StepResult, timing *taskTiming) string {
	if ctx.Err() != nil {
		return ""
	}
	// The rescue stream has no separate execution receipt; account its usage
	// directly in the no-progress budget.
	ctx = context.WithValue(ctx, llmUsageReceiptExpectedKey{}, false)
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
	// Defence in depth for the plan-time gate in ExecuteTasks: a task that
	// reaches the tool-less sidecar path while needing tools must fail here
	// rather than have its prose recorded as a completed change.
	if err := c.validateSidecarTaskContract(task); err != nil {
		c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, 0, ExecutionUsage{})
		c.PersistFailureWithClassAndStatus(task.Agent, taskDesc, todoID, c.FailureDetail(err, FailureSourceError), NeedsHuman, FailureContract, TaskBlocked)
		return "", err
	}
	attemptStarted := time.Now()
	c.recordExecutionEvent(todoID, task.Agent, 1, "in_progress", c.sidecarModel, 0, ExecutionUsage{})

	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskInProgress, "", "", nil); err != nil {
		return "", fmt.Errorf("mark sidecar task started: %w", err)
	}
	c.reconcileTaskStatusProjection()
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ID == todoID {
			c.reEvaluateAffectedCriteria(ctx, item)
			break
		}
	}
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
		c.PersistFailureWithClass(task.Agent, taskDesc, todoID, c.FailureDetail(err, FailureSourceError), RetryNone, FailureExecution)
		return "", fmt.Errorf("sidecar execution failed (model: %s): %w", c.sidecarModel, err)
	}
	if verr := validateTaskOutput(task, result); verr != nil {
		c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
		c.PersistFailureWithClass(task.Agent, taskDesc, todoID, c.FailureDetail(verr, FailureSourceError), RetryNone, FailureProtocol)
		return "", fmt.Errorf("task completion validation failed: %w", verr)
	}
	if task.Verify != "" || task.VerifySpec != nil {
		if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskVerifying, "running objective verification", "", nil); err != nil {
			c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
			return "", err
		}
		vMsg := task.Verify
		if vMsg == "" && task.VerifySpec != nil {
			vMsg = string(task.VerifySpec.Type)
		}
		c.report(c.newEvent("verify_start").withAgent(task.Agent).withMessage(vMsg).withTodoID(todoID))
		verification, verifyErr := c.verifyTaskDeliverableWithSpec(ctx, nil, task)
		if verification != nil {
			c.noteObjectiveVerifierResult(todoID, verifyErr == nil && isVerifySuccess(verification))
			_ = c.taskTracker.TodoList().SetVerificationResult(todoID, verification)
		}
		if verifyErr != nil {
			c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
			c.PersistFailureWithClass(task.Agent, taskDesc, todoID, c.FailureDetail(verifyErr, FailureSourceError), RetryNone, FailureVerify)
			c.report(c.newEvent("verify_error").withAgent(task.Agent).withMessage(verifyErr.Error()).withTodoID(todoID))
			return "", fmt.Errorf("deliverable verification failed: %w", verifyErr)
		}
		c.report(c.newEvent("verify_done").withAgent(task.Agent).withMessage("objective verification passed").withTodoID(todoID))
	}

	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskDone, utils.TruncateRunes(result, summaryMaxRunes), result, nil); err != nil {
		c.recordExecutionEvent(todoID, task.Agent, 1, "error", c.sidecarModel, time.Since(attemptStarted), ExecutionUsage{})
		return "", err
	}
	c.recordTerminalTypedTaskResult(todoID)
	c.reconcileTaskStatusProjection()
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ID == todoID {
			c.reEvaluateAffectedCriteria(ctx, item)
			break
		}
	}
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

// toolCallEvidence captures the most recent tool call's name, input, and
// result within a single executeTask attempt. It is per-attempt (not
// coordinator-global) to prevent cross-task leakage in the DAG scheduler's
// concurrent per-task goroutines. Tool input and result are redacted via
// utils.RedactSecrets before storage to prevent credential exposure in
// retry prompts (§6.1, §9).
type toolCallEvidence struct {
	toolName   string
	toolInput  string // redacted, bounded to 500 runes
	resultText string // redacted, bounded to 500 runes
	resultErr  bool
}

// toolCallEvidenceKey is a context key for per-attempt tool call evidence.
type toolCallEvidenceKey struct{}

// agentToolNames returns the concrete names in the exact tool slice handed to
// a model. It deliberately does not call SelectTools or consult frontmatter:
// callers must pass the completed invocation slice, after MCP and protocol
// tools have been appended, so authorization cannot drift from exposure.
func agentToolNames(agentTools []fantasy.AgentTool) []string {
	seen := make(map[string]bool, len(agentTools))
	names := make([]string, 0, len(agentTools))
	for _, tool := range agentTools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.Info().Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

// withEffectiveToolsAllowed attaches the tools permitted for one worker
// invocation. exposedToolNames must come from the final []fantasy.AgentTool
// supplied to that invocation, not a second SelectTools/MCP prediction. The
// agent-specific canonical MCP alias remains an authorizer concern and is
// included in addition to the concrete model-visible name.
func (c *Coordinator) withEffectiveToolsAllowed(ctx context.Context, def *agent.AgentDef, exposedToolNames []string) context.Context {
	return c.withEffectiveToolsAllowedForTask(ctx, def, exposedToolNames, TaskDef{})
}

// withEffectiveToolsAllowedForTask keeps team-level denials in force while
// admitting the exact grants of a runtime-selected static contract. This must
// mirror selectWorkerToolsForTask: exposing a granted tool without allowing it
// here makes the worker's first permitted call fail at the runtime gate.
func (c *Coordinator) withEffectiveToolsAllowedForTask(ctx context.Context, def *agent.AgentDef, exposedToolNames []string, task TaskDef) context.Context {
	if c == nil || c.session == nil {
		return ctx
	}

	declared := append([]string(nil), c.session.Config.ToolsAllowed...)
	if def != nil {
		for _, name := range strings.Split(def.Tools, ",") {
			if name = strings.TrimSpace(name); name != "" {
				declared = append(declared, name)
			}
		}
		// Agent-specific MCP tools are a supported frontmatter grant. Keep
		// the display/tool-call name for the stream gate and add the canonical
		// agent:tool name for the transport authorizer.
		mcpAllowed := c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || c.phaseWorkflow.State() == PhaseExecute
		if mcpAllowed {
			for name := range def.MCPTools {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				declared = append(declared, name, strings.ToLower(strings.TrimSpace(def.Name))+":"+name)
			}
		} else if c.mcpManager != nil {
			mcpNames := make(map[string]bool)
			for _, t := range c.mcpManager.AsAgentTools() {
				mcpNames[t.Info().Name] = true
			}
			for name := range def.MCPTools {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				mcpNames[name] = true
				mcpNames[strings.ToLower(strings.TrimSpace(def.Name))+":"+name] = true
			}
			var filtered []string
			for _, name := range declared {
				if !mcpNames[name] {
					filtered = append(filtered, name)
				}
			}
			declared = filtered
		}
	}
	// Nothing was granted explicitly anywhere. Leave the policy unset: the
	// stream gate only engages once an allowlist is attached, so attaching one
	// here — even a protocol-tools-only one — would flip an unconstrained agent
	// into a deny-all agent.
	if len(declared) == 0 {
		return ctx
	}
	// An allowlist is in force, so it has to cover the final model tool slice.
	allowed := c.filterDeniedToolNamesWithGrants(append(declared, exposedToolNames...), templateGrantedToolNames(def, task))
	return context.WithValue(ctx, tools.AgentToolsAllowedKey, dedupeToolNames(allowed))
}

// stepBudget resolves the per-attempt step budget for def, honouring the agent's
// max-steps and then the team's (which is what --max-steps writes into), and
// falling back to the caller's role-specific default.
//
// Call sites used to pass a non-zero AgentConfig.MaxSteps directly. CreateAgent
// prefers that field over resolveMaxSteps, so every team-level and --max-steps
// override was silently discarded and every agent ran on the compiled-in
// default. Resolving here keeps the same defaults while making the overrides
// take effect.
func (c *Coordinator) stepBudget(def *agent.AgentDef, fallback int) int {
	if def != nil && def.MaxSteps > 0 {
		return def.MaxSteps
	}
	if c != nil && c.session != nil && c.session.Config.MaxSteps > 0 {
		return c.session.Config.MaxSteps
	}
	return fallback
}

// dedupeToolNames collapses duplicates introduced by unioning several grant
// sources, keeping first-seen order so allowlist logs stay readable.
func dedupeToolNames(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// authorizeStreamTool routes namespaced MCP tools through the same policy
// boundary as built-in tools. MCPToolManager uses server__tool names to keep
// names distinct; the policy contract records the canonical server:tool key.
func (c *Coordinator) authorizeStreamTool(ctx context.Context, agentName, toolName string, allowed map[string]bool) (PolicyDecision, error) {
	canonicalAgent := strings.ToLower(strings.TrimSpace(agentName))
	if allowed[canonicalAgent+":"+toolName] {
		return c.AuthorizeMCPCall(ctx, MCPAuthorizationRequest{
			Agent:        agentName,
			Server:       canonicalAgent,
			Tool:         toolName,
			AllowedTools: allowed,
			FailureMode:  c.ExecutionProfile().PolicyFailureMode,
		})
	}
	if server, tool, ok := strings.Cut(toolName, "__"); ok && server != "" && tool != "" {
		canonical := server + ":" + tool
		if allowed[toolName] || allowed[canonical] {
			allowed[canonical] = true
		}
		return c.AuthorizeMCPCall(ctx, MCPAuthorizationRequest{
			Agent:        agentName,
			Server:       server,
			Tool:         tool,
			AllowedTools: allowed,
			FailureMode:  c.ExecutionProfile().PolicyFailureMode,
		})
	}
	return c.AuthorizeToolCall(ctx, ToolAuthorizationRequest{
		Agent:        agentName,
		Tool:         toolName,
		AllowedTools: allowed,
		FailureMode:  c.ExecutionProfile().PolicyFailureMode,
	})
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
	ctx = mcp.WithToolAuthorizer(ctx, func(callCtx context.Context, server, tool, _ string) error {
		allowed := make(map[string]bool)
		for _, name := range tools.GetToolsAllowed(callCtx) {
			allowed[strings.TrimSpace(name)] = true
		}
		policyServer := server
		if strings.EqualFold(server, agentName) {
			policyServer = strings.ToLower(strings.TrimSpace(agentName))
		}
		canonical := policyServer + ":" + tool
		if allowed[server+"__"+tool] || allowed[canonical] || allowed[tool] || allowed[strings.ToLower(server)+":"+tool] {
			allowed[canonical] = true
		}
		decision, err := c.AuthorizeMCPCall(callCtx, MCPAuthorizationRequest{
			Agent:        agentName,
			Server:       policyServer,
			Tool:         tool,
			AllowedTools: allowed,
			FailureMode:  c.ExecutionProfile().PolicyFailureMode,
		})
		if err != nil {
			return err
		}
		if decision.Code != DecisionAllow {
			return fmt.Errorf("MCP authorization denied for %s: %s", canonical, decision.Reason)
		}
		return nil
	})
	reportFn := c.reportStatus
	workspace := c.session.Workspace
	teamName := c.session.Config.Name
	logWrite := func(entry string) { writeLLMLog(workspace, teamName, agentName, entry) }

	// Pick up the TodoItem ID injected by executeTask so events can be attributed to a task.
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	attemptTokens, _ := ctx.Value(attemptBudgetKey{}).(*attemptBudget)

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
	pendingToolStarted := make(map[string]time.Time)
	tp := &ThinkParser{}

	streamCall := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		StopWhen: extraStop,
		PrepareStep: func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			// Keep this request within the context model token budget.
			modelID := ""
			if opts.Model != nil {
				modelID = opts.Model.Model()
			}
			spec := globalRegistry.GetSpec(modelID).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(c.agentDefByName(agentName)))
			budget := c.ContextCompiler().CalculateBudget(spec, 0, 0)
			preparedMessages := opts.Messages
			messagesCapped := false
			if capped := CapStepMessagesWithCounter(ctx, defaultCounter, modelID, opts.Messages, budget.Available); capped != nil {
				opts.Messages = capped
				preparedMessages = capped
				messagesCapped = true
			}
			if attemptTokens != nil {
				if err := attemptTokens.reserveContext(estimateStepRequestTokens(preparedMessages, prompt)); err != nil {
					return ctx, fantasy.PrepareStepResult{}, err
				}
			}
			// Log only requests that cleared the circuit breaker; a rejected
			// request was never sent to the provider and must not look like an
			// attempted model call in the task evidence.
			llmLogMu.Lock()
			loggedMsgs, lastReqBytes = llmLogRequest(logWrite, opts, preparedMessages, loggedMsgs)
			llmLogMu.Unlock()
			if messagesCapped {
				return ctx, fantasy.PrepareStepResult{Messages: preparedMessages}, nil
			}
			return ctx, fantasy.PrepareStepResult{}, nil
		},
		OnStepStart: func(stepNumber int) error {
			reportFn(c.newEvent("step").withAgent(agentName).withTodoID(todoID).withStep(stepNumber).withMessage(fmt.Sprintf("step %d", stepNumber)))
			c.SetCurrentStep(stepNumber)
			return nil
		},
		OnToolCall: func(tc fantasy.ToolCallContent) error {
			timing.beginTool()
			// Authorization is enforced in policyGatedTool.Run, not here. An
			// error returned from this callback aborts the whole model round, so
			// deciding policy here made every denial destroy the attempt — and
			// it did so before the call below could record the attempt as
			// evidence. See internal/team/tool_policy_gate.go.
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

			if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
				phase := c.phaseWorkflow.State()
				eventType := "tool_observation_started"
				if phase == PhaseExecute {
					eventType = "action_started"
				}
				_ = c.emitEvent(eventType, agentName, todoID, LifecycleEventPayload{
					Phase:    string(phase),
					Agent:    agentName,
					ActionID: tc.ToolCallID,
					ToolName: tc.ToolName,
				})
			}
			c.SetCurrentStage("tool")
			c.SetCurrentTool(tc.ToolName)
			c.taskTracker.TodoList().SetLastOperation(todoID, tc.ToolName)
			// Record the actual tool call input (redacted, bounded) for
			// retry context (§6.1). Stored in per-attempt evidence via
			// context to avoid cross-task leakage in concurrent dispatch.
			if ev, _ := ctx.Value(toolCallEvidenceKey{}).(*toolCallEvidence); ev != nil {
				ev.toolName = tc.ToolName
				ev.toolInput = utils.TruncateRunes(utils.RedactSecrets(tc.Input), 500)
			}

			// 🔁 Deadloop / thrashing detection!
			loopDetectMu.Lock()
			pendingToolInputs[tc.ToolCallID] = tc.Input
			pendingToolStarted[tc.ToolCallID] = time.Now().UTC()
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
			// Record the tool result for retry context (§6.1), redacted
			// and stored in per-attempt evidence via context.
			if ev, _ := ctx.Value(toolCallEvidenceKey{}).(*toolCallEvidence); ev != nil {
				ev.resultText = utils.TruncateRunes(utils.RedactSecrets(resultPreview), 500)
				ev.resultErr = isErrResult
			}

			// 🔁 Track error count for the exact call (tool + input) the
			// detector is watching; results of other in-flight calls of the
			// same tool must not inflate the counter.
			loopDetectMu.Lock()
			callInput, tracked := pendingToolInputs[tr.ToolCallID]
			callStarted := pendingToolStarted[tr.ToolCallID]
			delete(pendingToolInputs, tr.ToolCallID)
			delete(pendingToolStarted, tr.ToolCallID)
			if tracked && lastToolCall != nil && lastToolCall.toolName == tr.ToolName && lastToolCall.input == callInput {
				if isErrResult {
					consecutiveErrCount++
				} else {
					consecutiveErrCount = 0
				}
			}
			loopDetectMu.Unlock()
			attempt, _ := ctx.Value(executionAttemptKey{}).(int)
			if receiptErr := c.recordActualToolReceipt(todoID, attempt, tr.ToolCallID, tr.ToolName, callInput, resultPreview, isErrResult, callStarted); receiptErr != nil {
				return fmt.Errorf("record tool execution receipt: %w", receiptErr)
			}
			if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
				phase := c.phaseWorkflow.State()
				eventType := "tool_observation_completed"
				status := "success"
				if isErrResult {
					eventType = "tool_observation_failed"
					status = "failure"
				}
				if phase == PhaseExecute {
					if isErrResult {
						eventType = "action_failed"
					} else {
						eventType = "action_completed"
					}
				}
				_ = c.emitEvent(eventType, agentName, todoID, LifecycleEventPayload{
					Phase:        string(phase),
					Agent:        agentName,
					ActionID:     tr.ToolCallID,
					ToolName:     tr.ToolName,
					ActionStatus: status,
					Artifacts: []ArtifactRef{
						{ID: tr.ToolCallID, Kind: "transcript"},
					},
				})
			}
			// A worker can use a tool error as evidence and still produce a
			// typed result for its bounded task. The coordinator has no such
			// result contract: continuing after an unavailable tool, invalid
			// arguments, or a failed direct tool call lets it make decisions
			// from incomplete evidence. Stop this orchestrator turn after the
			// failing receipt has been persisted instead.
			if todoID == CoordTodoID && isErrResult {
				trimmedResult := strings.TrimSpace(resultPreview)
				if strings.HasPrefix(trimmedResult, "Tool argument schema violation:") ||
					c.isInitialToolCorrectionResult(tr.ToolName, trimmedResult) {
					// Allow protocol repair prompt to reach the model; it is
					// explicitly formulated as an error result block so the LLM
					// knows it must retry. Initial-tool correction is likewise a
					// bounded, runtime-issued protocol response; arbitrary
					// coordinator tool errors remain terminal.
				} else {
					return fmt.Errorf("%w: tool %q failed: %s", errCoordinatorToolFailure, tr.ToolName, trimmedResult)
				}
			}

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
			if attemptTokens != nil {
				// Only generated output is charged here. Input tokens are the
				// resent conversation, which reserveContext already accounted
				// for as growth; charging them again is what turned this guard
				// into a step ceiling.
				if err := attemptTokens.chargeOutput(usage.OutputTokens); err != nil {
					return err
				}
			}

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
		accountedTokens := c.addStepTokens(result.Steps)
		// Coordinator and auxiliary streams do not emit worker execution
		// receipts, so add their usage directly to the no-progress budget.
		// Worker/direct-agent streams are accounted by the cumulative receipt
		// path. The context marker, rather than todo ID shape, is authoritative
		// because repairs, rescues, sub-agents, and plan reviewers can share IDs.
		if llmUsageNeedsDirectNoProgressAccounting(ctx) {
			c.recordNoProgressTokens(accountedTokens)
		}
	}
	if err != nil {
		// Preserve any partial output and steps even when the agent errored,
		// so the retry prompt can include evidence of what was attempted
		// (§6.1: retry prompt must include class, evidence, last command/exit).
		// Without this, a worker that did useful work before failing leaves
		// no evidence, and computeEvidenceComplete returns false, blocking retry.
		output := ""
		var steps []fantasy.StepResult
		if result != nil {
			output = result.Response.Content.Text()
			steps = result.Steps
		}
		return output, steps, err
	}
	return result.Response.Content.Text(), result.Steps, nil
}

// estimateStepRequestTokens derives a conservative request charge when a
// provider does not report usage. It counts all message content sent on this
// step (including prior tool output); fantasy normally puts the user prompt in
// those messages, but the fallback makes custom Agent implementations safe.
func estimateStepRequestTokens(messages []fantasy.Message, prompt string) int64 {
	chars := 0
	for _, message := range messages {
		chars += messageTextSize(message)
	}
	if chars == 0 {
		chars = len(prompt)
	}
	if chars <= 0 {
		return 1
	}
	return int64((chars + 3) / 4)
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

func (c *Coordinator) verifyTaskDeliverableWithSpec(parentCtx context.Context, agentDef *agent.AgentDef, task TaskDef) (*VerificationResult, error) {
	spec := task.VerifySpec
	if spec == nil && task.Verify != "" {
		spec = &agent.VerificationSpec{
			Mode:    task.VerifyMode,
			Command: task.Verify,
		}
	}
	if spec == nil {
		return nil, nil
	}
	// Mixed legacy/typed task definitions are valid during migration. Preserve
	// their legacy command and mode when the typed object omits either value,
	// rather than silently defaulting to a different assertion mode.
	normalizedSpec := NormalizeVerificationSpec(*spec, task.Verify, task.VerifyMode)
	shell := "sh"
	if agentDef != nil && agentDef.Shell != "" {
		shell = agentDef.Shell
	} else if c != nil && c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}
	workDir := c.verificationWorkDir()
	timeout := c.verifyTaskTimeout()
	verifyCtx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()
	return ExecuteVerificationSpec(verifyCtx, shell, workDir, normalizedSpec)
}

// verifyTaskDeliverable runs the task's optional verify command and returns a
// non-nil error if the command exits non-zero (or cannot be run). This provides
// an objective, non-LLM check that the deliverable actually exists/works before
// a task is accepted as done. The command runs in the project directory using
// the team's (or agent's) configured shell, falling back to "sh".
func (c *Coordinator) verifyTaskDeliverable(parentCtx context.Context, agentDef *agent.AgentDef, command string) (*VerificationResult, error) {
	return c.verifyTaskDeliverableWithMode(parentCtx, agentDef, command, "success")
}

func (c *Coordinator) verifyTaskDeliverableWithMode(ctx context.Context, agentDef *agent.AgentDef, command, mode string) (*VerificationResult, error) {
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
	workDir := c.verificationWorkDir()
	spec := VerificationSpec{
		Type:    VerifyCommandExit,
		Mode:    mode,
		Command: command,
	}
	timeout := c.verifyTaskTimeout()
	verifyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return ExecuteVerificationSpec(verifyCtx, shell, workDir, spec)
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

// toolNameSet indexes tool names for membership tests.
func toolNameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	return set
}

// sharedKnowledgeInstructions describes how to share findings with later agents,
// naming stm_write only when the worker actually holds that grant. Without the
// grant the worker gets the same guidance pointed at a tool it can use, instead
// of an instruction that aborts its attempt.
func (c *Coordinator) sharedKnowledgeInstructions(granted map[string]bool) string {
	if c == nil || c.session == nil {
		return ""
	}
	stmPath := STMPath(c.session.Workspace)
	b := &strings.Builder{}
	fmt.Fprintf(b, "\n- Key knowledge from previous agents is provided below. You do NOT need to read `%s` at the start. Only read it later if you need to check for *new* updates from concurrent agents.\n", stmPath)
	if c.contextRepo != nil {
		// Canonical mode: shared memory is captured from structured results.
		// Never instruct workers to edit the projection file directly; a direct
		// stm.md write would be discarded on the next projection rebuild.
		b.WriteString("- Return important findings, decisions, questions, artifacts, and verification in your structured result. The runtime captures shared memory automatically.\n")
		return b.String()
	}
	switch {
	case granted["stm_write"]:
		b.WriteString("- Return important findings, decisions, questions, artifacts, and verification in your structured result. The runtime captures shared memory automatically; `stm_write` is a deprecated typed compatibility tool.\n")
	case granted["write"] || granted["edit"]:
		fmt.Fprintf(b, "- When you discover something important (API shape, file location, decision, error), append it to `%s` immediately — do not wait until the end.\n", stmPath)
	default:
		b.WriteString("- Report anything important you discover (API shape, file location, decision, error) in your structured result.\n")
	}
	return b.String()
}

// resultProtocolInstructions states the result contract the runner enforces.
// ExecuteTasks sets RequiresResult on every non-sidecar task, so a worker that
// finishes with prose alone fails the task — and that failure is
// indistinguishable from genuine non-completion. Stating the contract is the
// only thing that makes the enforcement fair.
func resultProtocolInstructions(task TaskDef, granted map[string]bool) string {
	if !task.Execution.RequiresResult || !granted["submit_result"] {
		return ""
	}
	b := &strings.Builder{}
	b.WriteString("\n\n## Result Protocol\n\n")
	if task.ContractID != "" && task.ContractHash != "" {
		fmt.Fprintf(b, "Effective contract: `%s` (revision %d, sha256 %s). This immutable runtime contract is authoritative over any conflicting prose.\n\n", task.ContractID, task.ContractRevision, task.ContractHash)
	}
	b.WriteString("This task is not complete until you call `submit_result`. A prose summary is NOT a result: if you end your turn without calling `submit_result`, the task is recorded as failed no matter how much work you finished.\n\n")
	b.WriteString("- Call `submit_result` exactly once, as the last thing you do.\n")
	b.WriteString("- Set `status` truthfully: use `success` when the goal is fully met without a known target limitation; use `completed_with_gaps` when the assigned work is complete but it discovered a target limitation, and record that limitation in findings/risks/open questions; otherwise use `partial`, `failed`, or `blocked`. A truthful `partial` is far more useful than an optimistic completion claim.\n")
	b.WriteString("- Put the facts a later agent needs into `summary`, `details`, `findings`, and `decisions`. Use `details` for the complete textual plan, analysis, review, or handoff; do not create a report file merely to make that content available.\n")
	b.WriteString("- `open_questions` accepts strings, or structured objects with required string `question` and optional string `context` and `detail`; do not send arbitrary object shapes.\n")
	b.WriteString("- Reserve your final model step for `submit_result`. Once you have enough evidence, stop writing prose or making new tool calls and submit the result; if you are running out of steps, submit what you have.\n")
	if len(task.Execution.ToolSequence) > 0 {
		b.WriteString("- This is a closed tool sequence. The runtime permits only this order: `")
		b.WriteString(strings.Join(task.Execution.ToolSequence, " → "))
		b.WriteString("`. Do not call a tool outside that sequence or after `submit_result`.\n")
		b.WriteString("- Each listed position is exactly one tool call. After a tool succeeds, advance to the next listed position; never repeat, revise, or repair that slot with another call. If the result makes it impossible to continue, stop immediately with one early `submit_result` using status `failed` or `blocked`; do not consume a later slot to repair it.\n")
		b.WriteString("- The task goal and constraints are descriptive requirements, not extra protocol slots. A phrase such as discover, confirm, inspect, or check does not authorize an additional tool call unless that tool appears at the current sequence position. If a required value is unavailable from the task context or an already captured result, submit an early truthful `blocked` result; never add a discovery/probe call.\n")
		b.WriteString("- The first assistant action after receiving this task MUST be the first listed tool call. Do not plan aloud, inspect files, read source, ask for help, or make any exploratory/preparatory tool call before it.\n")
		b.WriteString("- If the first listed tool cannot be called exactly as specified, call `submit_result` with status `failed` or `blocked`; never probe another tool or repair the sequence.\n")
		for index, codes := range task.Execution.ToolExpectedExitCodes {
			if len(codes) == 0 || index >= len(task.Execution.ToolSequence) {
				continue
			}
			fmt.Fprintf(b, "- At sequence position %d (`%s`), exit code(s) %s are expected observations, not failures. Preserve their output and continue to the next sequence slot.\n", index+1, task.Execution.ToolSequence[index], formatExpectedExitCodes(codes))
		}
	}
	return b.String()
}

func formatExpectedExitCodes(codes []int) string {
	values := make([]string, len(codes))
	for index, code := range codes {
		values[index] = fmt.Sprintf("%d", code)
	}
	return strings.Join(values, ", ")
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
	lastErr = redactRetryText(lastErr, 500)
	s := c.AgentPool().Sidecar()
	if s != nil {
		// Use a shorter timeout for reflection to avoid holding up retries
		reflectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		prompt := buildFailureReflectionPrompt(agentName, goal, lastErr)
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

// redactRetryText is the final safety boundary before untrusted execution
// evidence is sent to a worker or reflection sidecar. Redact before bounding
// so a credential cannot be split by truncation and evade the matcher.
func redactRetryText(value string, maxRunes int) string {
	return utils.TruncateRunes(utils.RedactSecrets(value), maxRunes)
}

func retryPartialOutput(output, previousOutput string) string {
	if strings.TrimSpace(output) != "" {
		return redactRetryText(output, 300)
	}
	if strings.TrimSpace(previousOutput) != "" {
		return redactRetryText(previousOutput, 300)
	}
	return ""
}

func buildFailureReflectionPrompt(agentName, goal, lastErr string) string {
	return fmt.Sprintf("Agent %q failed to achieve goal: %q\nError: %s\n\nAnalyze the error and provide a concise hint (max 100 words) for the next attempt. Focus on what to change or avoid.",
		agentName, goal, redactRetryText(lastErr, 500))
}

var errWrongVerificationPolarity = errors.New("verification wrong polarity")

// isUnfixableVerifyFailure reports whether err comes from the structured "wrong
// polarity" verify-command detection in verifyTaskDeliverable (a
// grep/grep-c-based cleanup check that asserts a resource EXISTS instead of
// asserting it's GONE). The task.Verify command is fixed by the coordinator
// at task-assignment time — the worker executing the task has no way to
// edit its own verify field — so this failure is guaranteed to recur
// identically on every retry regardless of what the worker does.
func isUnfixableVerifyFailure(err error) bool {
	return errors.Is(err, errWrongVerificationPolarity)
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
	case strings.Contains(e, "step budget exhausted"):
		// Truncation is not a wrong approach. The prior attempt's conversation
		// is carried into this one, so the correct instruction is to resume —
		// telling it to change approach makes it redo finished work.
		return "You ran out of steps last time; you were cut off, not wrong. Your earlier work is in the conversation above — continue from where you stopped rather than redoing it. Skip re-exploration, go straight to the remaining actions, and leave a step to call submit_result."
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

func (c *Coordinator) createTaskAgentWithResultTool(ctx context.Context, def *agent.AgentDef, overrideModel string, resultTool *submitResultTool, task TaskDef) (fantasy.Agent, []string, error) {
	resolvedTask := task
	if overrideModel != "" {
		resolvedTask.Model = overrideModel
	}
	modelID, err := c.ModelRuntime().ResolveTaskModel(def, resolvedTask)
	if err != nil {
		return nil, nil, err
	}
	agentDef := def
	if overrideModel != "" {
		overriddenDef := *def
		overriddenDef.Generation.Model = overrideModel
		agentDef = &overriddenDef
	}

	agentDef = c.injectWorkerContext(ctx, agentDef)
	ctx = tools.SetSSHSessionManager(ctx, c.sshSessionMgr)

	extras := []fantasy.AgentTool(nil)
	if resultTool != nil {
		extras = append(extras, resultTool)
	}
	resolvedTools, err := c.ToolResolver().ResolveTaskTools(ctx, agentDef, task, extras)
	if err != nil {
		return nil, nil, err
	}
	provider, err := c.ModelRuntime().ProviderFor(modelID)
	if err != nil {
		return nil, nil, err
	}
	ag, err := c.createGatedAgent(ctx, provider, agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   c.stepBudget(agentDef, agent.DefaultMaxSteps),
	}, resolvedTools.Tools)
	if err != nil {
		return nil, nil, err
	}
	return ag, resolvedTools.Names, nil
}

// missingExecutionTools validates a closed sequence against the concrete
// worker tool set before the model is started. Without this preflight a
// coordinator can assign a bash/write sequence to a read-only helper; the
// worker then sees only submit_result, reports an environment block, and the
// real contract mistake is obscured as a task failure.
func missingExecutionTools(agentTools []fantasy.AgentTool, sequence []string) []string {
	if len(sequence) == 0 {
		return nil
	}
	available := make(map[string]bool, len(agentTools)+1)
	for _, tool := range agentTools {
		if tool != nil {
			available[strings.TrimSpace(tool.Info().Name)] = true
		}
	}
	// submit_result is appended by the caller after this check.
	available["submit_result"] = true
	seen := make(map[string]bool)
	var missing []string
	for _, name := range sequence {
		name = strings.TrimSpace(name)
		if name == "" || available[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	return missing
}

// computeEvidenceComplete determines whether the current attempt captured
// sufficient execution evidence for a meaningful retry (§6.1: retry prompt
// must include class, evidence, last command/exit). Evidence is considered
// complete when:
//   - if a transcript was required (verbatim/requires_result), the transcript
//     manifest was successfully created (transcriptRef != "");
//   - for all tasks (including non-transcript), the agent produced at least
//     some tool-call steps or output — a bare error with no steps and no
//     output leaves no evidence of what was attempted.
//
// When evidence is incomplete, DecideRecovery downgrades RetryWorker to
// ReplanRequired because the retry prompt cannot include the required
// evidence context.
func computeEvidenceComplete(task TaskDef, transcriptRef string, steps []fantasy.StepResult, output string) bool {
	// If a transcript was required but not captured, evidence is incomplete.
	if (taskUsesVerbatimTranscript(task) || task.Execution.RequiresResult) && transcriptRef == "" {
		return false
	}
	// When a transcript was captured, it contains the full conversation
	// history (tool calls, results, agent output) as evidence.
	if transcriptRef != "" {
		return true
	}
	// For non-transcript tasks, evidence is complete only when the agent
	// produced substantive evidence: non-empty output or steps with
	// actual message content (tool calls, tool results, or assistant
	// responses). An empty StepResult{} with no Messages does not
	// constitute evidence of what was attempted.
	if strings.TrimSpace(output) != "" {
		return true
	}
	for _, step := range steps {
		if len(step.Messages) > 0 {
			return true
		}
	}
	return false
}

// buildRetryContext constructs the structured retry context required by §6.1.
// The context includes:
//  1. Failure class (contract, environment, execution, verification, etc.)
//  2. Evidence reference (opaque transcript artifact ID) and summary (last output)
//  3. Previous command and exit/verification result — always rendered,
//     using "unavailable" when no command or exit code was recorded
//  4. Explicit mutable next-step fields (what the worker can change)
//
// This is appended to the retry prompt so the worker knows exactly what
// failed, what evidence exists, and what it can modify.
func buildRetryContext(class TaskFailureClass, lastErr error, transcriptRef, verifyCmd string, verifyExit int, workerExitCode *int, lastToolCall, lastToolInput, lastToolResult string, lastToolResultErr bool, lastOutput string, task TaskDef) string {
	b := &strings.Builder{}
	b.WriteString("\n\n## Retry Context (§6.1)\n\n")
	if lastErr == nil {
		lastErr = errors.New("unknown failure")
	}
	transcriptRef = redactRetryText(transcriptRef, 500)
	verifyCmd = redactRetryText(verifyCmd, 500)
	lastToolCall = redactRetryText(lastToolCall, 120)
	lastToolInput = redactRetryText(lastToolInput, 200)
	lastToolResult = redactRetryText(lastToolResult, 300)
	lastOutput = redactRetryText(lastOutput, 300)
	lastFailure := redactRetryText(lastErr.Error(), 500)

	// 1. Failure class
	fmt.Fprintf(b, "**Failure class:** %s\n", class)

	// 2. Evidence reference — include the actual partial output that
	// authorized the retry, the transcript reference, and the error.
	b.WriteString("**Evidence:** ")
	if transcriptRef != "" {
		fmt.Fprintf(b, "transcript at %s; ", transcriptRef)
	}
	if strings.TrimSpace(lastOutput) != "" {
		fmt.Fprintf(b, "partial output: %s; ", lastOutput)
	}
	fmt.Fprintf(b, "error: %s\n", lastFailure)

	// 3. Previous command and exit/verification result — always rendered
	// with the actual command/input when available (§6.1).
	b.WriteString("**Previous command/exit:** ")
	if verifyCmd != "" {
		fmt.Fprintf(b, "verify `%s` (exit: %d)", verifyCmd, verifyExit)
	} else if lastToolCall != "" && lastToolInput != "" {
		exitStr := "unavailable"
		if lastToolResultErr {
			exitStr = "error"
		} else if lastToolResult != "" {
			exitStr = "ok"
		}
		if workerExitCode != nil {
			exitStr = fmt.Sprintf("%d", *workerExitCode)
		}
		fmt.Fprintf(b, "%s (input: `%s`, exit: %s", lastToolCall, lastToolInput, exitStr)
		if lastToolResult != "" {
			fmt.Fprintf(b, ", result: %s", lastToolResult)
		}
		b.WriteString(")")
	} else if lastToolCall != "" {
		exitStr := "unavailable"
		if workerExitCode != nil {
			exitStr = fmt.Sprintf("%d", *workerExitCode)
		}
		fmt.Fprintf(b, "last tool call: %s (exit: %s)", lastToolCall, exitStr)
	} else if workerExitCode != nil {
		fmt.Fprintf(b, "agent execution (exit: %d)", *workerExitCode)
	} else {
		b.WriteString("agent execution (exit: unavailable, no tool calls recorded)")
	}
	b.WriteString("\n")

	// 4. Explicit mutable next-step fields
	b.WriteString("**What you can change:** ")
	switch class {
	case FailureContract:
		b.WriteString("The task contract (verify command, assertion, or deadline) is invalid. Fix the contract definition rather than retrying the same work.\n")
	case FailureEnvironment:
		b.WriteString("The execution environment (PATH, shell, workdir, or executable) is misconfigured. Resolve the environment issue before retrying.\n")
	case FailureVerify:
		b.WriteString("The deliverable exists but does not meet the verification assertion. Fix the artifact content, structure, or format to satisfy the verify command.\n")
	case FailureTimeout:
		b.WriteString("The task timed out. Optimize for speed, reduce scope, or break the work into smaller steps.\n")
	case FailureProtocol:
		b.WriteString("The execution completed but the result protocol was not followed. Submit the structured result via submit_result.\n")
	case FailureExecution:
		b.WriteString("The execution failed. Change your approach, fix the error, and produce a working deliverable.\n")
	case FailurePolicy:
		b.WriteString("A policy guard blocked the operation. Adjust the approach to comply with the guard rules.\n")
	default:
		b.WriteString("Change your approach based on the error above.\n")
	}

	return b.String()
}
