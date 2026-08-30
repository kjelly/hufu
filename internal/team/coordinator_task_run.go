package team

// Single-task execution: building the worker prompt, running the agent with
// status reporting, deliverable verification, and failure reflection.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/audit"
	"github.com/kjelly/hufu/internal/hooks"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

// errCoordinatorToolFailure marks a direct coordinator tool error as a hard
// orchestration boundary. It is intentionally distinct from worker failures:
// workers can still report bounded partial results after collecting failure
// evidence, while coordinators must not continue making decisions from it.
var errCoordinatorToolFailure = errors.New("coordinator direct tool failure")

type coordinatorModelContinuation struct {
	Model    fantasy.LanguageModel
	Messages []fantasy.Message
	System   string
	Tools    []fantasy.AgentTool
	ToolsSet bool
	Context  context.Context
}

// admitCoordinatorContext is the single coordinator admission boundary. It
// records every complete-request decision before the caller can cross into a
// provider, including a typed pre-provider CannotFit candidate.
func (c *Coordinator) admitCoordinatorContext(ctx context.Context, manager *ContextWindowManager, request ContextWindowRequest, phase, taskID string, attempt int) (ContextWindowAdmission, error) {
	admission, err := manager.Admit(ctx, request)
	if err != nil {
		return admission, err
	}
	telemetry := c.newContextWindowTelemetry(EventContextWindowAdmission, request, admission, phase, taskID, attempt)
	if admission.Candidate != nil {
		telemetry.FallbackReason = "candidate_requires_final_admission"
	}
	if err := c.recordContextWindowTelemetry(EventContextWindowAdmission, telemetry, taskID); err != nil {
		return ContextWindowAdmission{}, err
	}
	return admission, nil
}

// admitCoordinatorEarlierModel is the only coordinator model downshift
// boundary. It accepts only a compacted candidate that the current P1
// admission proved could not be sent, and independently admits each earlier
// configured model before constructing its continuation model.
func (c *Coordinator) admitCoordinatorEarlierModel(ctx context.Context, preflight *coordinatorRequestPreflight, messages []fantasy.Message, prompt string, stepNumber, maxOutputTokens int, currentModel string) (coordinatorModelContinuation, error) {
	if c == nil || preflight == nil || c.providerManager == nil {
		return coordinatorModelContinuation{}, nil
	}
	currentIndex := -1
	for i, entry := range c.modelList {
		if entry.ID == currentModel {
			currentIndex = i
			break
		}
	}
	if currentIndex <= 0 {
		return coordinatorModelContinuation{}, nil
	}
	fullSystem, fullTools := preflight.configuration()
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	seen := make(map[string]bool)
	for i := currentIndex - 1; i >= 0; i-- {
		candidateID := strings.TrimSpace(c.modelList[i].ID)
		if candidateID == "" || seen[candidateID] || candidateID == currentModel {
			continue
		}
		seen[candidateID] = true
		candidatePreflight := newCoordinatorRequestPreflight(candidateID, prompt, fullSystem, fullTools)
		candidateSpec := globalRegistry.GetSpec(candidateID).WithEffectiveMaxOutputTokens(maxOutputTokens)
		// A downshift must not trade a proven CannotFit for an unknown
		// capacity: estimated candidates stay ineligible even though the
		// primary model admits its own estimate.
		if candidateSpec.IsEstimated {
			continue
		}
		candidateSystem, candidateTools, applied, err := candidatePreflight.prepare(ctx, messages, prompt, maxOutputTokens, stepNumber)
		if err != nil {
			continue
		}
		requestSystem, requestTools := fullSystem, fullTools
		if applied {
			requestSystem, requestTools = candidateSystem, candidateTools
		}
		manager := NewContextWindowManager(defaultCounter, nil)
		admission, err := c.admitCoordinatorContext(ctx, manager, ContextWindowRequest{
			ModelID: candidateID, System: requestSystem, Tools: requestTools, Messages: messages,
			Prompt: prompt, Window: candidateSpec.ContextWindow, ReservedOutputTokens: maxOutputTokens,
			SafetyMarginTokens: candidateSpec.SafetyMarginTokens, StepNumber: stepNumber,
		}, "downshift_candidate", CoordTodoID, attempt)
		if err != nil || admission.Decision == ContextWindowCannotFit || admission.Messages == nil {
			continue
		}
		provider := c.providerManager.GetProvider(candidateID)
		if provider == nil {
			continue
		}
		payload := map[string]any{"from_model": currentModel, "to_model": candidateID, "request_tokens": admission.RequestTokens, "available": admission.Budget.Available, "step": stepNumber}
		if err := c.emitEvent(modelContinuationEventType, "coordinator", CoordTodoID, payload); err != nil {
			return coordinatorModelContinuation{}, fmt.Errorf("persist coordinator model continuation admission: %w", err)
		}
		downshift := c.newContextWindowTelemetry(EventContextWindowDownshift, ContextWindowRequest{ModelID: candidateID, ReservedOutputTokens: maxOutputTokens, SafetyMarginTokens: candidateSpec.SafetyMarginTokens, Window: candidateSpec.ContextWindow, StepNumber: stepNumber}, admission, "downshift", CoordTodoID, attempt)
		downshift.Decision = "downshift"
		downshift.FallbackReason = "earlier_model_admitted"
		if err := c.recordContextWindowTelemetry(EventContextWindowDownshift, downshift, CoordTodoID); err != nil {
			return coordinatorModelContinuation{}, err
		}
		model, err := provider.LanguageModel(ctx, candidateID)
		if err != nil {
			continue
		}
		return coordinatorModelContinuation{
			Model: model, Messages: admission.Messages, System: candidateSystem, Tools: candidateTools,
			ToolsSet: applied, Context: context.WithValue(ctx, modelKey{}, candidateID),
		}, nil
	}
	exhausted := c.newContextWindowTelemetry(EventContextWindowDownshift, ContextWindowRequest{ModelID: currentModel, ReservedOutputTokens: maxOutputTokens, StepNumber: stepNumber}, ContextWindowAdmission{Decision: ContextWindowCannotFit}, "downshift", CoordTodoID, attempt)
	exhausted.Decision = "exhausted"
	exhausted.FallbackReason = "no_admitted_candidate"
	if err := c.recordContextWindowTelemetry(EventContextWindowDownshift, exhausted, CoordTodoID); err != nil {
		return coordinatorModelContinuation{}, err
	}
	return coordinatorModelContinuation{}, nil
}

// acceptedTerminalResultStop is scoped to one worker stream invocation. The
// result tool flips it only after the occurrence transaction commits; the
// stream boundary then stops before Fantasy asks the model for another step.
// Keeping the occurrence identity here prevents a delayed or stale result
// from stopping a different retry or task.
type acceptedTerminalResultStop struct {
	mu       sync.Mutex
	identity submitResultRuntimeIdentity
	accepted bool
}

type acceptedTerminalResultStopKey struct{}

func (s *acceptedTerminalResultStop) markAccepted(identity submitResultRuntimeIdentity) {
	if s == nil || !validSubmitResultIdentity(identity) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accepted {
		return
	}
	if validSubmitResultIdentity(s.identity) && !sameTaskResultOccurrence(s.identity, identity) {
		return
	}
	s.identity = identity
	s.accepted = true
}

