package team

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// contractWarningDedup deduplicates contract_warning events per
// (todoID, code, message) within a single dispatch cycle. It is a shared
// pointer so that cloned/isolated coordinators (extra-models) participate in
// the same dedup set rather than each re-emitting the same warning for the
// same todoID. Refs: §4.3, WP-02 reviewer P2.
type contractWarningDedup struct {
	mu      sync.Mutex
	emitted map[string]bool
}

// newContractWarningDedup returns a fresh dedup set.
func newContractWarningDedup() *contractWarningDedup {
	return &contractWarningDedup{emitted: make(map[string]bool)}
}

// contractWarningsDedup returns the coordinator's shared contract-warning
// dedup set, initializing it exactly once under contractWarningsOnce. This
// is thread-safe for concurrent scheduler goroutines and parallel extra-model
// clones; it never reassigns a non-nil pointer, so the dedup set is never
// lost to a race. Refs: §4.3, WP-02.
func (c *Coordinator) contractWarningsDedup() *contractWarningDedup {
	c.contractWarningsOnce.Do(func() {
		if c.contractWarnings == nil {
			c.contractWarnings = newContractWarningDedup()
		}
	})
	return c.contractWarnings
}

// emitOnce reports whether this is the first time the given key is seen. If
// so it returns true (caller should emit) and records the key; otherwise it
// returns false (already emitted — suppress).
func (d *contractWarningDedup) emitOnce(key string) bool {
	if d == nil {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.emitted == nil {
		d.emitted = make(map[string]bool)
	}
	if d.emitted[key] {
		return false
	}
	d.emitted[key] = true
	return true
}

// expandPipelineDeps rewrites the pipeline:true shorthand into explicit
// depends_on edges: a task with Pipeline set gains a dependency on the
// immediately preceding task in the batch. Pipeline on the first task has
// nothing to wait for and is ignored. Explicit depends_on entries are
// preserved (deduplicated). The input slice and its DependsOn backing
// arrays are never mutated.
func expandPipelineDeps(tasks []TaskDef) []TaskDef {
	out := make([]TaskDef, len(tasks))
	copy(out, tasks)
	for i := range out {
		if !out[i].Pipeline || i == 0 {
			continue
		}
		deps := make([]int, 0, len(out[i].DependsOn)+1)
		seen := make(map[int]bool, len(out[i].DependsOn)+1)
		for _, d := range out[i].DependsOn {
			if !seen[d] {
				seen[d] = true
				deps = append(deps, d)
			}
		}
		if !seen[i-1] {
			deps = append(deps, i-1)
		}
		out[i].DependsOn = deps
	}
	return out
}

func (c *Coordinator) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	var err error
	tasks, err = c.bindInitialTaskContracts(tasks)
	if err != nil {
		// This is a compile-time configuration conflict, not a worker failure:
		// no TODO/model call has happened and no execution retry is consumed.
		return "", c.rejectDelegationPolicy(err.Error())
	}
	tasks, err = c.bindTaskGoalContracts(tasks)
	if err != nil {
		// A selector ambiguity is a static contract error. No TODO/model call has
		// happened and no execution retry is consumed.
		return "", c.rejectDelegationPolicy(err.Error())
	}
	// Normalize accidental mutation batching before policy/preflight. This
	// preserves the coordinator's requested task set while making the safety
	// dependency explicit to the DAG scheduler.
	tasks = c.serializeMutationTasks(tasks)
	// A configured delegation policy is checked before workspace validation,
	// resource locking, TODO creation, or worker startup. It therefore leaves
	// previously successful independent work untouched on rejection.
	if err := c.validateDelegationPolicy(tasks); err != nil {
		return "", err
	}
	if c.session != nil {
		for _, task := range tasks {
			if err := validateSharedContextFiles(c.session.Workspace, task.ContextFiles); err != nil {
				return "", fmt.Errorf("invalid context_files for agent %q: %w", task.Agent, err)
			}
		}
	}
	if err := c.ValidateWorkspaceIsolation(); err != nil {
		return "", err
	}
	if err := c.ValidateResourceLocks(ctx); err != nil {
		return "", err
	}
	if c.IsWrapUp() && !c.acceptanceRecovery.Load() {
		c.report(c.newEvent("step").withMessage("Wrap-up: refusing to start new tasks"))
		return "", fmt.Errorf("wrap-up in progress: refusing to delegate new tasks. Call finish immediately with your best summary of work completed so far")
	}

	tasks = expandPipelineDeps(tasks)
	// A worker must make its terminal outcome explicit. This is independent of
	// task type and prevents a prose failure report from being recorded as a
	// completed task. Apply the runtime invariant before structural preflight:
	// a closed sequence terminating in submit_result is coherent only when the
	// worker is required to submit that result.
	//
	// A sidecar is exempt because a tool-less call cannot invoke submit_result;
	// validateSidecarTaskContracts below rejects an unsafe sidecar contract.
	for i := range tasks {
		if !tasks[i].Sidecar {
			tasks[i].Execution.RequiresResult = true
		}
	}
	for _, task := range tasks {
		if err := validateTaskOutputMode(task); err != nil {
			return "", err
		}
		if err := c.validateAndReportContract(task, ""); err != nil {
			return "", err
		}
	}

	if detectTaskCycle(tasks) {
		return "", fmt.Errorf("tasks contain a dependency cycle — check depends_on indices")
	}

	if err := validateOnFailureTargets(tasks); err != nil {
		return "", fmt.Errorf("on_failure validation failed: %w", err)
	}

	// on_failure without max_retries is the natural way to request a retry
	// loop; default to a single retry so the loop actually triggers.
	// A sidecar has no tools, so it cannot be exempted from RequiresResult and
	// also be trusted to have changed something. Reject that combination before
	// any model call happens.
	if err := c.validateSidecarTaskContracts(tasks); err != nil {
		return "", err
	}

	for i := range tasks {
		if tasks[i].OnFailure != nil && tasks[i].MaxRetries < 1 {
			tasks[i].MaxRetries = 1
		}
	}

	// Validate all agents upfront to catch unknown agents early.
	// This must run regardless of whether session is nil — skipping
	// validation would allow invalid agent names to pass silently.
	var invalidAgents []string
	seenInvalid := make(map[string]bool)
	for _, t := range tasks {
		if _, _, err := c.AgentPool().ResolveAgentName(t.Agent); err != nil {
			if !seenInvalid[t.Agent] {
				invalidAgents = append(invalidAgents, err.Error())
				seenInvalid[t.Agent] = true
			}
		}
	}
	if len(invalidAgents) > 0 {
		return "", fmt.Errorf("agent validation failed:\n- %s", strings.Join(invalidAgents, "\n- "))
	}
	c.normalizeOutcomeTaskKinds(tasks)
	if err := c.validateTaskCriterionLinks(tasks); err != nil {
		return "", err
	}

	if c.forcePlanFirst {
		for i := range tasks {
			if tasks[i].PlanID == "" {
				tasks[i].PlanFirst = true
			}
		}
	}

	c.round++
	if c.session.Config.MaxRounds > 0 && c.round > c.session.Config.MaxRounds && !c.acceptanceRecovery.Load() {
		c.wrapUp.Store(1)
		detail := c.FailureDetail(fmt.Errorf("max rounds (%d) exceeded", c.session.Config.MaxRounds), FailureSourceMaxRoundsExceeded)
		c.PersistFailure("coordinator", fmt.Sprintf("max rounds (%d) exceeded", c.session.Config.MaxRounds), "", detail)
		return "", fmt.Errorf("max rounds (%d) exceeded: call finish immediately with your best summary of work completed so far", c.session.Config.MaxRounds)
	}

	callerID, _ := ctx.Value(todoIDKey{}).(string)

	// Budget circuit-breaker: when running unattended there is no human to stop
	// a runaway. If a configured wall-clock or token budget is exceeded, force
	// wrap-up, emit a notifiable event, and refuse to delegate new tasks.
	if exceeded, reason := c.budgetExceeded(); exceeded && !c.acceptanceRecovery.Load() {
		c.wrapUp.Store(1)
		detail := c.FailureDetail(fmt.Errorf("%s", reason), FailureSourceBudgetExceeded)
		agentName := c.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
		taskDesc := c.getSnapshotField(func(s *currentSnapshot) string { return s.Task })
		todoID := ""
		if callerID != "" && callerID != CoordTodoID {
			todoID = callerID
		}
		c.PersistFailure(agentName, taskDesc, todoID, detail)
		if c.budgetTripped.CompareAndSwap(false, true) {
			c.report(c.newEvent("budget_exceeded").withMessage(reason))
		}
		return "", fmt.Errorf("%s: call finish immediately with your best summary of work completed so far", reason)
	}

	// Bump the cache generation when the coordinator starts a new delegation
	// round. Worker-level ExecuteTasks calls carry a worker todoID in context,
	// not CoordTodoID, so they do NOT bump the generation. This means all
	// sub-tasks spawned by workers within the same coordinator round share the
	// same generation and can deduplicate against each other. When the
	// coordinator starts a new round (new agent call), the generation bumps,
	// making all previous cached results invalid — ensuring stale workspace
	// state is never reused.
	if callerID == "" || callerID == CoordTodoID {
		newGen := c.cacheGeneration.Add(1)
		c.taskResultCacheMu.Lock()
		for key, entries := range c.taskResultCache {
			var fresh []cachedTaskEntry
			for _, e := range entries {
				// Pinned entries were restored from a previous run (session.json
				// or the task journal); without this they would be wiped before
				// their first lookup ever happens.
				if e.generation == newGen || e.pinned {
					fresh = append(fresh, e)
				}
			}
			c.taskResultCache[key] = fresh
		}
		c.taskResultCacheMu.Unlock()
	}

	c.report(c.newEvent("step").withMessage(fmt.Sprintf("Round %d: delegating %d task(s)", c.round, len(tasks))))

	duplicateWarnings, duplicateIndices, suppressedDuplicates := c.Planner().CheckDuplicate(ctx, tasks)
	if len(duplicateWarnings) > 0 {
		c.report(c.newEvent("loop_warning").withMessage(fmt.Sprintf("Duplicate task delegation detected: %v", duplicateWarnings)))
	}

	todoBatch := make([]TodoSpec, len(tasks))
	for i, t := range tasks {
		agentDef, _, resolveErr := c.AgentPool().ResolveAgentName(t.Agent)
		var resolvedModel string
		if resolveErr != nil {
			c.report(c.newEvent("step").withMessage(fmt.Sprintf("warning: could not resolve agent %q: %v", t.Agent, resolveErr)))
		} else if agentDef != nil {
			overrideModel := c.selectTaskModel(t, agentDef)
			if len(c.modelList) == 0 {
				overrideModel = ""
			}
			resolvedModel = c.resolveAgentModel(agentDef, overrideModel)
		} else {
			c.report(c.newEvent("step").withMessage(fmt.Sprintf("warning: unknown agent %q", t.Agent)))
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		// Side-effect classification precedence (§11.2):
		//   1. task-level explicit (TaskDef.SideEffect)
		//   2. agent-level default (agent .md frontmatter)
		//   3. tool-inferred heuristic (InferSideEffectClass)
		// An empty class at all tiers falls back to SideEffectNone (→ retry),
		// preserving pre-recovery behavior for read-only agents.
		sideEffect, recovery, reconcileTool := c.PolicyEngine().ResolveRecoveryPolicy(agentDef, t)
		todoBatch[i] = TodoSpec{
			PlanTaskID:          t.ID,
			ContractID:          t.ContractID,
			ContractHash:        t.ContractHash,
			ContractRevision:    t.ContractRevision,
			Agent:               strings.ToLower(t.Agent),
			Desc:                desc,
			Model:               resolvedModel,
			Source:              TaskSourceCoordinator,
			ParentID:            "",
			Verify:              t.Verify,
			VerifyMode:          t.VerifyMode,
			VerifySpec:          cloneVerificationSpecPtr(t.VerifySpec),
			MaxRetries:          t.MaxRetries,
			SideEffect:          sideEffect,
			Recovery:            recovery,
			ReconcileTool:       reconcileTool,
			Kind:                t.Kind,
			Advances:            append([]string(nil), t.Advances...),
			ExpectedStateChange: t.ExpectedStateChange,
			RecoveryHypothesis:  t.RecoveryHypothesis,
			Execution:           t.Execution,
		}
	}
	// The successful exact initial-policy validation above is the sole point at
	// which a fresh session may advance. Persist the phase before AddBatch's
	// checkpoint callback so a crash cannot leave a task list and phase that
	// disagree on resume.
	c.markInitialDelegationAccepted()
	todoItems := c.taskTracker.TodoList().AddBatch(todoBatch)
	// No-progress budget (§8.1, WP-12): each newly created task is one unit
	// of "tasks since last objective progress". Increment here at AddBatch;
	// reset only by criterion advancement (criteria.go).
	c.recordNoProgressTasks(len(todoItems))
	if len(c.session.Config.Preflight) > 0 {
		if _, err := c.checkCapabilityRequirements(ctx, c.session.Config.Preflight); err != nil {
			if blocked, ok := isCapabilityBlockedError(err); ok {
				detail := blocked.Reason
				if detail == "" {
					detail = "capability preflight blocked"
				}
				for _, item := range todoItems {
					c.PersistFailureWithClassAndStatus(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailurePolicy, TaskBlocked)
				}
				c.reconcileTaskStatusProjection()
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("needs_human").withMessage(detail))
				c.PersistFailure("coordinator", "capability preflight failed", "", detail)
				return "", fmt.Errorf("capability preflight failed: %s", detail)
			}
			return "", err
		}
	}
	if len(suppressedDuplicates) > 0 {
		var removeIDs []string
		for idx := range suppressedDuplicates {
			if idx >= 0 && idx < len(todoItems) {
				removeIDs = append(removeIDs, todoItems[idx].ID)
			}
		}
		c.taskTracker.TodoList().DeleteIDs(removeIDs...)
	}

	// Fill in OnFailure IDs now that todo IDs exist. Indices were validated by
	// validateOnFailureTargets above.
	for i, t := range tasks {
		if t.OnFailure != nil {
			todoItems[i].OnFailure = todoItems[*t.OnFailure].ID
		}
	}

	// Fill in dependency IDs for display and dependency-wait logic.
	for i, t := range tasks {
		if len(t.DependsOn) > 0 {
			var depIDs []string
			for _, depIdx := range t.DependsOn {
				if depIdx >= 0 && depIdx < len(todoItems) && depIdx != i {
					depIDs = append(depIDs, todoItems[depIdx].ID)
				}
			}
			if len(depIDs) > 0 {
				todoItems[i].DependsOn = depIDs
			}
		}
	}

	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	c.stepConfirmFnMu.RLock()
	stepFn := c.stepConfirmFn
	c.stepConfirmFnMu.RUnlock()
	if stepFn != nil {
		approved, err := stepFn(ctx, tasks)
		if err != nil {
			return "", err
		}
		if !approved {
			for _, item := range todoItems {
				c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, c.FailureDetail(fmt.Errorf("user declined task execution"), FailureSourceUserDeclined))
			}
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("step").withMessage("Steps: user declined task execution"))
			c.wrapUp.Store(1)
			c.PersistFailure("coordinator", "user declined task execution", "", c.FailureDetail(fmt.Errorf("user declined task execution"), FailureSourceUserDeclined))
			return "", fmt.Errorf("user declined task execution: call finish immediately with your best summary of work completed so far")
		}
	}

	results, err := newDAGScheduler(c, tasks, todoItems, duplicateIndices).run(ctx)
	if err != nil {
		return "", err
	}

	if c.forcePlanFirst {
		for i := range results {
			r := &results[i]
			if r.planText == "" {
				continue
			}
			for reviewCycle := 0; reviewCycle <= planReviewerMaxReviews+1; reviewCycle++ {
				pr, err := c.getPlanReviewer(ctx, r.todoID)
				if err != nil {
					r.planText = ""
					r.err = fmt.Errorf("plan reviewer failed: %w", err)
					break
				}
				output, approved, execErr, err := pr.review(ctx, r.planText)
				if err != nil {
					r.planText = ""
					r.err = fmt.Errorf("plan reviewer failed: %w", err)
					break
				}
				if execErr != nil {
					r.planText = ""
					r.err = execErr
					break
				}
				if approved {
					r.planText = ""
					r.output = output
					break
				}
				c.pendingPlansMu.Lock()
				entry := c.pendingPlans[r.todoID]
				if entry != nil {
					r.planText = entry.PlanText
				} else {
					r.planText = ""
				}
				c.pendingPlansMu.Unlock()
				if r.planText == "" {
					r.err = fmt.Errorf("plan rejected but no new plan submitted")
					break
				}
			}
		}
	}

	c.checkpointSTM()

	// Check for skill patterns after completing a round
	c.checkSkillPatterns()

	// No-progress budget enforcement at the task-end / round boundary (§8.1,
	// WP-12). After a round of tasks completes, evaluate the three counters
	// against the configured limits. First threshold → replan (emit event,
	// force wrap-up turn, refuse further blind delegation); second threshold
	// → stop the run with a partial outcome and continuation record.
	if stopped, stopReason := c.enforceNoProgressBudget(); stopped {
		return "", fmt.Errorf("%s: call finish immediately with your best summary of work completed so far", stopReason)
	}

	return formatTaskResults(results, len(tasks), duplicateWarnings)
}

