package team

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"os/exec"

	"encoding/json"
	"path/filepath"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

type runAgentsTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *runAgentsTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "agent",
		Description: "Delegate tasks to team workers. Runs all tasks in parallel. Returns structured results from each agent.",
		Parameters: map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"properties":           buildAgentTaskProperties(t.coordinator.workerNameList(), len(t.coordinator.modelList) > 0, filepath.Join(t.coordinator.session.Workspace, sharedDir)),
					"required":             []string{"agent"},
					"additionalProperties": false,
				},
			},
		},
		Required: []string{"tasks"},
	}
}

func (t *runAgentsTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Tasks []TaskDef `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if len(args.Tasks) == 0 {
		return fantasy.NewTextErrorResponse("no tasks provided"), nil
	}
	for _, t := range args.Tasks {
		if t.Goal == "" {
			return fantasy.NewTextErrorResponse("each task requires 'goal'"), nil
		}
	}

	result, err := t.coordinator.ExecuteTasks(ctx, args.Tasks)
	if err != nil {
		// Keep the per-task detail: formatTaskResults returns the full report
		// even on "all tasks failed", and discarding it left the coordinator
		// blind to why tasks failed, forcing guesswork re-delegation.
		if strings.TrimSpace(result) != "" {
			return fantasy.NewTextErrorResponse(result + "\n\nERROR: " + err.Error()), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(result), nil
}

type finishTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *finishTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "finish",
		Description: "Signal that you have completed the user's request and provide your final answer. Call this when you are done coordinating and have a complete response for the user. You MUST call this instead of just outputting text — your final answer goes in the response field. If worker tasks failed or were blocked, first fix them; only set acknowledge_failed_tasks when you must end with a clearly disclosed partial result.",
		Parameters: map[string]any{
			"response": map[string]any{
				"type":        "string",
				"description": "Your final answer to the user",
			},
			"acknowledge_failed_tasks": map[string]any{
				"type":        "boolean",
				"description": "Set true only when ending with unresolved failed or blocked tasks. The final response will include their IDs and errors.",
			},
		},
		Required: []string{"response"},
	}
}

func (t *finishTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Response               string `json:"response"`
		AcknowledgeFailedTasks bool   `json:"acknowledge_failed_tasks"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	todoList := t.coordinator.taskTracker.TodoList()
	failedTasks := failedTodoItems(todoList.Items())
	prof := t.coordinator.ExecutionProfile()

	if len(failedTasks) > 0 {
		if prof.RequireEvidenceManifest || prof.StrictPolicy {
			return fantasy.NewTextErrorResponse("RequireEvidenceManifest policy violation: cannot finish while worker tasks failed or were blocked:\n" + formatFailedTasks(failedTasks)), nil
		}
		if !args.AcknowledgeFailedTasks {
			return fantasy.NewTextErrorResponse("cannot finish successfully while worker tasks failed or were blocked:\n" + formatFailedTasks(failedTasks) + "\nFix or re-delegate these tasks. If the user needs a partial result, call finish again with acknowledge_failed_tasks:true; hufu will append the unresolved-task warning."), nil
		}
	}

	if t.coordinator.terminalSessionMgr != nil {
		// A resumed terminal can belong to a prior execution run. It remains an
		// unresolved resource, so a new run must not sidestep the gate by using
		// a new executionRunID.
		if err := t.coordinator.terminalSessionMgr.RequireNoLeaks(""); err != nil {
			return fantasy.NewTextErrorResponse("cannot finish while terminal sessions remain unresolved: " + err.Error()), nil
		}
	}

	completed := todoList.CompletedCount()
	failed := todoList.ErrorCount()
	summary := fmt.Sprintf("[summary] %d/%d tasks done, %d rounds, %s elapsed",
		completed, completed+failed, t.coordinator.totalRounds(),
		time.Since(t.coordinator.sessionTime).Round(time.Second))
	_ = t.coordinator.updateSTM(func(existing string) string {
		if existing == "" {
			existing = fmt.Sprintf("Session started at %s.", t.coordinator.sessionTime.Format(time.RFC3339))
		}
		newContent := appendSTMEntry(existing, summary, stmSectionProgress)
		return TruncateSTM(newContent)
	})

	t.coordinator.AutoExtractLTM(ctx)

	// Team-level acceptance check: an objective gate over the whole run. A
	// non-zero exit does not block finishing (the work is already done) but is
	// surfaced in the result and via a notifiable event so an unattended run's
	// failure is not silent.
	response := args.Response
	if len(failedTasks) > 0 {
		response += "\n\n⚠️ UNRESOLVED TASKS\n" + formatFailedTasks(failedTasks)
	}

	if prof.RequireClosedTerminals && t.coordinator.terminalSessionMgr != nil {
		if err := t.coordinator.terminalSessionMgr.RequireNoLeaks(""); err != nil {
			return fantasy.NewTextErrorResponse("RequireClosedTerminals policy violation: active terminal sessions remain open. Close all terminal sessions before finishing: " + err.Error()), nil
		}
	}

	if prof.RequireEvidenceManifest {
		items := t.coordinator.TaskTracker().TodoList().Items()
		completedCount := 0
		for _, item := range items {
			if item != nil && item.Status == TaskDone {
				completedCount++
				hasEvidence := item.VerifyResult != nil || (item.TypedResult != nil && len(item.TypedResult.Evidence) > 0)
				if !hasEvidence {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("RequireEvidenceManifest policy violation: completed task %q (%s) is missing verification evidence.", item.ID, item.Desc)), nil
				}
			}
		}
		if completedCount == 0 {
			return fantasy.NewTextErrorResponse("RequireEvidenceManifest policy violation: no completed tasks with verification evidence recorded."), nil
		}
	}

	// A no-progress second-threshold stop is terminal even when all tasks have
	// already reached a terminal state. Honor it before acceptance handling so
	// self-healing and rollback cannot mutate the workspace after the partial
	// result and continuation have been persisted by stopForNoProgress.
	if t.coordinator.noProgressStopPending() {
		existing := t.coordinator.LastRunResult()
		if existing == nil {
			return fantasy.NewTextErrorResponse("no-progress budget exhausted; partial run result is not available"), nil
		}
		preserved := *existing
		preserved.Response = response
		preserved.Stats = SummarizeRunStats(todoList.Items())
		preserved.Metrics = t.coordinator.Metrics()
		t.coordinator.SetLastRunResult(&preserved)
		t.coordinator.finishCalled.Store(true)
		return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", response)), nil
	}

	accRes, accErr := t.coordinator.runAcceptance(ctx)
	if accErr != nil {
		if prof.AcceptanceMode == AcceptanceBlocking || t.coordinator.IsUnattended() {
			if t.coordinator.selfHealingAttempts < 2 {
				t.coordinator.selfHealingAttempts++
				msg := fmt.Sprintf("Acceptance check failed (attempt %d/2). Initiating self-healing. Error: %v", t.coordinator.selfHealingAttempts, accErr)
				t.coordinator.report(t.coordinator.newEvent("error").withMessage(msg))
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Acceptance check failed: %v. Please analyze the failure log, modify files/re-run tasks to fix the issues, and call finish again.", accErr)), nil
			}
			if t.coordinator.IsUnattended() {
				msg := fmt.Sprintf("Acceptance check failed after %d self-healing attempts. Initiating rollback...", t.coordinator.selfHealingAttempts)
				t.coordinator.report(t.coordinator.newEvent("error").withMessage(msg))
				if rollErr := t.coordinator.runRollback(ctx); rollErr != nil {
					rollMsg := fmt.Sprintf("Rollback failed: %v", rollErr)
					t.coordinator.report(t.coordinator.newEvent("error").withMessage(rollMsg))
					response += fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v\n⚠️ ROLLBACK FAILED: %v", accErr, rollErr)
				} else {
					t.coordinator.report(t.coordinator.newEvent("error").withMessage("Workspace rolled back successfully due to acceptance check failure."))
					response += fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v\n✓ Workspace rolled back successfully.", accErr)
				}
			}
			if prof.AcceptanceMode == AcceptanceBlocking {
				t.coordinator.report(t.coordinator.newEvent("error").withMessage("acceptance check failed (blocking): " + accErr.Error()))
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Acceptance check failed (blocking): %v", accErr)), nil
			}
		} else {
			// Interactive mode: preserve standard behavior
			note := fmt.Sprintf("\n\n⚠️ ACCEPTANCE CHECK FAILED: %v", accErr)
			response += note
			t.coordinator.report(t.coordinator.newEvent("error").withMessage("acceptance check failed: " + accErr.Error()))
		}
	}

	unresolvedPending := pendingTodoItems(todoList.Items())
	allUnresolved := append(failedTasks, unresolvedPending...)
	acceptanceState := AcceptanceNotConfigured
	if accRes != nil {
		acceptanceState = accRes.State
	}
	evaluated := EvaluateRunOutcome(RunEvaluationInput{
		UnresolvedTasks: toTaskReferences(allUnresolved),
		Acceptance:      acceptanceState,
		Response:        args.Response,
		Stats:           SummarizeRunStats(todoList.Items()),
		Metrics:         t.coordinator.Metrics(),
		GoalMode:        t.coordinator.GoalMode(),
	})
	evaluated.Acceptance = accRes
	runRes := &evaluated
	t.coordinator.SetLastRunResult(runRes)

	t.coordinator.finishCalled.Store(true)
	return fantasy.NewTextResponse(fmt.Sprintf("FINISHED:%s", response)), nil
}