func (s *acceptedTerminalResultStop) isAcceptedFor(c *Coordinator, todoID string) bool {
	if s == nil || c == nil || strings.TrimSpace(todoID) == "" {
		return false
	}
	s.mu.Lock()
	identity, accepted := s.identity, s.accepted
	s.mu.Unlock()
	if !accepted || identity.TaskID != todoID {
		return false
	}
	active, ok := c.activeTaskResultOccurrence(todoID)
	return ok && sameTaskResultOccurrence(identity, active)
}

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (result string, returnErr error) {
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
	resolvedSideEffect, resolvedRecovery, resolvedReconcileTool := resolveTaskRecovery(agentDef, task)
	// Recovery decisions below operate on TaskDef, not the agent definition.
	// Materialize the resolved values once so diagnostics and retry policy do
	// not accidentally treat a workspace-writing worker as side_effect:none.
	task.SideEffect = resolvedSideEffect
	task.Recovery = resolvedRecovery
	task.ReconcileTool = resolvedReconcileTool

	if len(agentDef.MCPTools) > 0 {
		defer func() {
			_ = c.mcpManager.UnloadAgentMCPServer(agentName)
		}()
	}

	// Check if agent has extra-models configured
	if len(agentDef.ExtraModels) > 0 {
		return c.executeTaskWithExtraModels(parentCtx, agentName, agentDef, task, todoID)
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

	maxAttempts := c.effectiveWorkerMaxAttempts(agentDef)

	if err := c.CommitTaskTransition(parentCtx, todoID, TaskPending, TaskInProgress, "", "", nil); err != nil {
		return "", fmt.Errorf("mark task started: %w", err)
	}
	// Every path after admission must leave canonical lifecycle state terminal
	// when it returns an error. The normal retry loop already persists its
	// classified decision; this postcondition covers deterministic pre-model
	// failures such as tool, context, and artifact preflight errors.
	defer func() {
		if returnErr != nil {
			c.terminalizeTaskErrorIfUnresolved(todoID, returnErr)
		}
	}()
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

	var approvedPlan, instructions string
	if task.PlanFirst && task.PlanID != "" {
		c.pendingPlansMu.Lock()
		entry := c.pendingPlans[task.PlanID]
		if entry == nil {
			c.pendingPlansMu.Unlock()
			return "", fmt.Errorf("plan not found for id %s", task.PlanID)
		}
		planText := entry.PlanText
		c.pendingPlansMu.Unlock()

		approvedPlan = planText
		instructions = "Execute the approved plan above. You have already planned — now implement each step.\n" + c.sharedKnowledgeInstructions(granted)
	} else if task.PlanFirst {
		instructions = "Draft a detailed task execution plan before doing any work. Your plan should be a numbered list of concrete, actionable steps with brief descriptions. Consider your skills, available tools, and the project context. Call `submit_plan` with your complete plan when ready. Do NOT execute any steps yet — only plan."
	} else {
		instructions = "You are a domain expert. Determine your own implementation approach based on the goal above.\n" + c.sharedKnowledgeInstructions(granted)
	}
	verificationCriteria := ""
	if task.Verify != "" && (!task.PlanFirst || task.PlanID != "") {
		verificationCriteria = completionVerificationInstructions(task.Verify, c.projectDir)
	}
	// The result protocol is enforced for every non-sidecar task, so it has to
	// be stated. A worker that ends its turn with prose fails the contract, and
	// the failure is indistinguishable from real non-completion.
	if !task.PlanFirst || task.PlanID != "" {
		instructions += resultProtocolInstructions(task, granted)
	}
	if taskUsesVerbatimTranscript(task) {
		instructions += "\n\n## Verbatim Output Contract\n\nhufu captures every tool call and tool result into a complete transcript artifact. Do not reproduce raw command output in your final response. Submit a concise structured result; the runner will attach the authoritative transcript manifest."
	}
	if agentDef != nil {
		if note := toolUsageNotes(granted); note != "" {
			instructions += note
		}
	}

	// SSH session tracking is handled by the ssh tool's response hint.
	// No coordinator-level tracking is needed - each SSH call is independent.

	skillContext, skillErr := c.buildSkillContextItems(agentDef, agentName, task.Goal, todoID, granted)
	if skillErr != nil {
		return "", fmt.Errorf("worker skill context preflight failed: %w", skillErr)
	}
	runtimeContext := ""
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		execCtx := c.phaseWorkflow.executionContext()
		runtimeContext = formatRuntimeWorkflowContext(c.phaseWorkflow.State(), execCtx)
	}
	if item := c.todoItemByID(todoID); item != nil && item.WorksetBinding != nil {
		runtimeContext = appendWorksetWorkerRuntimeContext(runtimeContext, item.WorksetBinding)
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
						depResults = append(depResults, projectDependencyResultForWorker(res, currentTodo.WorksetBinding))
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
	request := c.newTaskContextRequest(task, todoID, 1, ContextTriggerTaskDispatch, agentName, agentDef.Role, nil)
	if !c.historicalMemoryDisabled() {
		canonical = c.contextRepo != nil
	}
	if !c.historicalMemoryDisabled() && !canonical {
		rawSTM, rawLTM, memoryStore = LoadSTM(c.session.Workspace), LoadLTM(c.session.Workspace, c.session.Config.Name), c.memoryStore
	}
	workerInput := buildWorkerContextInput(request, task, agentDef, approvedPlan, instructions, verificationCriteria, runtimeContext, skillContext)
	workerInput.RawSTM = rawSTM
	workerInput.RawLTM = rawLTM
	workerInput.ContextFiles = contextFiles
	workerInput.ConcurrentTasks = c.buildConcurrentTasksContext(todoID)
	workerInput.DependencyResults = depResults
	workerInput.MemoryStore = memoryStore
	workerInput.CanonicalMemory = canonicalMemory
	workerInput.ModelContext = modelSpec
	workerInput.MaxAuxChars = maxWorkerAuxContextChars
	workerInput.DisableMemory = c.historicalMemoryDisabled()
	legacyPrompt := strings.Join([]string{task.Goal, task.Constraints, approvedPlan, instructions, verificationCriteria, runtimeContext}, "\n\n")

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
	var lastClass TaskFailureClass      // previous attempt's failure class
	var lastTranscriptRef string        // previous attempt's opaque transcript artifact reference
	var lastVerifyCmd string            // previous attempt's verify command
	var lastVerifyExit int              // previous attempt's verify exit code (-1 = unknown)
	var lastExitCode *int               // previous attempt's worker exit code (nil = errored)
	var lastToolCall string             // previous attempt's last tool call name (if any)
	var lastToolInput string            // previous attempt's last tool call input (actual command)
	var lastToolResult string           // previous attempt's last tool result preview
	var lastToolResultErr bool          // previous attempt's last tool result was an error
	var lastPartialOutput string        // previous attempt's partial output (evidence)
	var lastSubmittedResult *TaskResult // previous structurally valid non-terminal handoff
	// A model that reaches work execution but cannot honour submit_result is a
	// capability failure, not evidence that it performed the requested write.
	// We permit exactly one clean re-dispatch only after every recorded tool
	// call is independently proven read-only. This path intentionally does not
	// weaken the normal fail-closed rule for any uncertain or mutating attempt.
	protocolCapabilityRetryUsed := false
	protocolFallbackModel := ""
	// A policy-denied attempt has no executable work to replay. Keep its
	// structured dispositions separate from ordinary failure evidence so the
	// one permitted retry receives deterministic correction instructions.
	var lastPolicyDeniedDispositions []ToolExecutionDisposition
retryLoop:
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptsMade = attempt
		c.setCurrentTaskAttempt(todoID, attempt)
		attemptIdentity, attemptIdentityOK := c.activeTaskResultOccurrence(todoID)
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
		if taskUsesVerbatimTranscript(task) || task.Execution.RequiresResult || c.allowsFreeTextWorkerResult(task) {
			// Each attempt gets its own immutable transcript. The repair agent
			// is intentionally given no recorder, so it cannot alter this
			// execution evidence. Explicitly opted-in read-only free-text workers
			// need the same runner-owned evidence: their provider may omit
			// submit_result, but its prose must not become accepted completion
			// without an auditable record of the inspection that produced it.
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
			// setCurrentTaskAttempt above atomically opened and cleared the new
			// occurrence. Do not clear again: a delayed old submission must be
			// rejected against the new owner, and a new submission must not be
			// erased between opening and execution.
		}
		trigger := ContextTriggerTaskDispatch
		var failureContext *ContextFailure
		if attempt > 1 {
			trigger = ContextTriggerRetry
			failureContext = &ContextFailure{Class: lastClass, ErrorClass: string(lastClass), EvidenceRefs: []string{lastTranscriptRef}, ToolName: lastToolCall, ToolInputHash: hashContentKey(utils.RedactSecrets(lastToolInput)), ExitCode: lastExitCode}
			if lastErr != nil {
				failureContext.EvidenceRefs = append(failureContext.EvidenceRefs, redactRetryText(lastErr.Error(), 300))
			}
		}
		request = c.newTaskContextRequest(task, todoID, attempt, trigger, agentName, agentDef.Role, failureContext)
		attemptArtifactScope, scopeErr := c.buildArtifactAccessScope(todoID, attempt)
		if scopeErr != nil {
			closeTranscript()
			return "", fmt.Errorf("artifact scope preflight failed: %w", scopeErr)
		}
		if c.workerAgentOverride == nil {
			if err := c.validateBoundWorkerToolPolicy(resolvedTools, task, todoID, attemptArtifactScope); err != nil {
				closeTranscript()
				return "", fmt.Errorf("worker tool policy preflight failed: %w", err)
			}
		}
		retrievalQuery := request.RetrievalQuery()
		attemptInput := workerInput
		attemptInput.Request = request
		if attempt > 1 && lastErr != nil {
			if len(lastPolicyDeniedDispositions) > 0 {
				attemptInput.FailureContext = buildPolicyDeniedRetryContext(lastPolicyDeniedDispositions)
			} else {
				attemptInput.FailureContext = buildRetryContextWithSubmittedResult(lastClass, lastErr, lastTranscriptRef, lastVerifyCmd, lastVerifyExit, lastExitCode, lastToolCall, lastToolInput, lastToolResult, lastToolResultErr, lastPartialOutput, lastSubmittedResult, task)
				failureEvidence := redactRetryText(lastErr.Error(), 500)
				if hint := c.reflectOnFailure(parentCtx, agentName, task.Goal, failureEvidence); hint != "" {
					attemptInput.FailureContext += hint
					appliedHint = strings.TrimPrefix(hint, reflectionHeader)
					appliedHintTrigger = failureEvidence
					c.rememberDiagnosticHint(todoID, appliedHint)
				}
			}
		}
		var routeDecisions []ContextRouteDecision
		if canonical && !c.historicalMemoryDisabled() {
			bundle, decisions, _, routeErr := c.canonicalContextBundleForRequest(parentCtx, request)
			if routeErr != nil {
				closeTranscript()
				return "", fmt.Errorf("worker context routing preflight failed: %w", routeErr)
			}
			attemptInput.CanonicalMemory = bundle
			routeDecisions = decisions
		}
		attemptInput.WorkerMemory = c.recallWorkerMemory(parentCtx, agentDef, retrievalQuery)
		compiled, compileErr := c.ContextCompiler().CompileWorkerContext(parentCtx, attemptInput)
		c.recordShadowTrace(parentCtx, "worker", legacyPrompt, request, routeDecisions, attemptInput.ModelContext, compiled, compileErr)
		if compileErr != nil {
			closeTranscript()
			return "", fmt.Errorf("worker context preflight failed: %w", compileErr)
		}
		if strings.TrimSpace(compiled.Prompt) == "" {
			closeTranscript()
			return "", fmt.Errorf("worker context preflight failed: compiled prompt is empty")
		}
		contextManifest := BuildContextInjectionManifest(request, compiled, routeDecisions, agentName, time.Now().UTC())
		if err := c.persistContextManifest(&contextManifest); err != nil {
			closeTranscript()
			return "", fmt.Errorf("worker context manifest preflight failed: %w", err)
		}
		attemptStarted := time.Now()
		runID := c.executionRunID
		if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			runID = c.taskTracker.TodoList().RunID()
		}
		attemptManifest := buildMemoryInjectionManifestFromContextManifest(compiled, &contextManifest, runID, todoID, attempt, agentName, retrievalQuery, c.session.Config.MemoryLearning)
		if err := c.persistMemoryManifest(attemptManifest); err != nil {
			closeTranscript()
			return "", fmt.Errorf("worker memory manifest preflight failed: %w", err)
		}
		c.recordExecutionEvent(todoID, agentName, attempt, "in_progress", resolvedModel, 0, ExecutionUsage{})
		if attemptArtifactScope != nil {
			committedScopeReceipt := &ExecutionReceipt{
				RunID: runID, TaskID: todoID, Attempt: attempt, ProducerID: agentName,
				ArtifactScope: cloneArtifactAccessScope(attemptArtifactScope),
			}
			if err := c.taskTracker.TodoList().SetExecutionReceipt(todoID, committedScopeReceipt); err != nil {
				closeTranscript()
				return "", fmt.Errorf("commit artifact scope receipt: %w", err)
			}
		}
		currentPrompt := compiled.Prompt
		if attempt > 1 {
			detail := fmt.Sprintf("attempt %d/%d", attempt, maxAttempts)
			if err := c.commitTaskTransitionFromCurrent(parentCtx, todoID, TaskInProgress, detail, "", nil); err != nil {
				closeTranscript()
				return "", fmt.Errorf("mark retry task started: %w", err)
			}
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d/%d — continuing from previous progress", attempt, maxAttempts)))
			if protocolFallbackModel != "" {
				resolvedModel = protocolFallbackModel
				protocolFallbackModel = ""
				c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("retrying result-contract failure with fallback model %s", resolvedModel)).withTodoID(todoID))
			} else if escalate {
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
		attemptDispositions := &attemptToolDispositions{}
		protocolFailure := false
		protocolCapabilityFallback := false
		policyDenialRepairExhausted := false
		protocolRepairTerminalFailure := false
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
			taskCtx = context.WithValue(taskCtx, taskRequiresResultKey{}, task.Execution.RequiresResult)
			taskCtx = withInvocationMetadata(taskCtx, invocationMetadataFromRequest(request, contextManifest))
			if identity, ok := c.activeTaskResultOccurrence(todoID); ok {
				taskCtx = withSubmitResultRuntimeIdentity(taskCtx, identity)
			}
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
			taskCtx = context.WithValue(taskCtx, workerStepBudgetKey{}, stepBudget)
			// Read-only teams that explicitly opt into free-text handoffs must
			// still get a usable final budget step.  Their contract deliberately
			// has no submit_result tool, so the terminal step below closes every
			// tool and asks for Markdown rather than exposing an impossible
			// submit_result-only interface.
			if c.allowsFreeTextWorkerResult(task) {
				taskCtx = context.WithValue(taskCtx, workerFreeTextFinalizationKey{}, true)
			}
			taskCtx = context.WithValue(taskCtx, tools.ToolExecutionDispositionReporterKey, newToolDispositionReporter(attemptDispositions, resolvedSideEffect, runID, todoID, attempt))
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
			if resolvedSideEffect == SideEffectNone {
				taskCtx = context.WithValue(taskCtx, tools.AgentReadOnlyExecutionKey, true)
			}
			if writePaths := c.runtimeAllowedWritePaths(); len(writePaths) > 0 {
				taskCtx = context.WithValue(taskCtx, tools.AgentAllowedWritePathsKey, writePaths)
				if command := c.boundedWorkflowBashCommand(task); command != "" {
					taskCtx = context.WithValue(taskCtx, tools.WorkflowBoundedBashKey, tools.WorkflowBoundedBash{Command: command})
				}
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
			if attemptArtifactScope != nil {
				taskCtx = context.WithValue(taskCtx, artifactAccessScopeKey, cloneArtifactAccessScope(attemptArtifactScope))
				taskCtx = context.WithValue(taskCtx, tools.ArtifactPathPolicyKey, tools.ArtifactPathPolicy{
					BlockedPaths:             c.artifactScopePathCandidates(attemptArtifactScope),
					FailClosedForUnsupported: c.todoItemByID(todoID) != nil && c.todoItemByID(todoID).WorksetBinding != nil,
				})
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
			if err == nil && !task.Execution.RequiresResult && len(steps) > 0 &&
				(strings.TrimSpace(output) == "" || (c.allowsFreeTextWorkerResult(task) && freeTextResultNeedsSummary(task, output))) {
				// The agent worked but did not produce an acceptable final message.
				// Give it one genuinely tool-free turn to summarize instead of
				// accepting a mid-narration fragment or re-running inspection from
				// scratch. Requires-result workers instead receive the dedicated
				// result-only finalization turn below, where submit_result is the
				// sole exposed tool.
				if rescued := c.rescueFinalSummary(taskCtx, ag, agentName, agentDef, resolvedModel, task, steps, timing); rescued != "" && validateTaskOutput(task, rescued) == nil {
					output = rescued
				} else if c.allowsFreeTextWorkerResult(task) {
					// This is an explicitly opted-in, read-only review task. Its
					// transcript remains the evidence, while this deterministic
					// result makes the limitation visible to the coordinator without
					// inventing a success claim. It is deliberately still a failed
					// attempt: a coverage-limited handoff cannot be marked done or
					// become eligible for the coordinator's success-only dispatch
					// protections. The normal retry policy decides whether the worker
					// gets another attempt.
					output = incompleteReadOnlyReviewSummary(len(steps))
					err = withFailureClassOverride(
						errors.New("free-text worker produced an incomplete review handoff; retry is required before accepting the task"),
						FailureExecution,
					)
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
			RunID:            runID,
			TaskID:           todoID,
			Attempt:          attempt,
			ModelExecutionID: contextManifest.ModelExecutionID,
			StartedAt:        attemptStarted,
			FinishedAt:       time.Now(),
			ProducerID:       agentName,
			ArtifactScope:    cloneArtifactAccessScope(attemptArtifactScope),
			TranscriptRef:    transcriptRef,
			MemoryManifest:   cloneMemoryInjectionManifest(attemptManifest),
			ContextManifest:  cloneContextInjectionManifest(&contextManifest),
			StepBudget: &StepBudgetUsage{
				Used:      len(steps),
				Limit:     stepBudget,
				Exhausted: stepBudget > 0 && len(steps) >= stepBudget,
			},
			ToolDispositions: attemptDispositions.snapshot(),
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
			// A worker that reports partial/failed/blocked has made a valid typed
			// handoff, but not a terminal completion. Preserve it on this exact
			// receipt and route it through normal recovery as an execution
			// failure; malformed submit_result data remains a protocol failure.
			if typedRes != nil && typedRes.Source == "submitted" {
				receipt.HandoffState = ResultHandoffSubmitted
				if schemaErr := validateSubmittedTaskResult(typedRes); schemaErr != nil {
					err = withFailureClassOverride(schemaErr, FailureProtocol)
					failedExit := 1
					receipt.ExitCode = &failedExit
					if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
						_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
					}
				} else if resultErr := validateCompletedTaskResult(typedRes); resultErr != nil {
					receipt.SubmittedResult = typedRes
					err = withFailureClassOverride(resultErr, FailureExecution)
					failedExit := 1
					receipt.ExitCode = &failedExit
					if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
						_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
					}
				}
			}
			if typedRes == nil {
				if task.Execution.RequiresResult {
					budgetExhausted := receipt.StepBudget != nil && receipt.StepBudget.Exhausted
					safePolicyDenial := !budgetExhausted && attemptDispositions.onlyPolicyDeniedToolCalls(steps) && parentCtx.Err() == nil
					if safePolicyDenial {
						receipt.HandoffState = ResultHandoffMissingAfterSafeDenial
						if protocolCapabilityRetryUsed {
							protocolFailure = true
							policyDenialRepairExhausted = true
							err = withFailureClassOverride(errors.New("policy-denied task omitted submit_result after its only safe fresh retry"), FailurePolicy)
						} else {
							protocolCapabilityFallback = true
							err = withFailureClassOverride(errors.New("worker tool call was denied before execution; applying deterministic policy repair"), FailureExecution)
						}
						if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
							_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
						}
					} else {
						// Protocol failure: the agent finished execution but omitted submit_result.
						// Classify as FailureProtocol, set task to protocol_incomplete,
						// and attempt single-step, tool-free repair allowing ONLY submit_result.
						protocolFailure = true
						// §8: the step budget covers work; result finalization gets its
						// own turn outside it. Distinguishing exhaustion from a genuine
						// protocol violation keeps the retry hint honest — telling a
						// truncated worker to "change your approach" is what turns a
						// nearly-finished task into a thrashing loop.
						receipt.HandoffState = handoffStateForMissingResult(budgetExhausted, attemptDispositions, steps, parentCtx.Err())
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

						{
							repairResultTool := &submitResultTool{coordinator: c, todoID: todoID}
							repairEvidence := utils.TruncateRunes(output, 12000)
							repairPrompt := fmt.Sprintf("## Goal\n%s\n\n## Bounded execution evidence\n%s\n\n## Repair Instructions\nYour execution completed and produced output, but you did not submit a structured result via submit_result as required. Call submit_result now using only the bounded evidence above to supply the required structured result. Include a concise summary and put any complete plan, analysis, review, or report body in `details`. For `open_questions`, use strings or objects with `question` and optional string `context`/`detail` fields. Do NOT call any other tools or emit a prose final response.\n", utils.TruncateRunes(task.Goal, 4000), repairEvidence)
							if budgetExhausted {
								repairPrompt = fmt.Sprintf("## Goal\n%s\n\n## Bounded execution evidence\n%s\n\n## Finalization Instructions\nYou ran out of steps (%d/%d) before submitting a result. The evidence above is bounded and this turn is only for reporting it. Call submit_result now, and do NOT call any other tools or emit a prose final response. Put any complete textual deliverable in `details`. Use `success` only when fully met; otherwise use `partial` or `blocked` truthfully.", utils.TruncateRunes(task.Goal, 4000), repairEvidence, len(steps), stepBudget)
							}
							// Result-only repair must be a clean tool context. Replaying the
							// original tool-call messages caused models to repeat a prior
							// `view`/`grep` call even though only submit_result is exposed.
							// The prompt already contains the worker's final output; provide
							// a bounded, text-only evidence summary rather than executable
							// tool history.
							repairPrompt += protocolRepairEvidenceSummary(steps, transcriptArtifact)
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
								repairCtx = context.WithValue(repairCtx, protocolRepairExecutionKey{}, true)
								// Protocol repair has no separate execution receipt. The
								// parent worker context is receipt-backed, so override its
								// marker before accounting this auxiliary LLM stream.
								repairCtx = context.WithValue(repairCtx, llmUsageReceiptExpectedKey{}, false)
								repairCtx = context.WithValue(repairCtx, todoIDKey{}, todoID)
								repairCtx = context.WithValue(repairCtx, modelKey{}, resolvedModel)
								repairCtx = context.WithValue(repairCtx, tools.AgentNameKey, agentName)
								if identity, ok := c.activeTaskResultOccurrence(todoID); ok {
									repairCtx = withSubmitResultRuntimeIdentity(repairCtx, identity)
								}
								repairCtx = context.WithValue(repairCtx, hooks.AgentNameKey, agentName)
								repairCtx = context.WithValue(repairCtx, hooks.TeamNameKey, c.session.Config.Name)
								repairCtx = context.WithValue(repairCtx, hooks.TaskDescKey, taskDesc)
								repairCtx = context.WithValue(repairCtx, taskToolSequenceKey{}, attemptSequence.protocolRepairSequence())

								preparedPrompt, prepareErr := c.prepareAuxiliaryPrompt(repairCtx, "result_repair", prompt)
								if prepareErr != nil {
									return nil
								}
								_, repairSteps, _ := c.runAgentWithStatusAndHistory(repairCtx, repairAg, agentName, preparedPrompt, nil, timing, fantasy.StepCountIs(1))
								typedRes = c.GetTaskResult(todoID)
								return repairSteps
							}

							repairSteps := runRepair(repairPrompt)
							repairSuccess := typedRes != nil && typedRes.Source == "submitted" && validateCompletedTaskResult(typedRes) == nil
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
								schemaRepairPrompt := fmt.Sprintf("## Goal\n%s\n\n## Schema-only repair\nThe previous submit_result call was rejected because its arguments did not match the result schema. This is the final repair attempt. Call submit_result exactly once with corrected schema and preserve only the bounded execution evidence below. Do NOT execute work, call any other tools, or emit a prose final response. The call must include both required fields: `status` (one of `success`, `completed_with_gaps`, `partial`, `failed`, or `blocked`) and a non-empty `summary`; put any complete textual deliverable in `details`. For `open_questions`, use strings or objects with `question` and optional string `context`/`detail` fields.\n\n## Bounded execution evidence\n%s", utils.TruncateRunes(task.Goal, 4000), repairEvidence)
								schemaRepairSteps := runRepair(schemaRepairPrompt)
								repairSuccess = typedRes != nil && typedRes.Source == "submitted" && validateCompletedTaskResult(typedRes) == nil
								repairReason, reclassifyExecution = classifyRepairFailure(schemaRepairSteps, typedRes)
								repairAttempts = append(repairAttempts, RepairAttemptProvenance{
									Attempt:         2,
									Success:         repairSuccess,
									Prompt:          schemaRepairPrompt,
									SubmittedResult: typedRes,
									FailureReason:   repairReason,
								})
							}
							// A read-only worker may produce a complete Markdown handoff in
							// its final assistant message but omit the typed tool call. Promote
							// only when the runtime can prove the attempt used observation-only
							// tools and the declared evidence contract validates the full text.
							// This preserves the typed-result/report pipeline without allowing
							// an incomplete or mutating free-text claim to become success.
							if !repairSuccess && !reclassifyExecution && typedRes == nil &&
								resolvedSideEffect == SideEffectNone && protocolAttemptWasReadOnly(steps) {
								if promoted := promoteValidatedReadOnlyHandoff(task, todoID, agentName, output); promoted != nil {
									c.storeSubmittedTaskResult(todoID, promoted)
									typedRes = promoted
									repairSuccess = true
									repairReason = ""
									last := &repairAttempts[len(repairAttempts)-1]
									last.Success = true
									last.FailureReason = ""
									last.SubmittedResult = promoted
								}
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
								if typedRes != nil && typedRes.Source == "promoted_free_text" {
									receipt.HandoffState = ResultHandoffPromotedFreeText
								} else {
									receipt.HandoffState = ResultHandoffSubmitted
								}
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
									receipt.SubmittedResult = typedRes
									err = withFailureClassOverride(
										fmt.Errorf("execution failure (reclassified from protocol repair: worker reported status %q via submit_result; task is not complete) for task %s (%s)", submittedStatus, todoID, agentName),
										FailureExecution,
									)
									receipt.RepairProvenance.Error = err.Error()
								} else {
									// Preserve the worker's original output as a low-confidence,
									// provisional result. It is evidence for reconciliation, not
									// a successful terminal result and never marks the task done.
									recovered := ParseFreeTextResult(utils.TruncateRunes(output, 12000))
									if strings.TrimSpace(output) == "" {
										recovered.Status = TaskResultStatusBlocked
										recovered.Summary = "Runtime could not obtain the required structured result; no usable execution evidence was produced."
									} else {
										recovered.Status = TaskResultStatusPartial
										evidence := utils.TruncateRunes(output, 12000)
										recovered.Summary = "Runtime obtained execution evidence but result finalization failed; work is partial and requires reconciliation. Evidence: " + evidence
										recovered.Details = evidence
									}
									recovered.TaskID = todoID
									recovered.Agent = agentName
									recovered.Source = "recovered_protocol"
									protocolRepairTerminalFailure = true
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
							if !repairSuccess && repairReason == RepairFailureNoToolCall && !budgetExhausted &&
								!protocolCapabilityRetryUsed && (protocolAttemptWasReadOnly(steps) || attemptDispositions.onlyPolicyDeniedToolCalls(steps)) && parentCtx.Err() == nil {
								// No state-changing tool ran, so one fresh worker attempt is
								// safe. Override the provisional protocol error only after the
								// normal protocol branch has built its evidence; otherwise that
								// branch would overwrite this execution classification.
								protocolFailure = false
								protocolCapabilityFallback = true
								err = withFailureClassOverride(errors.New("model did not honour the submit_result tool contract after a read-only attempt"), FailureExecution)
								receipt.RepairProvenance.Error = err.Error()
							}
							if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
								_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
							}
						}
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
					if resultErr := validateCompletedTaskResult(typedRes); resultErr != nil {
						err = withFailureClassOverride(resultErr, FailureExecution)
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
				if !attemptIdentityOK {
					err = fmt.Errorf("verbatim transcript finalization failed: no captured task result occurrence")
				} else {
					contract := taskResultSubmissionContractForTask(task)
					coordinatorOutput, err = c.finalizeTaskResultOccurrence(attemptIdentity, transcript, contract)
					if err != nil {
						err = fmt.Errorf("verbatim transcript finalization failed: %w", err)
					} else {
						// Verification and TaskDone must consume the same canonical
						// candidate that received the sealed transcript fields.
						typedRes = c.GetTaskResult(todoID)
						if typedRes == nil {
							err = fmt.Errorf("verbatim transcript finalization failed: canonical task result disappeared")
						}
					}
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
					verification, verr := c.verifyTaskDeliverableWithSpecAndResult(parentCtx, agentDef, task, steps, typedRes)
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
				if averr := c.adversarialVerify(parentCtx, task, todoID, output); averr != nil {
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
				if typedRes != nil && typedRes.Source == "submitted" {
					receipt.HandoffState = ResultHandoffSubmitted
					if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
						_ = c.taskTracker.TodoList().SetExecutionReceipt(todoID, &receipt)
					}
				}
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
			MaxRetries:          maxAttempts,
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
			ProtocolRetrySafe:   protocolFailure && resolvedSideEffect == SideEffectNone && protocolAttemptWasReadOnly(steps),
		}
		disposition, reason := DecideRecovery(recoveryInput)
		if policyDenialRepairExhausted {
			disposition = NeedsHuman
			reason = "policy denial repeated after the only safe fresh retry"
		}
		if protocolRepairTerminalFailure && resolvedSideEffect != SideEffectNone {
			// Once a mutating attempt has run, a failed result-only repair is a
			// terminal runtime outcome. Never redispatch the worker: doing so could
			// repeat an external side effect from an attempt whose final state is
			// already represented by the deterministic partial/blocked result.
			disposition = ReconcileOnly
			reason = "result-only repair failed after a side-effecting attempt; reconcile the preserved partial/blocked result"
		}
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
			MaxAttempts:     maxAttempts,
			BudgetExhausted: parentCtx.Err() != nil,
		})
		_ = c.emitEvent("repair_decision", "repair_controller", todoID, map[string]interface{}{
			"action": string(repairDecision.Action), "reason": repairDecision.Reason, "attempt": attempt,
		})
		if disposition == RetryWorker && repairDecision.Action == RepairBlock {
			disposition = ReconcileOnly
			reason = repairDecision.Reason
		}
		if protocolCapabilityFallback {
			// This is a bounded pre-mutation capability fallback, not a replay
			// of an ambiguous worker attempt. It is permitted even when the
			// normal task recovery policy is manual because the evidence proves
			// no mutating tool ran.
			protocolCapabilityRetryUsed = true
			maxAttempts = max(maxAttempts, attempt+1)
			conversationHistory = nil
			if next := nextStrongerModel(c.modelList, resolvedModel); next != "" {
				if _, providerErr := c.ModelRuntime().ProviderFor(next); providerErr == nil {
					protocolFallbackModel = next
				}
			}
			disposition = RetryWorker
			reason = "model missed submit_result after a provably read-only attempt; retrying once with a clean tool context"
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
		priorSubmittedResult := receipt.SubmittedResult
		if priorSubmittedResult != nil {
			priorPartialOutput = retryPartialOutput(priorSubmittedResult.FormatForContext(), priorPartialOutput)
		}

		// Build conversation history for a potential retry. The one bounded
		// result-contract capability fallback is deliberately a clean attempt:
		// it may not inherit old tool messages that invite the model to repeat
		// tools unavailable in its repair turn.
		if receipt.SubmittedResult != nil {
			// A semantic incomplete handoff is a finalization retry, not a
			// continuation of the old tool conversation. The typed evidence is
			// injected explicitly below, so clearing history prevents the model
			// from blindly repeating a prior inspection sequence.
			conversationHistory = nil
		} else if !protocolCapabilityFallback {
			if len(conversationHistory) == 0 && len(steps) > 0 {
				conversationHistory = append(conversationHistory, fantasy.NewUserMessage(currentPrompt))
			}
			for _, step := range steps {
				conversationHistory = append(conversationHistory, step.Messages...)
			}
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
			if isUnfixableVerifyFailure(err) && attempt < maxAttempts {
				c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("stopping retries: attempt %d hit a verify command that cannot be fixed by retrying (wrong exit-code polarity)", attempt)).withTodoID(todoID))
				c.PersistFailureWithClass(agentName, taskDesc, todoID, c.FailureDetail(fmt.Errorf("verify command has unfixable wrong polarity after %d attempt(s): %w", attempt, err), "error"), ReplanRequired, currentClass)
			} else if prevErr != nil && sameFailure(prevErr.Error(), err.Error()) && attempt < maxAttempts {
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
			lastSubmittedResult = priorSubmittedResult
			if protocolCapabilityFallback {
				lastPolicyDeniedDispositions = attemptDispositions.snapshot()
			} else {
				lastPolicyDeniedDispositions = nil
			}
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
	if maxAttempts > 1 {
		c.persistReflexionLessonAsync(agentName, todoID, task.Goal, lastErr.Error(), appliedHint, false, isUnfixableVerifyFailure(lastErr))
	}
	failErr := fmt.Errorf("agent %q failed after %d attempt(s) (model: %s): %w", agentName, attemptsMade, resolvedModel, lastErr)
	if strings.TrimSpace(lastOutput) != "" {
		failErr = fmt.Errorf("%w\n\nLast agent output before failure (may contain useful findings):\n%s", failErr, utils.TruncateRunes(lastOutput, 2000))
	}
	return "", failErr
}

func formatRuntimeWorkflowContext(phase Phase, execCtx ExecutionContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- **Phase**: `%s`\n", phase)
	fmt.Fprintf(&b, "- **Runtime Workspace**: `%s`\n", execCtx.RuntimeWorkspace.Root)
	fmt.Fprintf(&b, "- **Artifacts Directory (output staging only)**: `%s/artifacts`\n", execCtx.RuntimeWorkspace.Root)
	fmt.Fprintf(&b, "- **Receipts Directory**: `%s/receipts`\n", execCtx.RuntimeWorkspace.Root)
	b.WriteString(runtimeArtifactStorageGuidance(execCtx.RuntimeWorkspace.Root))
	b.WriteByte('\n')
	if len(execCtx.Capabilities.Required) > 0 {
		fmt.Fprintf(&b, "- **Required Capabilities**: `%s`\n", strings.Join(execCtx.Capabilities.Required, ", "))
	}
	return b.String()
}

// appendWorksetWorkerRuntimeContext projects only the child task's assigned
// inputs. The source manifest and its lineage identity authorize expansion in
// the coordinator, but are not a worker capability and must stay private to
// prevent the model from guessing an unauthorized artifact reference.
func appendWorksetWorkerRuntimeContext(runtimeContext string, binding *WorksetBinding) string {
	if binding == nil {
		return runtimeContext
	}
	runtimeContext += fmt.Sprintf("- **Workset item**: `%s` (workset `%s`)\n", binding.ItemKey, binding.WorksetID)
	if len(binding.Inputs) == 0 {
		return runtimeContext
	}
	runtimeContext += "- **Assigned input artifacts** (opaque `artifact_ref` values; pass each ID unchanged to `view.artifact_ref`, never as `file_path`):\n"
	for _, input := range binding.Inputs {
		runtimeContext += fmt.Sprintf("  - `%s` (sha256 `%s`)\n", input.ID, input.SHA256)
	}
	return runtimeContext
}

func (c *Coordinator) effectiveWorkerMaxAttempts(agentDef *agent.AgentDef) int {
	retries := c.session.Config.MaxRetries
	if agentDef != nil && agentDef.MaxRetries >= 0 {
		retries = agentDef.MaxRetries
	}
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.policies.FailFast {
		return 1
	}
	if retries < 0 {
		retries = 0
	}
	// max-retries is a retry budget, not a total-attempt budget: a value of
	// one means the initial attempt plus one replay when recovery permits it.
	return retries + 1
}

// promoteValidatedReadOnlyHandoff turns a complete final assistant message
// into a typed result only at the protocol boundary. It is deliberately
// limited to callers that already proved every worker tool was read-only; a
// mutating task must never get a success result merely because its prose looks
// complete. validateTaskOutput enforces the task-declared scope/evidence
// contract, including literal review range, batch, and required sections.
func promoteValidatedReadOnlyHandoff(task TaskDef, todoID, agentName, output string) *TaskResult {
	if strings.TrimSpace(output) == "" || validateTaskOutput(task, output) != nil {
		return nil
	}
	result := ParseFreeTextResult(output)
	result.TaskID = todoID
	result.Agent = agentName
	result.Status = TaskResultStatusSuccess
	result.Source = "promoted_free_text"
	result.Confidence = 0.75
	result.Summary = "Validated read-only Markdown handoff promoted by runtime."
	result.Details = output
	return result
}

// executeRuntimeAction gives provider-backed actions the same durable TODO
// lifecycle as worker tasks. In particular, a successful provider response
// must mark its static contract done so the phase state machine can advance.
func coordinatorRuntimeRunID(c *Coordinator) string {
	if c == nil {
		return "run-unknown"
	}
	if runID := strings.TrimSpace(c.executionRunID); runID != "" {
		return runID
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		if runID := strings.TrimSpace(c.taskTracker.TodoList().RunID()); runID != "" {
			return runID
		}
	}
	return "run-unknown"
}

func (c *Coordinator) allocateRuntimeActionWorkspace(todoID string, startedAt time.Time) (string, string, error) {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return "", "", fmt.Errorf("action invocation requires a workspace")
	}
	if c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return "", "", fmt.Errorf("action invocation requires an enabled runtime workflow")
	}
	runID := coordinatorRuntimeRunID(c)
	runName := safeNameRegex.ReplaceAllString(runID, "-")
	taskName := safeNameRegex.ReplaceAllString(strings.TrimSpace(todoID), "-")
	if taskName == "" {
		taskName = "unknown"
	}
	parent, err := c.phaseWorkflow.executionContext().RuntimeWorkspace.Resolve(filepath.Join("runs", runName, "actions"))
	if err != nil {
		return "", "", fmt.Errorf("resolve action staging parent: %w", err)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", "", fmt.Errorf("create action staging parent: %w", err)
	}
	for range 8 {
		seq := actionWorkspaceSeq.Add(1)
		actionID := fmt.Sprintf("structured-action-%s-%d-%d", taskName, startedAt.UnixNano(), seq)
		actionRoot := filepath.Join(parent, actionID)
		if err := os.Mkdir(actionRoot, 0o755); err == nil {
			return actionID, actionRoot, nil
		} else if !os.IsExist(err) {
			return "", "", fmt.Errorf("create action staging directory: %w", err)
		}
	}
	return "", "", fmt.Errorf("allocate unique action staging directory")
}