// validateObjectiveVerification rejects interactive trec-drive tasks that do
// validateObjectiveVerification validates structured execution contracts for tasks.
// Execution risk and verification requirements are driven by ValidateExecutionContract without inspecting Goal prose.
func validateObjectiveVerification(tasks []TaskDef) error {
	for _, task := range tasks {
		if err := ValidateExecutionContract(task); err != nil {
			return err
		}
	}
	return nil
}

// validateAndReportContract runs the pre-dispatch contract preflight for a
// single task (called by ExecuteTasks before TODOs are created). It enforces
// error-severity findings (blocks dispatch) and records contract-class
// failures, but does NOT emit contract_warning events — the per-task
// execution path (executeTask → validateContractStructural) is the single
// warning emitter so a warn-mode task emits exactly one contract_warning per
// dispatch regardless of entry point (scheduler or crash-resume).
// Refs: §4.3, WP-02 reviewer P2.
func (c *Coordinator) validateAndReportContract(task TaskDef, todoID string) error {
	lintMode := ""
	if c.session != nil {
		lintMode = c.session.Config.Reliability.VerifierLintMode
	}
	result := ValidateExecutionContractFull(task, lintMode)
	if err := result.Error(); err != nil {
		c.recordContractFailure(task, todoID, result.Findings)
		return err
	}
	return nil
}