func failedTodoItems(items []*TodoItem) []*TodoItem {
	failed := make([]*TodoItem, 0)
	for _, item := range items {
		if item != nil && (item.Status == TaskError || item.Status == TaskBlocked || item.Status == TaskProtocolIncomplete) {
			if item.Resolution != nil && (item.Resolution.Status == "superseded" || item.Resolution.Status == "reconciled" || item.Resolution.Status == "waived") {
				continue
			}
			failed = append(failed, item)
		}
	}
	return failed
}

func pendingTodoItems(items []*TodoItem) []*TodoItem {
	pending := make([]*TodoItem, 0)
	for _, item := range items {
		if item != nil && (item.Status == TaskPending || item.Status == TaskInProgress || item.Status == TaskPlanned || item.Status == TaskVerifying || item.Status == TaskPaused || item.Status == TaskProtocolIncomplete) {
			if item.Resolution != nil && (item.Resolution.Status == "superseded" || item.Resolution.Status == "reconciled" || item.Resolution.Status == "waived") {
				continue
			}
			pending = append(pending, item)
		}
	}
	return pending
}

func formatFailedTasks(items []*TodoItem) string {
	var b strings.Builder
	for _, item := range items {
		detail := FailureDisplayText(item)
		if detail == "" {
			detail = "no failure detail recorded"
		}
		fmt.Fprintf(&b, "- Task %s (%s, %s): %s\n", item.ID, item.Agent, item.Status, utils.TruncateString(detail, 1500))
	}
	return strings.TrimRight(b.String(), "\n")
}