func (c *Coordinator) executeRuntimeAction(ctx context.Context, task TaskDef, todoID string) (string, error) {
	startedAt := time.Now().UTC()
	actionID, actionRoot, err := c.allocateRuntimeActionWorkspace(todoID, startedAt)
	if err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, "", "failure", startedAt, time.Now().UTC(), "", err)
		return "", err
	}
	attempt := c.currentTaskAttempt(todoID) + 1
	c.setCurrentTaskAttempt(todoID, attempt)
	c.emitRuntimeActionEvent("action_started", task, todoID, actionID, "started", startedAt, time.Time{}, "", nil)
	if err := c.taskTracker.TodoList().SetRuntimeError(todoID, nil); err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("clear structured action error: %w", err)
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskInProgress, "executing structured action", "", nil); err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("mark structured action in progress: %w", err)
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	actionEnv := ActionEnvironment{
		Workspace: actionRoot, Repository: c.projectDir, RunID: coordinatorRuntimeRunID(c),
		TaskID: todoID, Attempt: attempt, ActionInvocationID: actionID,
	}
	if c.session != nil {
		actionEnv.TeamName = c.session.Config.Name
	}
	actionCtx := WithActionEnvironment(ctx, actionEnv)
	rawResult, err := c.phaseWorkflow.executeActionValueForTask(actionCtx, *task.Action, string(task.SideEffect))
	if err != nil {
		runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
		_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
		c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", err
	}
	actionResult, err := decodeActionResult(rawResult)
	if err != nil {
		runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
		_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
		c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", err
	}
	declaredArtifacts := append([]ArtifactRef(nil), actionResult.Artifacts...)
	providerArtifacts, err := c.ingestActionProviderArtifacts(ctx, actionRoot, task, todoID, attempt, declaredArtifacts)
	if err != nil {
		runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
		_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
		c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", err
	}
	actionResult.Artifacts = providerArtifacts
	if err := c.publishRuntimeWorksetProjection(actionRoot, actionID, declaredArtifacts, providerArtifacts); err != nil {
		runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
		_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
		c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", err
	}
	output := actionResultDisplay(rawResult, actionResult)
	if task.Verify != "" || task.VerifySpec != nil {
		if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskVerifying, "running objective verification", output, nil); err != nil {
			c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
			return "", fmt.Errorf("enter structured action verification: %w", err)
		}
		verification, verifyErr := c.verifyTaskDeliverableWithSpec(ctx, nil, task, nil)
		if verification != nil {
			_ = c.taskTracker.TodoList().SetVerificationResult(todoID, verification)
		}
		if verifyErr != nil {
			err := fmt.Errorf("structured action verification failed: %w", verifyErr)
			c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
			c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
			return "", err
		}
	}
	typedResult := &TaskResult{
		TaskID: todoID, Agent: task.Agent, Attempt: attempt, Status: TaskResultStatusSuccess,
		Summary: output, Details: output, Source: "runtime", Artifacts: providerArtifacts,
		Facts: actionResult.Outputs, Confidence: 1,
	}
	c.storeSubmittedTaskResult(todoID, typedResult)
	if item := c.todoItemByID(todoID); item != nil {
		c.emitArtifactEvents(item)
	}
	if err := c.persistSuccessfulCoordinatorTaskReceipt(todoID, task.Agent, attempt, startedAt, output); err != nil {
		runtimeErr := c.phaseWorkflow.actionExecutionError(task, err)
		_ = c.taskTracker.TodoList().SetRuntimeError(todoID, &runtimeErr)
		c.PersistFailure(task.Agent, task.Goal, todoID, c.FailureDetail(err, FailureSourceError))
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("persist structured action receipt: %w", err)
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output, nil); err != nil {
		c.emitRuntimeActionEvent("action_failed", task, todoID, actionID, "failure", startedAt, time.Now().UTC(), "", err)
		return "", fmt.Errorf("mark structured action done: %w", err)
	}
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("done").withAgent(task.Agent).withOutput(output).withMessage("structured action completed").withTodoID(todoID))
	c.emitRuntimeActionEvent("action_completed", task, todoID, actionID, "success", startedAt, time.Now().UTC(), output, nil, providerArtifacts)
	return output, nil
}