// validateContractStructural enforces the blocking (error-severity) findings
// from the contract preflight AND emits contract_warning events for
// warning-severity findings. It is the single warning emitter: every
// executeTask invocation (whether reached via the scheduler or via
// crash-resume) routes through it. emitContractWarnings deduplicates per
// (todoID, code, message) so a task retried within a dispatch cycle does not
// re-emit the same warning. Refs: §4.3, WP-02 reviewer P1 (todoID) & P2.
func (c *Coordinator) validateContractStructural(task TaskDef, todoID string) error {
	lintMode := ""
	if c.session != nil {
		lintMode = c.session.Config.Reliability.VerifierLintMode
	}
	result := ValidateExecutionContractFull(task, lintMode)
	if err := result.Error(); err != nil {
		c.recordContractFailure(task, todoID, result.Findings)
		return err
	}
	c.emitContractWarnings(todoID, result.Findings)
	return nil
}

// emitContractWarnings emits a contract_warning event for each warning-severity
// finding, but at most once per (todoID, code) within a dispatch cycle. This
// ensures a warn-mode task emits exactly one contract_warning per finding
// whether it reaches execution via the scheduler (preflight + executeTask) or
// via crash-resume (executeTask only). Refs: §4.3, WP-02 reviewer P2.
func (c *Coordinator) emitContractWarnings(todoID string, findings []ContractFinding) {
	if c == nil || len(findings) == 0 {
		return
	}
	dedup := c.contractWarningsDedup()
	for _, f := range findings {
		if f.Severity != FindingSeverityWarning {
			continue
		}
		key := todoID + "|" + f.Code + "|" + f.Message
		if !dedup.emitOnce(key) {
			continue
		}
		c.report(c.newEvent("contract_warning").
			withMessage(fmt.Sprintf("%s: %s", f.Code, f.Message)))
	}
}