// runAcceptance runs the team's optional acceptance command / spec in the project dir.
func (c *Coordinator) runAcceptance(parentCtx context.Context) (*AcceptanceResult, error) {
	res := &AcceptanceResult{State: AcceptanceNotConfigured}
	c.mu.RLock()
	var spec *AcceptanceSpec
	acceptanceRevision := c.acceptanceContractRevision
	if c.acceptanceSpec != nil {
		specCopy := cloneAcceptanceSpec(*c.acceptanceSpec)
		spec = &specCopy
	}
	acceptanceCmd := c.acceptanceCmd
	c.mu.RUnlock()

	if spec == nil && acceptanceCmd != "" {
		spec = &AcceptanceSpec{Commands: []string{acceptanceCmd}}
	}
	if spec == nil {
		return res, nil
	}
	// An empty structured value is the compatibility representation of no
	// acceptance gate (for example SetAcceptance("")). It is not evidence of
	// goal completion and must retain the not_configured state.
	if !acceptanceSpecHasChecks(*spec) {
		return res, nil
	}
	res.State = AcceptancePassed
	res.Passed = true

	res.Commands = spec.Commands
	res.RequiredArtifacts = spec.RequiredArtifacts

	// Build all verification specifications (translated legacy + explicit typed verifications)
	var allSpecs []VerificationSpec
	for _, cmd := range spec.Commands {
		if strings.TrimSpace(cmd) != "" {
			allSpecs = append(allSpecs, VerificationSpec{
				Type:    VerifyCommandExit,
				Mode:    "success",
				Command: cmd,
			})
		}
	}
	for _, artPath := range spec.RequiredArtifacts {
		if strings.TrimSpace(artPath) != "" {
			allSpecs = append(allSpecs, VerificationSpec{
				Type: VerifyFileExists,
				Mode: "success",
				Path: artPath,
			})
		}
	}
	allSpecs = append(allSpecs, spec.Verifications...)

	// RequireNoUnresolvedTasks check
	if spec.RequireNoUnresolvedTasks {
		items := c.taskTracker.TodoList().Items()
		unresolved := append(failedTodoItems(items), pendingTodoItems(items)...)
		if len(unresolved) > 0 {
			errMsg := fmt.Sprintf("unresolved tasks exist (%d task(s))", len(unresolved))
			res.Errors = append(res.Errors, errMsg)
			res.Passed = false
			res.State = AcceptanceFailed
		}
	}

	shell := "sh"
	if c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}
	workDir := c.verificationWorkDir()
	securityMode := c.verificationSecurityMode(shell)

	// Use a bounded timeout for each acceptance verification (same as task verification).
	acceptanceTimeout := c.verifyTaskTimeout()
	var evidence []*VerificationResult

	mandatoryPassedCount := 0
	for _, vSpec := range allSpecs {
		normalizedSpec := NormalizeVerificationSpec(vSpec, "", "")
		// Invalid verifier configuration is never an observation. Validate before
		// the observation-mode skip so a malformed supplemental verifier cannot
		// make an otherwise passing acceptance contract appear satisfied.
		validationErr := validateVerificationSpec(normalizedSpec)
		verifyCtx, verifyCancel := context.WithTimeout(parentCtx, acceptanceTimeout)
		vRes, vErr := ExecuteVerificationSpec(verifyCtx, shell, workDir, vSpec)
		verifyCancel()
		// Always collect evidence for durable inspection
		if vRes != nil {
			// Acceptance evidence is tied to the immutable contract revision and
			// security settings that governed execution. A later contract or
			// profile change must not make this evidence look reusable.
			vRes.Fingerprint = ComputeVerificationFingerprintFull(
				normalizedSpec, vRes, workDir, strconv.Itoa(acceptanceRevision), securityMode)
			evidence = append(evidence, vRes)
		}
		if validationErr != nil {
			errMsg := fmt.Sprintf("acceptance verification malformed (%s): %s", normalizedSpec.Type, validationErr)
			res.Errors = append(res.Errors, errMsg)
			res.Passed = false
			res.State = AcceptanceFailed
			continue
		}
		if normalizedSpec.Mode == "observation" {
			// Observation records useful evidence, but it must not count as a
			// mandatory acceptance criterion. It is valid to collect observations
			// alongside actual assertions; an observation-only contract is rejected
			// below because mandatoryPassedCount remains zero.
			continue
		}
		// ExecuteVerificationSpec has already applied the verification mode.
		// In particular, an expected_failure assertion is successful when its
		// underlying check exits non-zero. Looking at ExitCode again here would
		// incorrectly reject that valid assertion (and would make the mode
		// usable for task verification but not run acceptance).
		if vErr != nil || vRes == nil {
			errMsg := fmt.Sprintf("acceptance verification failed (%s)", vSpec.Type)
			if vErr != nil {
				errMsg += ": " + vErr.Error()
			}
			if vRes != nil && vRes.Stderr != "" {
				errMsg += ": " + utils.TruncateString(vRes.Stderr, 500)
			}
			res.Errors = append(res.Errors, errMsg)
			res.Passed = false
			res.State = AcceptanceFailed
		} else {
			mandatoryPassedCount++
		}
	}
	res.VerificationEvidence = evidence

	if len(spec.Criteria) > 0 {
		criteria, criteriaErr := c.evaluateCriteria(parentCtx, spec.Criteria)
		res.CriterionResults = criteria
		if criteriaErr != nil {
			res.Errors = append(res.Errors, "acceptance criteria invalid: "+criteriaErr.Error())
			res.Passed, res.State = false, AcceptanceFailed
		} else {
			for _, criterion := range spec.Criteria {
				if !criterion.Required {
					continue
				}
				for _, result := range criteria {
					if result.ID == criterion.ID && result.State != CriterionPassed {
						res.Errors = append(res.Errors, fmt.Sprintf("criterion %s is %s: %s", result.ID, result.State, result.FailureReason))
						res.Passed, res.State = false, AcceptanceFailed
					}
				}
			}
		}
	}

	if len(allSpecs) > 0 && mandatoryPassedCount == 0 && res.Passed {
		res.Errors = append(res.Errors, "no mandatory acceptance criteria passed")
		res.Passed = false
		res.State = AcceptanceFailed
	}

	if !res.Passed {
		return res, fmt.Errorf("acceptance check failed: %s", strings.Join(res.Errors, "; "))
	}
	return res, nil
}