func actionResultDisplay(raw interface{}, result ActionResult) string {
	if raw == nil {
		return "structured action completed"
	}
	if text, ok := raw.(string); ok {
		return text
	}
	return encodeActionResult(result)
}

func (c *Coordinator) ingestActionProviderArtifacts(ctx context.Context, actionRoot string, task TaskDef, todoID string, attempt int, declared []ArtifactRef) ([]ArtifactRef, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return nil, fmt.Errorf("action artifacts require a workspace")
	}
	if strings.TrimSpace(actionRoot) == "" {
		return nil, fmt.Errorf("action artifacts require an invocation workspace")
	}
	workspaceRoot, err := filepath.Abs(c.session.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve session workspace: %w", err)
	}
	workspaceRoot, err = filepath.EvalSymlinks(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve session workspace: %w", err)
	}
	actionRoot, err = filepath.Abs(actionRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve action invocation workspace: %w", err)
	}
	actionRoot, err = filepath.EvalSymlinks(actionRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve action invocation workspace: %w", err)
	}
	actionRel, err := filepath.Rel(workspaceRoot, actionRoot)
	if err != nil || actionRel == ".." || strings.HasPrefix(actionRel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("action invocation workspace escapes session workspace")
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil, err
	}
	type pendingArtifact struct {
		request PutArtifactRequest
	}
	pending := make([]pendingArtifact, 0, len(declared))
	providerName := ""
	if c.phaseWorkflow != nil && task.Action != nil {
		providerName = c.phaseWorkflow.providerName(normalizeCapability(task.Action.Capability))
	}
	for index, artifact := range declared {
		if strings.TrimSpace(artifact.Path) == "" {
			return nil, fmt.Errorf("action artifacts[%d] path is required", index)
		}
		resolved, resolveErr := resolveArtifactSourcePath(actionRoot, artifact.Path)
		if resolveErr != nil {
			return nil, fmt.Errorf("action artifacts[%d] %q: %w", index, artifact.Path, resolveErr)
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil {
			return nil, fmt.Errorf("action artifacts[%d] %q: %w", index, artifact.Path, statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("action artifacts[%d] %q must be a regular file", index, artifact.Path)
		}
		file, openErr := os.Open(resolved)
		if openErr != nil {
			return nil, fmt.Errorf("action artifacts[%d] %q: %w", index, artifact.Path, openErr)
		}
		content, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("action artifacts[%d] %q: %w", index, artifact.Path, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("action artifacts[%d] %q: %w", index, artifact.Path, closeErr)
		}
		relative, relErr := filepath.Rel(workspaceRoot, resolved)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("action artifact %q escapes workspace", artifact.Path)
		}
		pending = append(pending, pendingArtifact{request: PutArtifactRequest{
			Content: content, Kind: artifact.Kind, Role: artifact.Role, Path: filepath.ToSlash(relative), Description: artifact.Description,
			MediaType: artifact.MediaType, RunID: coordinatorRuntimeRunID(c), TaskID: todoID, Agent: task.Agent,
			Provider: providerName, Attempt: attempt,
		}})
	}
	refs := make([]ArtifactRef, 0, len(pending))
	for _, item := range pending {
		putResult, putErr := store.Put(ctx, item.request)
		if putErr != nil {
			return nil, putErr
		}
		if verifyErr := store.Verify(ctx, putResult.ArtifactRef); verifyErr != nil {
			return nil, verifyErr
		}
		refs = append(refs, putResult.ArtifactRef)
	}
	return refs, nil
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

func (c *Coordinator) emitRuntimeActionEvent(eventType string, task TaskDef, todoID, actionID, status string, startedAt, finishedAt time.Time, output string, actionErr error, providerArtifacts ...[]ArtifactRef) {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || task.Action == nil {
		return
	}
	if strings.TrimSpace(actionID) == "" {
		actionID = "structured-action-" + safeNameRegex.ReplaceAllString(todoID, "-") + "-" + fmt.Sprintf("%d", startedAt.UnixNano())
		if strings.TrimSpace(todoID) == "" {
			actionID = "structured-action-unknown-" + fmt.Sprintf("%d", startedAt.UnixNano())
		}
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
			Version: 1, RunID: coordinatorRuntimeRunID(c), TaskID: todoID, Agent: task.Agent, ActionID: actionID,
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
	for _, artifacts := range providerArtifacts {
		refs = append(refs, artifacts...)
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
		if prior.Success && prior.SubmittedResult != nil && validateCompletedTaskResult(prior.SubmittedResult) == nil {
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
	if item.TypedResult != nil && item.TypedResult.Source == "submitted" && validateCompletedTaskResult(item.TypedResult) == nil {
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
		if provenance.Success && provenance.SubmittedResult != nil && validateCompletedTaskResult(provenance.SubmittedResult) == nil {
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
	repairCtx = context.WithValue(repairCtx, protocolRepairExecutionKey{}, true)
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
		c.setCurrentTaskAttempt(item.ID, attempt)
		attemptCtx := context.WithValue(repairCtx, executionAttemptKey{}, attempt)
		if identity, ok := c.activeTaskResultOccurrence(item.ID); ok {
			attemptCtx = withSubmitResultRuntimeIdentity(attemptCtx, identity)
		}
		if attempt > priorAttempts+1 {
			repairPrompt = fmt.Sprintf("## Goal\n%s\n\n## Schema-only repair\nThe previous result-only repair call did not match the submit_result schema. This is the final repair attempt. Call submit_result exactly once with corrected schema and preserve the execution facts below. Do NOT execute work, inspect files, call any other tool, or emit a prose final response. The call must include both required fields: `status` (one of `success`, `completed_with_gaps`, `partial`, `failed`, or `blocked`) and a non-empty `summary`; put any complete textual deliverable in `details`.\n\n## Execution Output\n%s", task.Goal, output)
		}
		preparedPrompt, prepareErr := c.prepareAuxiliaryPrompt(repairCtx, "result_repair", repairPrompt)
		if prepareErr != nil {
			runErr = prepareErr
			break
		}
		_, steps, callErr := c.runAgentWithStatusAndHistory(attemptCtx, repairAgent, agentName, preparedPrompt, nil, timing, fantasy.StepCountIs(1))
		runErr = callErr
		typedRes = c.GetTaskResult(item.ID)
		repairReason, _ = classifyRepairFailure(steps, typedRes)
		repairSuccess = typedRes != nil && typedRes.Source == "submitted" && validateCompletedTaskResult(typedRes) == nil
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
		verification, err := c.verifyTaskDeliverableWithSpecAndResult(ctx, nil, task, nil, result)
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

// freeTextResultNeedsSummary recognizes the only outputs the opt-in free-text
// compatibility mode may repair: blank output and a short, unfinished
// narration. It intentionally does not soften normal completion validation.
func freeTextResultNeedsSummary(task TaskDef, output string) bool {
	return validateTaskOutput(task, output) != nil
}

func incompleteReadOnlyReviewSummary(stepCount int) string {
	return fmt.Sprintf("Task evidence is incomplete: the read-only worker used %d inspection step(s) but the provider returned no final report. The task transcript preserves the inspected commands and outputs; treat this task as inconclusive rather than as a successful result.", stepCount)
}

// rescueFinalSummary gives an agent that stopped without a final message one
// genuinely tool-free turn to summarize what it did.  It deliberately does
// not replay the full step history: a large diff can consume the entire
// context window before the model sees the final-report instruction, and old
// tool-call messages can tempt it to call tools that this rescue does not
// expose.  The compact tool inventory preserves an honest coverage boundary
// without turning the recovery turn into another inspection round.
func (c *Coordinator) rescueFinalSummary(ctx context.Context, ag fantasy.Agent, agentName string, agentDef *agent.AgentDef, resolvedModel string, task TaskDef, steps []fantasy.StepResult, timing *taskTiming) string {
	if ctx.Err() != nil {
		return ""
	}
	// Rebuild the agent with no tools. Merely telling the original agent not to
	// call tools is insufficient: a model that hit its step limit often makes
	// another tool call instead of writing the requested summary.
	if c.workerAgentOverride == nil {
		provider, providerErr := c.ModelRuntime().ProviderFor(resolvedModel)
		if providerErr != nil || agentDef == nil {
			return ""
		}
		def := c.injectWorkerContext(ctx, agentDef)
		rescueAgent, createErr := c.createGatedAgent(ctx, provider, agent.AgentConfig{
			Def:        def,
			TeamConfig: &c.session.Config,
			WorkDir:    c.projectDir,
			MaxSteps:   1,
		}, nil)
		if createErr != nil {
			return ""
		}
		ag = rescueAgent
	}
	// The rescue stream has no separate execution receipt; account its usage
	// directly in the no-progress budget.
	ctx = context.WithValue(ctx, llmUsageReceiptExpectedKey{}, false)
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{})
	c.report(c.newEvent("step").withAgent(agentName).withMessage("agent stopped without a final message; requesting a summary turn"))
	rescuePrompt, prepareErr := c.prepareAuxiliaryPrompt(ctx, "final_summary_repair", buildRescueFinalSummaryInstruction(task))
	if prepareErr != nil {
		return ""
	}
	rescuePrompt += freeTextFinalSummaryEvidence(steps)
	if transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript); transcript != nil {
		if excerpt := transcript.CompactEvidence(8, 500); excerpt != "" {
			rescuePrompt += "\n## Bounded transcript excerpts\nUse these as concrete evidence where applicable; do not invent details beyond them.\n" + excerpt
		}
	}
	summary, _, err := c.runAgentWithStatusAndHistory(ctx, ag, agentName,
		rescuePrompt,
		nil, timing, fantasy.StepCountIs(1))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(summary)
}

// buildRescueFinalSummaryInstruction preserves the task contract when a
// worker reaches its step budget and is moved to the tool-free rescue turn.
// The normal conversation may have been compacted by then; omitting this
// contract makes the rescue model unable to identify the assigned batch or
// literal range and it will (correctly) report that its evidence is unusable.
func buildRescueFinalSummaryInstruction(task TaskDef) string {
	var b strings.Builder
	b.WriteString("You stopped before writing a final message. Do NOT call any tools. ")
	b.WriteString("Write the complete Markdown final report now. Preserve literal range, batch ID, artifact paths, and other task-specific identifiers from the assigned contract. State only findings you can support from the evidence already collected; otherwise explicitly state the coverage limit. Include what you inspected, what you found, and what remains to be done.\n\n")
	b.WriteString("## Authoritative assigned task contract\n")
	b.WriteString("The following goal and constraints are the original assignment and must be retained verbatim in the handoff:\n")
	b.WriteString("Goal: ")
	b.WriteString(strings.TrimSpace(task.Goal))
	b.WriteByte('\n')
	if constraints := strings.TrimSpace(task.Constraints); constraints != "" {
		b.WriteString("Constraints: ")
		b.WriteString(constraints)
		b.WriteByte('\n')
	}
	b.WriteString("Respect any output requirements declared by that contract. Do not replace task-specific details with a generic summary.\n")
	return b.String()
}

func freeTextFinalSummaryEvidence(steps []fantasy.StepResult) string {
	var names []string
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			if name := strings.TrimSpace(call.ToolName); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 {
		return "\n\n## Bounded execution evidence\nNo tool calls were recorded. Do not claim that the assigned review was completed.\n"
	}
	return "\n\n## Bounded execution evidence\nThe earlier read-only attempt used these tools: " + strings.Join(names, ", ") + ". Raw calls and outputs remain in the task transcript; they are not available tools in this final response.\n"
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
		if manifestErr := c.recordAuxiliaryFallback(ctx, "sidecar_task", "normal_worker_fallback"); manifestErr != nil {
			return "", manifestErr
		}
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

	result, err := s.Execute(sidecar.WithPurpose(sidecarCtx, "sidecar_task"), taskDesc)
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
		verification, verifyErr := c.verifyTaskDeliverableWithSpec(ctx, nil, task, nil)
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

// workerStepBudgetKey carries the declared tool-step ceiling into the common
// stream hook. It is only installed for normal worker attempts; coordinator
// and result-only repair streams keep their existing explicit tool contracts.
type workerStepBudgetKey struct{}

type workerStepBudgetTerminalOnlyKey struct{}

// workerFreeTextFinalizationKey marks the narrow compatibility mode in which
// a read-only worker is expected to finish in prose rather than submit a typed
// result.  It is set only after allowsFreeTextWorkerResult verifies both the
// team opt-in and side_effect:none boundary.
type workerFreeTextFinalizationKey struct{}

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

	allowed := append([]string(nil), exposedToolNames...)
	if def != nil {
		// Agent-specific MCP tools are a supported frontmatter grant. Keep
		// the display/tool-call name for the stream gate and add the canonical
		// agent:tool name for the transport authorizer.
		mcpAllowed := c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || c.phaseWorkflow.State() == PhaseExecute
		if mcpAllowed {
			for name := range def.MCPTools {
				name = strings.TrimSpace(name)
				if name == "" || !slices.Contains(exposedToolNames, name) {
					continue
				}
				allowed = append(allowed, strings.ToLower(strings.TrimSpace(def.Name))+":"+name)
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
			for _, name := range allowed {
				if !mcpNames[name] {
					filtered = append(filtered, name)
				}
			}
			allowed = filtered
		}
	}
	// Runtime authorization is derived from the final concrete model surface.
	// Empty and "all" declarations are selection inputs, never hidden grants.
	allowed = c.filterDeniedToolNamesWithGrants(allowed, c.taskToolGrants(def, task))
	// The concrete model surface is authoritative for compatibility aliases.
	// Remove hidden aliases from the declared union so a forged call cannot
	// invoke a default-disabled implementation through the stream gate.
	exposed := make(map[string]bool, len(exposedToolNames))
	for _, name := range exposedToolNames {
		exposed[strings.TrimSpace(name)] = true
	}
	filtered := allowed[:0]
	for _, name := range allowed {
		if isLegacyMemoryMutationTool(name) && !exposed[name] {
			continue
		}
		filtered = append(filtered, name)
	}
	return context.WithValue(ctx, tools.AgentToolsAllowedKey, dedupeToolNames(filtered))
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
	acceptedTerminalResult := &acceptedTerminalResultStop{}
	ctx = context.WithValue(ctx, acceptedTerminalResultStopKey{}, acceptedTerminalResult)
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
	var tokenStateMu sync.Mutex
	var tokenSteps []*tokenStepAdmission
	var activeTokenStep *tokenStepAdmission
	newTokenStepSettlement := func(requestTokens int64) *tokenStepAdmission {
		if requestTokens <= 0 {
			requestTokens = 1
		}
		// This path runs only after the provider has already returned usage.
		// Settlement must not attempt a second admission: admission belongs to
		// PrepareStep, while observed usage is always charged, including when it
		// takes the ledger beyond the configured cap.
		admission := &tokenStepAdmission{requestTokens: requestTokens}
		tokenStateMu.Lock()
		tokenSteps = append(tokenSteps, admission)
		tokenStateMu.Unlock()
		return admission
	}

	var loopDetectMu sync.Mutex
	var lastToolCall *lastToolCallEntry
	normalizeUsageTokens := func(usage fantasy.Usage) int64 {
		if usage.TotalTokens > 0 {
			return usage.TotalTokens
		}
		return usage.InputTokens + usage.OutputTokens
	}
	var observedStreamUsageMu sync.Mutex
	var observedStreamUsage int64
	var directStreamUsageRecorded bool
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
	var recoveryMu sync.Mutex
	pendingRecovery := ""
	tp := &ThinkParser{}
	textRepetitionDetector := NewStreamRepetitionDetector()
	reasoningRepetitionDetector := NewStreamRepetitionDetector()
	streamPreflight := coordinatorRequestPreflightFromContext(ctx)
	var contextManager *ContextWindowManager
	coordinatorFallbackUsed := false
	coordinatorFallbackAttempted := false
	if streamPreflight != nil {
		contextManager = NewContextWindowManagerWithPredecessor(defaultCounter, func(compactCtx context.Context, messages []fantasy.Message, predecessor *StructuredSummary) ([]fantasy.Message, *StructuredSummary, error) {
			counts := make([]int, len(messages))
			for i := range counts {
				counts[i] = 1
			}
			projection := c.buildTransientCompactionProjection(compactCtx, messages, 0, counts, predecessor)
			return projection.messages, projection.summary, nil
		})
	}

	stopWhen := append([]fantasy.StopCondition(nil), extraStop...)
	stopWhen = append(stopWhen, func([]fantasy.StepResult) bool {
		return acceptedTerminalResult.isAcceptedFor(c, todoID)
	})
	streamCall := fantasy.AgentStreamCall{
		Prompt:   prompt,
		Messages: history,
		StopWhen: stopWhen,
		// Fantasy's streaming loop reads this field directly, not the
		// agent-level default set via fantasy.WithRepairToolCall in
		// agent.CreateAgent — see internal/agent/toolcall_repair.go.
		RepairToolCall: agent.RepairConcatenatedToolCall,
		PrepareStep: func(ctx context.Context, opts fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
			// Keep this request within the context model token budget.
			modelID := ""
			if opts.Model != nil {
				modelID = opts.Model.Model()
			}
			spec := globalRegistry.GetSpec(modelID).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(c.agentDefByName(agentName)))
			budget := c.ContextCompiler().CalculateBudget(spec, 0, 0)
			preparedMessages := opts.Messages
			stepBudget, hasStepBudget := ctx.Value(workerStepBudgetKey{}).(int)
			freeTextFinalization, _ := ctx.Value(workerFreeTextFinalizationKey{}).(bool)
			stepBudgetCheckpoint := ""
			terminalOnly := false
			if hasStepBudget && stepBudget > 0 {
				remaining := stepBudget - opts.StepNumber
				// Reserve the same final 20% window used for the checkpoint as a
				// genuine finalization grace period.  A three-step tail on a
				// forty-step review is too small for a model to stop exploring,
				// consolidate evidence, and emit a Markdown handoff; in practice
				// workers reached the cap and rescue lost the task contract.
				checkpointAt := max(1, (stepBudget+4)/5)
				wrapUpAt := checkpointAt
				if remaining == checkpointAt {
					if freeTextFinalization {
						stepBudgetCheckpoint = "\n\n## Step Budget Checkpoint\nYou have reached the final 20% of the tool-step budget. Stop expanding exploration, consolidate the evidence you already have, and prepare your complete Markdown final response.\n"
					} else {
						stepBudgetCheckpoint = "\n\n## Step Budget Checkpoint\nYou have reached the final 20% of the tool-step budget. Stop expanding exploration, consolidate the evidence you already have, and prepare an accurate submit_result.\n"
					}
				}
				if remaining <= wrapUpAt {
					terminalOnly = true
					if freeTextFinalization {
						stepBudgetCheckpoint = "\n\n## Step Budget Wrap-up\nReason code: step_budget_wrap_up. Do not make any new inspection calls. Write your complete Markdown final response now using the evidence already collected.\n"
					} else {
						stepBudgetCheckpoint = "\n\n## Step Budget Wrap-up\nReason code: step_budget_wrap_up. Do not make any new inspection calls. Call submit_result now with the evidence already collected.\n"
					}
				}
			}
			if stepBudgetCheckpoint != "" {
				preparedMessages = append(append([]fantasy.Message(nil), preparedMessages...), fantasy.NewUserMessage(stepBudgetCheckpoint))
			}
			recoveryMu.Lock()
			if pendingRecovery != "" {
				preparedMessages = append(append([]fantasy.Message(nil), preparedMessages...), fantasy.NewUserMessage(pendingRecovery))
				pendingRecovery = ""
			}
			recoveryMu.Unlock()
			var preflightSystem string
			var preflightTools []fantasy.AgentTool
			preflightApplied := false
			contextChanged := false
			if contextManager != nil {
				fullSystem, fullTools := streamPreflight.configuration()
				attempt, _ := ctx.Value(executionAttemptKey{}).(int)
				admission, admissionErr := c.admitCoordinatorContext(ctx, contextManager, ContextWindowRequest{
					ModelID:              modelID,
					System:               fullSystem,
					Tools:                fullTools,
					Messages:             preparedMessages,
					Prompt:               prompt,
					Window:               streamPreflight.windowValue(),
					ReservedOutputTokens: spec.MaxOutputTokens,
					SafetyMarginTokens:   spec.SafetyMarginTokens,
					StepNumber:           opts.StepNumber,
				}, "initial", todoID, attempt)
				if admissionErr != nil {
					return ctx, fantasy.PrepareStepResult{}, admissionErr
				}
				if admission.Candidate != nil {
					// A compacted candidate is explicitly typed by the admission
					// owner. It is not yet provider-safe, but it is the only
					// history that preflight may use to calculate optional tool
					// projection. Prompt-only CannotFit has no candidate and keeps
					// the original opts.Messages unchanged.
					preparedMessages = admission.Candidate.Messages
				} else if admission.Decision != ContextWindowCannotFit {
					preparedMessages = admission.Messages
				}
				contextChanged = !reflect.DeepEqual(preparedMessages, opts.Messages)

				var preflightErr error
				preflightSystem, preflightTools, preflightApplied, preflightErr = streamPreflight.prepare(ctx, preparedMessages, prompt, spec.MaxOutputTokens, opts.StepNumber)
				if preflightErr != nil {
					return ctx, fantasy.PrepareStepResult{}, preflightErr
				}
				requestSystem, requestTools := fullSystem, fullTools
				if preflightApplied {
					requestSystem, requestTools = preflightSystem, preflightTools
				}
				finalAdmission, finalErr := c.admitCoordinatorContext(ctx, contextManager, ContextWindowRequest{
					ModelID:              modelID,
					System:               requestSystem,
					Tools:                requestTools,
					Messages:             preparedMessages,
					Prompt:               prompt,
					Window:               streamPreflight.windowValue(),
					ReservedOutputTokens: spec.MaxOutputTokens,
					SafetyMarginTokens:   spec.SafetyMarginTokens,
					StepNumber:           opts.StepNumber,
				}, "final", todoID, attempt)
				if finalErr != nil {
					return ctx, fantasy.PrepareStepResult{}, finalErr
				}
				if finalAdmission.Decision == ContextWindowCannotFit {
					fitErr := &CannotFitError{ModelID: modelID, RequestTokens: finalAdmission.RequestTokens, Available: finalAdmission.Budget.Available, ProvenNoSend: true}
					if _, provenNoSend := isProvenPreProviderCannotFit(fitErr); finalAdmission.Candidate != nil && provenNoSend && opts.StepNumber == 0 && !coordinatorFallbackUsed && !coordinatorFallbackAttempted {
						coordinatorFallbackAttempted = true
						if fallback, fallbackErr := c.admitCoordinatorEarlierModel(ctx, streamPreflight, preparedMessages, prompt, opts.StepNumber, spec.MaxOutputTokens, modelID); fallbackErr != nil {
							return ctx, fantasy.PrepareStepResult{}, fallbackErr
						} else if fallback.Model != nil {
							coordinatorFallbackUsed = true
							preparedMessages = fallback.Messages
							result := fantasy.PrepareStepResult{Messages: preparedMessages, Model: fallback.Model}
							if fallback.ToolsSet {
								result.System = &fallback.System
								result.Tools = fallback.Tools
							}
							return fallback.Context, result, nil
						}
					}
					return ctx, fantasy.PrepareStepResult{}, fitErr
				}
				preparedMessages = finalAdmission.Messages
				contextChanged = contextChanged || !reflect.DeepEqual(preparedMessages, opts.Messages)
			} else {
				capResult := CapStepMessagesWithCounterResult(ctx, defaultCounter, modelID, preparedMessages, budget.Available)
				if capResult.StillOverBudget {
					return ctx, fantasy.PrepareStepResult{}, fmt.Errorf("step context window admission cannot fit request for model %q", modelID)
				}
				if capResult.Changed {
					preparedMessages = capResult.Messages
					contextChanged = true
				}
			}
			opts.Messages = preparedMessages
			if attemptTokens != nil {
				if err := attemptTokens.reserveContext(estimateStepRequestTokens(preparedMessages, prompt)); err != nil {
					return ctx, fantasy.PrepareStepResult{}, err
				}
			}
			requestTokens := estimateStepRequestTokens(preparedMessages, prompt)
			admissionTokens := requestTokens + int64(spec.MaxOutputTokens)
			if admissionTokens <= 0 {
				admissionTokens = 1
			}
			reserved, err := c.reserveTokenStep(admissionTokens)
			if err != nil {
				return ctx, fantasy.PrepareStepResult{}, err
			}
			admission := &tokenStepAdmission{reservation: reserved, requestTokens: requestTokens}
			tokenStateMu.Lock()
			tokenSteps = append(tokenSteps, admission)
			activeTokenStep = admission
			tokenStateMu.Unlock()
			// Log only requests that cleared the circuit breaker; a rejected
			// request was never sent to the provider and must not look like an
			// attempted model call in the task evidence.
			llmLogMu.Lock()
			loggedMsgs, lastReqBytes = llmLogRequest(logWrite, opts, preparedMessages, loggedMsgs)
			llmLogMu.Unlock()
			if contextChanged || terminalOnly || stepBudgetCheckpoint != "" || preflightApplied {
				result := fantasy.PrepareStepResult{Messages: preparedMessages}
				if preflightApplied {
					result.System = &preflightSystem
					result.Tools = preflightTools
				}
				if terminalOnly {
					if freeTextFinalization {
						// An empty active-tool set is intentional: it gives the model a
						// final text-only turn without advertising a result tool that
						// this compatibility mode does not expose.
						result.ActiveTools = []string{}
						c.report(c.newEvent("step").withAgent(agentName).withTodoID(todoID).withMessage("step_budget_wrap_up: final Markdown response only"))
					} else {
						ctx = context.WithValue(ctx, workerStepBudgetTerminalOnlyKey{}, true)
						result.ActiveTools = []string{submitResultToolName}
						c.report(c.newEvent("step").withAgent(agentName).withTodoID(todoID).withMessage("step_budget_wrap_up: terminal handoff tools only"))
					}
				}
				return ctx, result, nil
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

			// load_skill records usage only after its handler successfully
			// returns full instructions; counting it here would mark a denied or
			// malformed request as a completed load and double-count success.
			if skillName := c.extractSkillFromToolCall(tc.ToolName, tc.Input); skillName != "" && tc.ToolName != "load_skill" {
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
			if isErrResult && todoID != CoordTodoID {
				recovery, recoveryErr := c.prepareToolFailureRecovery(ctx, agentName, tr.ToolCallID, tr.ToolName, callInput)
				if recoveryErr != nil {
					return fmt.Errorf("prepare tool-failure recovery context: %w", recoveryErr)
				}
				if recovery != "" {
					recoveryMu.Lock()
					pendingRecovery = recovery
					recoveryMu.Unlock()
				}
			}
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
			// typed result for its bounded task. A coordinator normally cannot
			// continue after a direct tool error: delegation, completion, writes,
			// and unknown tools could leave cross-task state incomplete. A failed
			// read-only observation is the deliberately narrow exception. It has
			// no side effect and the model receives the error result, so it can
			// select the correct observation tool (for example ls after view was
			// given a directory) without restarting an otherwise successful run.
			if todoID == CoordTodoID && isErrResult {
				trimmedResult := strings.TrimSpace(resultPreview)
				if strings.Contains(trimmedResult, coordinatorPolicyRepairExhaustedPrefix) {
					return fmt.Errorf("%w: %s", errCoordinatorPolicyRepairExhausted, trimmedResult)
				}
				if c.coordinatorPolicyRepairPending.Load() ||
					isCoordinatorPolicyRepairResult(trimmedResult) ||
					strings.HasPrefix(trimmedResult, "Tool argument schema violation:") ||
					c.isInitialToolCorrectionResult(tr.ToolName, trimmedResult) ||
					isReadOnlyToolCall(tr.ToolName, callInput) {
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
			if err := textRepetitionDetector.Process(text); err != nil {
				reportFn(c.newEvent("error").withAgent(agentName).withTodoID(todoID).withMessage(err.Error()))
				return err
			}
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
			if err := reasoningRepetitionDetector.Process(text); err != nil {
				reportFn(c.newEvent("error").withAgent(agentName).withTodoID(todoID).withMessage(err.Error()))
				return err
			}
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
			totalTokens := normalizeUsageTokens(usage)
			tokenStateMu.Lock()
			admission := activeTokenStep
			if totalTokens <= 0 && admission != nil {
				totalTokens = admission.requestTokens
			}
			tokenStateMu.Unlock()
			if admission == nil {
				requestTokens := int64((reqBytes + 3) / 4)
				if requestTokens <= 0 {
					requestTokens = 1
				}
				admission = newTokenStepSettlement(requestTokens)
				if totalTokens <= 0 {
					totalTokens = admission.requestTokens
				}
				tokenStateMu.Lock()
				activeTokenStep = admission
				tokenStateMu.Unlock()
			}
			observedStreamUsageMu.Lock()
			if totalTokens > observedStreamUsage {
				observedStreamUsage = totalTokens
			}
			newDirectUsage := llmUsageNeedsDirectNoProgressAccounting(ctx) && !directStreamUsageRecorded
			if newDirectUsage {
				directStreamUsageRecorded = true
			}
			observedStreamUsageMu.Unlock()
			settled := c.commitTokenStep(&admission.reservation, totalTokens)
			if settled && newDirectUsage {
				c.recordNoProgressTokens(totalTokens)
			}
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

	stepFallbackTokens := func(admission *tokenStepAdmission, step fantasy.StepResult) int64 {
		if step.Usage.TotalTokens > 0 {
			return step.Usage.TotalTokens
		}
		if total := step.Usage.InputTokens + step.Usage.OutputTokens; total > 0 {
			return total
		}
		return admission.requestTokens
	}
	reconcileTokenStream := func(start int, streamResult *fantasy.AgentResult) (int64, error) {
		tokenStateMu.Lock()
		if start > len(tokenSteps) {
			start = len(tokenSteps)
		}
		admissions := append([]*tokenStepAdmission(nil), tokenSteps[start:]...)
		tokenStateMu.Unlock()

		var accounted int64
		if streamResult != nil {
			if len(streamResult.Steps) > 0 {
				for index, step := range streamResult.Steps {
					var admission *tokenStepAdmission
					if index < len(admissions) {
						admission = admissions[index]
					} else {
						requestTokens := estimateStepRequestTokens(step.Messages, "")
						// The result is already an observation from a provider call, so
						// settle it without a new reservation. Any admission was made
						// before the call by PrepareStep and is released by commit.
						admission = newTokenStepSettlement(requestTokens)
					}
					total := stepFallbackTokens(admission, step)
					if c.commitTokenStep(&admission.reservation, total) {
						accounted += total
					}
				}
			} else {
				var admission *tokenStepAdmission
				for _, candidate := range admissions {
					if !candidate.reservation.settled {
						admission = candidate
						break
					}
				}
				if admission == nil && len(admissions) == 0 {
					// Some custom agents return only the final response and do not
					// emit Steps or invoke OnStreamFinish. This result is already an
					// observation from a provider call, so charge it through a
					// synthetic settlement without attempting a new admission.
					admission = newTokenStepSettlement(1)
				}
				if admission != nil {
					total := normalizeUsageTokens(streamResult.TotalUsage)
					if total <= 0 {
						total = normalizeUsageTokens(streamResult.Response.Usage)
					}
					if total <= 0 {
						total = admission.requestTokens
					}
					if c.commitTokenStep(&admission.reservation, total) {
						accounted += total
					}
				}
			}
		}
		// A provider may fail or cancel after PrepareStep and before emitting a
		// finish part. Releasing every still-open admission here also retires the
		// failed stream before an independent context-overflow retry reserves its
		// own capacity.
		for _, admission := range admissions {
			c.releaseTokenStep(&admission.reservation)
		}
		return accounted, nil
	}
	var receiptResponseUsage int64
	recordResponseOnlyReceiptUsage := func(delta int64) {
		if delta <= 0 || todoID == "" || llmUsageNeedsDirectNoProgressAccounting(ctx) {
			return
		}
		attempt, _ := ctx.Value(executionAttemptKey{}).(int)
		if attempt < 1 {
			// Direct-agent invocations have a real execution receipt but do not
			// need the task-run attempt context for tool authorization.
			attempt = 1
		}
		receiptResponseUsage += delta
		// Keep this on the same cumulative receipt projection as the later
		// execution event. recordReliabilityUsage's per-attempt maximum makes
		// the response-only reconciliation idempotent if that event is emitted.
		c.recordReliabilityUsage(todoID, attempt, int(receiptResponseUsage))
	}
	runStream := func(call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		tokenStateMu.Lock()
		start := len(tokenSteps)
		activeTokenStep = nil
		tokenStateMu.Unlock()
		observedStreamUsageMu.Lock()
		observedStreamUsage = 0
		directStreamUsageRecorded = false
		observedStreamUsageMu.Unlock()
		streamResult, streamErr := ag.Stream(ctx, call)
		accounted, reconcileErr := reconcileTokenStream(start, streamResult)
		if llmUsageNeedsDirectNoProgressAccounting(ctx) {
			if accounted > 0 {
				c.recordNoProgressTokens(accounted)
			}
		} else if streamResult == nil || len(streamResult.Steps) == 0 {
			observedStreamUsageMu.Lock()
			observed := observedStreamUsage
			observedStreamUsageMu.Unlock()
			if observed <= 0 {
				observed = accounted
			}
			if observed <= 0 && streamResult != nil {
				observed = normalizeUsageTokens(streamResult.TotalUsage)
				if observed <= 0 {
					observed = normalizeUsageTokens(streamResult.Response.Usage)
				}
			}
			recordResponseOnlyReceiptUsage(observed)
		}
		if streamErr == nil {
			streamErr = reconcileErr
		}
		return streamResult, streamErr
	}

	result, err := runStream(streamCall)
	if IsContextOverflowError(err) {
		modelID, _ := ctx.Value(modelKey{}).(string)
		if observedWindow, ok := ParseObservedContextWindow(err); ok {
			GlobalModelSpecRegistry().RegisterObservedContextWindow(modelID, observedWindow)
			if preflight := coordinatorRequestPreflightFromContext(ctx); preflight != nil {
				preflight.observeWindow(observedWindow)
			}
			reportFn(c.newEvent("text").withAgent(agentName).withTodoID(todoID).withMessage(fmt.Sprintf("provider reported effective context window %d; future coordinator requests will use it for admission", observedWindow)))
		}
	}
	// A context overflow after Stream starts is terminal for this invocation.
	// The stream may already have emitted and executed a tool call, so replaying
	// the whole stream would duplicate an external side effect. Admission above
	// is the only repair boundary; provider feedback is recorded by the caller's
	// normal failure path and is never used to replay this stream.
	// A task that requires a result but stopped without a tool call has
	// stalled, not finished: Fantasy's loop correctly ends a turn once a step
	// requests no tools, but Hufu's protocol for this task still needs either
	// more evidence-gathering or a submit_result. Ask for exactly one bounded
	// continuation in the SAME turn -- cheap compared to the fresh attempt
	// (new transcript, redo every tool call) that protocol failure would
	// otherwise require. Gated on RequiresResult: a task that is happy with a
	// plain-text answer ending a turn with no tool call is simply done, not
	// stalled, and must never be nudged.
	requiresResult, _ := ctx.Value(taskRequiresResultKey{}).(bool)
	if err == nil && result != nil && todoID != "" && requiresResult && c.GetTaskResult(todoID) == nil && stalledWithoutToolCall(result.Steps) {
		// A worker that already exhausted its step budget is handled by the
		// existing wrap-up/finalization path (PrepareStep's stepBudgetCheckpoint
		// above); nudging here would just add steps beyond the budget it was
		// given instead of letting that mechanism run its course.
		stepBudget, hasStepBudget := ctx.Value(workerStepBudgetKey{}).(int)
		budgetExhausted := hasStepBudget && stepBudget > 0 && len(result.Steps) >= stepBudget
		if !budgetExhausted {
			reportFn(c.newEvent("text").withAgent(agentName).withTodoID(todoID).withMessage("model stopped without a tool call or submit_result on a task that requires one; requesting one same-turn continuation before falling back to a fresh attempt"))
			continuationMessages := append([]fantasy.Message(nil), history...)
			if strings.TrimSpace(prompt) != "" {
				continuationMessages = append(continuationMessages, fantasy.NewUserMessage(prompt))
			}
			for _, step := range result.Steps {
				continuationMessages = append(continuationMessages, step.Messages...)
			}
			continuationMessages = append(continuationMessages, fantasy.NewUserMessage(
				"You stopped without calling a tool or submit_result. If you are not finished, make your next tool call now. If you have gathered enough evidence, call submit_result now. Do not just describe what you plan to do next.",
			))
			nudgeCall := streamCall
			nudgeCall.Prompt = ""
			nudgeCall.Messages = continuationMessages
			nudgeCall.StopWhen = append(append([]fantasy.StopCondition(nil), streamCall.StopWhen...), fantasy.StepCountIs(2))
			if nudgeResult, nudgeErr := runStream(nudgeCall); nudgeErr == nil && nudgeResult != nil && len(nudgeResult.Steps) > 0 {
				result.Steps = append(result.Steps, nudgeResult.Steps...)
				last := nudgeResult.Steps[len(nudgeResult.Steps)-1]
				if strings.TrimSpace(nudgeResult.Response.Content.Text()) != "" || len(last.Content.ToolCalls()) > 0 {
					result.Response = nudgeResult.Response
				}
			}
		}
	}
	// Fantasy may surface the transport error from the proxy after the
	// invocation context has already been cancelled. The invocation cause is
	// authoritative; a connection reset is only a consequence of the hard
	// abort and must not become the provider failure classification.
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	modelID, _ := ctx.Value(modelKey{}).(string)
	err = annotateProviderModelFailure(err, modelID)
	c.SetCurrentStage("idle")
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
// granted is the final model-visible capability set for this invocation.
func toolUsageNotes(granted map[string]bool) string {
	has := func(name string) bool {
		return granted[name]
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

func (c *Coordinator) verifyTaskDeliverableWithSpec(parentCtx context.Context, agentDef *agent.AgentDef, task TaskDef, steps []fantasy.StepResult) (*VerificationResult, error) {
	return c.verifyTaskDeliverableWithSpecAndResult(parentCtx, agentDef, task, steps, nil)
}

func (c *Coordinator) verifyTaskDeliverableWithSpecAndResult(parentCtx context.Context, agentDef *agent.AgentDef, task TaskDef, steps []fantasy.StepResult, taskResult *TaskResult) (*VerificationResult, error) {
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
	if normalizedSpec.Type == VerifyWorksetComplete {
		verification, verifyErr := c.executeWorksetCompleteVerification(verifyCtx, normalizedSpec)
		if verification != nil {
			verification.Fingerprint = ComputeVerificationFingerprintFull(normalizedSpec, verification, workDir, "", c.verificationSecurityMode(shell))
		}
		return verification, verifyErr
	}
	return ExecuteVerificationSpecWithStepsAndTaskResult(verifyCtx, shell, workDir, normalizedSpec, steps, taskResult)
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
		// Workers must never edit the projection file directly; a direct
		// stm.md write would be discarded on the next projection rebuild.
		b.WriteString("- Return important findings, decisions, questions, artifacts, and verification in your structured result. The runtime captures shared memory automatically.\n")
		if granted["stm_write"] {
			// Composed prompt MUST still mention the deprecated typed-compat
			// alias when the worker holds an explicit grant, otherwise the
			// worker gets a runtime-authorized tool with no contract guidance
			// (HF-MEM5-003). Keep this sentence strictly additive: the
			// structured-result instruction above is the canonical contract.
			b.WriteString("- `stm_write` is a deprecated typed compatibility tool; rely on the structured result above for shared memory.\n")
		}
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
	contract := taskResultSubmissionContractForTask(task)
	info := submitResultToolInfo(contract)
	fields := make([]string, 0, len(info.Parameters))
	for field := range info.Parameters {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	fmt.Fprintf(b, "- Legal top-level submit_result fields for this task: `%s`. The runtime rejects fields outside this list.\n", strings.Join(fields, "`, `"))
	if len(info.Required) > 0 {
		required := append([]string(nil), info.Required...)
		slices.Sort(required)
		fmt.Fprintf(b, "- Required submit_result fields for this task: `%s`.\n", strings.Join(required, "`, `"))
	}
	if contract.FilesReadMinItems > 0 {
		fmt.Fprintf(b, "- A successful result must include `files_read` with at least %d object(s), each containing a non-empty `path`; use `files_read`, not evidence, for observed inputs.\n", contract.FilesReadMinItems)
	}
	if !contract.AllowEvidence {
		b.WriteString("- `evidence` is not a legal field for this task. Do not submit it; use the documented `files_read` entries instead.\n")
	}
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
		if reflection, err := s.Execute(sidecar.WithPurpose(reflectCtx, "reflection"), prompt); err == nil && strings.TrimSpace(reflection) != "" {
			return reflectionHeader + reflection
		}
	}
	_ = c.recordAuxiliaryFallback(ctx, "reflection", "deterministic_fallback")
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
// This becomes a required typed compiler fragment so budgeting and the
// content-free manifest describe exactly what the retry worker receives.
func buildRetryContext(class TaskFailureClass, lastErr error, transcriptRef, verifyCmd string, verifyExit int, workerExitCode *int, lastToolCall, lastToolInput, lastToolResult string, lastToolResultErr bool, lastOutput string, task TaskDef) string {
	return buildRetryContextWithSubmittedResult(class, lastErr, transcriptRef, verifyCmd, verifyExit, workerExitCode, lastToolCall, lastToolInput, lastToolResult, lastToolResultErr, lastOutput, nil, task)
}

// buildRetryContextWithSubmittedResult extends the generic retry context with
// the prior typed handoff when the worker reported an honest non-terminal
// state. The evidence is bounded and redacted, and is only available to the
// next attempt of the same Todo; it is never historical cross-run context.
func buildRetryContextWithSubmittedResult(class TaskFailureClass, lastErr error, transcriptRef, verifyCmd string, verifyExit int, workerExitCode *int, lastToolCall, lastToolInput, lastToolResult string, lastToolResultErr bool, lastOutput string, submitted *TaskResult, task TaskDef) string {
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
	appendSubmittedResultRetryEvidence(b, submitted)

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

// appendSubmittedResultRetryEvidence preserves the useful structured facts
// from an incomplete handoff without treating them as completion. Its bounded
// form is intentionally generic: any replayable read-only analysis can use it
// to finish missing evidence rather than repeat already-recorded work.
func appendSubmittedResultRetryEvidence(b *strings.Builder, result *TaskResult) {
	if b == nil || result == nil {
		return
	}
	b.WriteString("**Prior typed handoff (preserved evidence):**\n")
	fmt.Fprintf(b, "- status: %s\n", redactRetryText(result.Status, 80))
	if summary := redactRetryText(result.Summary, 500); summary != "" {
		fmt.Fprintf(b, "- summary: %s\n", summary)
	}
	if details := redactRetryText(result.Details, 1800); details != "" {
		fmt.Fprintf(b, "- details: %s\n", details)
	}
	if len(result.Findings) > 0 {
		b.WriteString("- findings:\n")
		for i, finding := range result.Findings {
			if i == 8 {
				b.WriteString("  - additional findings omitted from retry context\n")
				break
			}
			text := strings.TrimSpace(strings.Join([]string{finding.Category, finding.Summary, finding.Detail}, ": "))
			fmt.Fprintf(b, "  - %s\n", redactRetryText(text, 500))
		}
	}
	if len(result.FilesRead) > 0 {
		b.WriteString("- files_read:\n")
		for i, file := range result.FilesRead {
			if i == 16 {
				b.WriteString("  - additional files omitted from retry context\n")
				break
			}
			fmt.Fprintf(b, "  - %s (%s)\n", redactRetryText(file.Path, 500), redactRetryText(file.Purpose, 120))
		}
	}
	b.WriteString("- missing evidence: ")
	missing := make([]string, 0, len(result.OpenQuestions)+1)
	for _, question := range result.OpenQuestions {
		if question = redactRetryText(question, 400); question != "" {
			missing = append(missing, question)
		}
	}
	if hint := redactRetryText(result.RetryHint, 400); hint != "" {
		missing = append(missing, hint)
	}
	if len(missing) == 0 {
		b.WriteString("not explicitly reported; compare files_read with the assigned evidence contract.\n")
	} else {
		b.WriteString(strings.Join(missing, "; "))
		b.WriteString("\n")
	}
	b.WriteString("This is an evidence-aware finalization retry: retain verified facts, inspect only missing required evidence, then submit success or completed_with_gaps only when the full task contract is satisfied.\n")
}