// recordContractFailure records a contract-class failure (no dispatch) with
// structured evidence so the failure is self-contained (§5, §9, WP-02).
// todoID is passed through to PersistFailure so the resumed task is marked
// terminal (TaskError/TaskBlocked) rather than left pending and re-driven
// forever on the next crash-resume (reviewer P1).
func (c *Coordinator) recordContractFailure(task TaskDef, todoID string, findings []ContractFinding) {
	if c != nil {
		preflightFailures := 0
		nonAsserting := 0
		for _, finding := range findings {
			if finding.Severity != FindingSeverityError {
				continue
			}
			preflightFailures++
			if finding.Code == FindingVerifierNotAsserting {
				nonAsserting++
			}
		}
		if preflightFailures > 0 {
			c.metricsMu.Lock()
			c.preflightFailuresCaught += preflightFailures
			c.nonAssertingVerifiersRejected += nonAsserting
			c.metricsMu.Unlock()
		}
	}
	agentName := task.Agent
	taskDesc := task.Goal
	var msgs []string
	for _, f := range findings {
		if f.Severity == FindingSeverityError {
			msgs = append(msgs, fmt.Sprintf("%s (%s): %s", f.Field, f.Code, f.Message))
		}
	}
	detail := fmt.Errorf("contract preflight failed: %s", strings.Join(msgs, "; "))
	c.PersistFailure(agentName, taskDesc, todoID, c.FailureDetail(detail, string(FailureContract)))
}