func acceptanceSpecHasChecks(spec AcceptanceSpec) bool {
	return AcceptanceSpecHasChecks(spec)
}

// runRollback runs the team's optional rollback command or default git rollback.
func (c *Coordinator) runRollback(parentCtx context.Context) error {
	cmd := strings.TrimSpace(c.rollbackCmd)
	shell := "sh"
	if c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}
	timeout := time.Duration(c.session.Config.Timeout) * time.Second
	if timeout <= 0 || timeout > 300*time.Second {
		timeout = 300 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	if cmd == "" {
		// Default rollback: git reset --hard and git clean -fd if it's a git repo
		gitDir := filepath.Join(c.projectDir, ".git")
		if _, err := os.Stat(gitDir); err == nil {
			cmd = "git reset --hard && git clean -fd"
		} else {
			return fmt.Errorf("no custom rollback command set and no git repository found in workspace")
		}
	}

	ex := exec.CommandContext(ctx, shell, "-c", cmd)
	ex.Env = utils.SanitizeSubprocessEnv(os.Environ())
	if c.projectDir != "" {
		ex.Dir = c.projectDir
	}
	out, err := ex.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			detail = ": " + utils.TruncateString(detail, 500)
		}
		return fmt.Errorf("%v%s", err, detail)
	}
	return nil
}

type loadSkillTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *loadSkillTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "load_skill",
		Description: "Load the full content of a skill by name. Use this when you need detailed instructions from a skill before planning delegation. The skill content will help you understand how to instruct workers properly.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The skill name to load (e.g. 'git-commit')",
			},
		},
		Required: []string{"name"},
	}
}

func (t *loadSkillTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Name == "" {
		return fantasy.NewTextErrorResponse("skill name is required"), nil
	}

	agentName := "coordinator"
	if name, _ := ctx.Value(tools.AgentNameKey).(string); name != "" {
		agentName = name
	}

	nameLower := strings.ToLower(args.Name)
	skills := t.coordinator.getSkills()
	for _, s := range skills {
		if strings.ToLower(s.Name) == nameLower {
			t.coordinator.recordSkillUsage(s.Name, agentName)

			if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID != "" {
				t.coordinator.taskTracker.TodoList().AddLoadedSkill(todoID, s.Name)
				t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))
			}

			return fantasy.NewTextResponse(fmt.Sprintf("Skill: %s\nFile: %s\n\n%s", s.Name, s.Path, s.Content)), nil
		}
	}

	available := make([]string, len(skills))
	for i, s := range skills {
		available[i] = s.Name
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf("skill %q not found (available: %v)", args.Name, available)), nil
}

type todoTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *todoTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "todo",
		Description: "Manage your task list to track progress. Create items to plan your work, update their status as you progress, and list your items to review.",
		Parameters: map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "Action to perform: create, update, or list",
			},
			"items": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Task descriptions to create (used with action=create)",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "The TODO item ID to update (used with action=update)",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "New status: in_progress or done (used with action=update)",
			},
			"detail": map[string]any{
				"type":        "string",
				"description": "Optional detail or note (used with action=update)",
			},
		},
		Required: []string{"action"},
	}
}

func (t *todoTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Action string   `json:"action"`
		Items  []string `json:"items"`
		ID     string   `json:"id"`
		Status string   `json:"status"`
		Detail string   `json:"detail"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	callerName := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	if callerName == "" {
		callerName = "agent"
	}

	switch args.Action {
	case "create":
		return t.handleCreate(callerName, args.Items)
	case "update":
		return t.handleUpdate(callerName, args.ID, args.Status, args.Detail)
	case "list":
		return t.handleList(callerName)
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown action %q (valid: create, update, list)", args.Action)), nil
	}
}

type reconcileTaskTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *reconcileTaskTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "reconcile_task",
		Description: "Mark a failed or superseded task as resolved by a subsequent task or objective evidence, removing it from the unresolved failed tasks finish gate.",
		Parameters: map[string]any{
			"task_id": map[string]any{
				"type":        "string",
				"description": "ID of the failed task to reconcile or mark superseded",
			},
			"status": map[string]any{
				"type":        "string",
				"description": "Resolution status: superseded or reconciled",
				"enum":        []string{"superseded", "reconciled"},
			},
			"resolved_by": map[string]any{
				"type":        "string",
				"description": "ID of the successful task that replaced or fixed the failed task",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Explanation of how the issue was fixed or why the task was superseded",
			},
			"evidence": map[string]any{
				"type":        "array",
				"description": "Optional list of objective evidence references verifying resolution",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"value":       map[string]any{"type": "string"},
					},
				},
			},
		},
		Required: []string{"task_id", "status", "resolved_by", "reason"},
	}
}

func (t *reconcileTaskTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TaskID     string        `json:"task_id"`
		Status     string        `json:"status"`
		ResolvedBy string        `json:"resolved_by"`
		Reason     string        `json:"reason"`
		Evidence   []EvidenceRef `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	// Security: strip model-injected HMAC signatures from tool input to prevent forged signatures
	for i := range args.Evidence {
		args.Evidence[i].SystemHMAC = ""
	}
	res := &TaskResolution{
		Status:     args.Status,
		ResolvedBy: args.ResolvedBy,
		Reason:     args.Reason,
		Evidence:   args.Evidence,
	}
	if err := t.coordinator.taskTracker.TodoList().SetTaskResolution(args.TaskID, res); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to reconcile task: %v", err)), nil
	}
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))
	return fantasy.NewTextResponse(fmt.Sprintf("task %s marked as %s by task %s: %s", args.TaskID, args.Status, args.ResolvedBy, args.Reason)), nil
}

func (t *todoTool) handleCreate(callerName string, items []string) (fantasy.ToolResponse, error) {
	if len(items) == 0 {
		return fantasy.NewTextErrorResponse("items is required for create action"), nil
	}

	resolvedModel := t.coordinator.resolveCurrentAgentModel(callerName)
	parentID := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })

	batch := make([]TodoSpec, len(items))
	for i, desc := range items {
		batch[i] = TodoSpec{Agent: callerName, Desc: desc, Model: resolvedModel, Source: TaskSourceAgent, ParentID: parentID}
	}

	added := t.coordinator.taskTracker.TodoList().AddBatch(batch)
	// Agent-created TODOs are objective work units even though they bypass the
	// coordinator's ExecuteTasks batch path.
	t.coordinator.recordNoProgressTasks(len(added))
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	var b strings.Builder
	b.WriteString("Created TODO items:\n")
	for _, item := range added {
		fmt.Fprintf(&b, "- %s: %s [%s]\n", item.ID, item.Desc, item.Status)
	}
	return fantasy.NewTextResponse(b.String()), nil
}

func (t *todoTool) handleUpdate(callerName string, id string, status string, detail string) (fantasy.ToolResponse, error) {
	if id == "" {
		return fantasy.NewTextErrorResponse("id is required for update action"), nil
	}

	todoItems := t.coordinator.taskTracker.TodoList().Items()
	var targetItem *TodoItem
	for _, item := range todoItems {
		if item.ID == id {
			targetItem = item
			break
		}
	}
	if targetItem == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("TODO item %q not found", id)), nil
	}
	if targetItem.Agent != callerName {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("cannot update TODO item %q: it belongs to agent %q", id, targetItem.Agent)), nil
	}

	var taskStatus TaskStatus
	switch status {
	case "in_progress":
		taskStatus = TaskInProgress
	case "done":
		taskStatus = TaskDone
	default:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid status %q (valid: in_progress, done)", status)), nil
	}

	// TaskDone and TaskSkipped are terminal states - cannot be updated.
	// TaskError can only be updated to in_progress (for retries).
	if targetItem.Status == TaskDone || targetItem.Status == TaskSkipped {
		return fantasy.NewTextErrorResponse(
			fmt.Sprintf("cannot update completed TODO %q (status: %s): create a new task instead", id, targetItem.Status),
		), nil
	}
	if targetItem.Status == TaskError && status != "in_progress" {
		return fantasy.NewTextErrorResponse(
			fmt.Sprintf("cannot update error TODO %q to %s: only in_progress is allowed for retries", id, status),
		), nil
	}

	t.coordinator.taskTracker.TodoList().UpdateStatus(id, taskStatus, detail)
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	return fantasy.NewTextResponse(fmt.Sprintf("Updated TODO %s to %s", id, taskStatus)), nil
}

func (t *todoTool) handleList(callerName string) (fantasy.ToolResponse, error) {
	todoItems := t.coordinator.taskTracker.TodoList().Items()
	var myItems []*TodoItem
	for _, item := range todoItems {
		if item.Agent == callerName {
			myItems = append(myItems, item)
		}
	}
	if len(myItems) == 0 {
		return fantasy.NewTextResponse("No TODO items."), nil
	}

	var b strings.Builder
	for _, item := range myItems {
		fmt.Fprintf(&b, "- %s: %s [%s]", item.ID, item.Desc, item.Status)
		if failure := FailureDisplayText(item); failure != "" {
			fmt.Fprintf(&b, " (%s)", utils.TruncateString(failure, 1500))
		} else if detail := TaskDetailDisplayText(item); detail != "" {
			fmt.Fprintf(&b, " (%s)", utils.TruncateString(detail, 500))
		}
		b.WriteString("\n")
	}
	return fantasy.NewTextResponse(b.String()), nil
}
