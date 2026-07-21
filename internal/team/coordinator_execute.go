package team

import (
	"context"
	"fmt"
	"strings"
)

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
	if c.IsWrapUp() {
		c.report(c.newEvent("step").withMessage("Wrap-up: refusing to start new tasks"))
		return "", fmt.Errorf("wrap-up in progress: refusing to delegate new tasks. Call finish immediately with your best summary of work completed so far")
	}

	tasks = expandPipelineDeps(tasks)

	if detectTaskCycle(tasks) {
		return "", fmt.Errorf("tasks contain a dependency cycle — check depends_on indices")
	}

	if err := validateOnFailureTargets(tasks); err != nil {
		return "", fmt.Errorf("on_failure validation failed: %w", err)
	}

	// on_failure without max_retries is the natural way to request a retry
	// loop; default to a single retry so the loop actually triggers.
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
		if _, _, err := c.resolveAgentName(t.Agent); err != nil {
			if !seenInvalid[t.Agent] {
				invalidAgents = append(invalidAgents, err.Error())
				seenInvalid[t.Agent] = true
			}
		}
	}
	if len(invalidAgents) > 0 {
		return "", fmt.Errorf("agent validation failed:\n- %s", strings.Join(invalidAgents, "\n- "))
	}

	if c.forcePlanFirst {
		for i := range tasks {
			if tasks[i].PlanID == "" {
				tasks[i].PlanFirst = true
			}
		}
	}

	c.round++
	if c.session.Config.MaxRounds > 0 && c.round > c.session.Config.MaxRounds {
		c.wrapUp.Store(1)
		detail := c.FailureDetail(fmt.Errorf("max rounds (%d) exceeded", c.session.Config.MaxRounds), FailureSourceMaxRoundsExceeded)
		c.PersistFailure("coordinator", fmt.Sprintf("max rounds (%d) exceeded", c.session.Config.MaxRounds), "", detail)
		return "", fmt.Errorf("max rounds (%d) exceeded: call finish immediately with your best summary of work completed so far", c.session.Config.MaxRounds)
	}

	callerID, _ := ctx.Value(todoIDKey{}).(string)

	// Budget circuit-breaker: when running unattended there is no human to stop
	// a runaway. If a configured wall-clock or token budget is exceeded, force
	// wrap-up, emit a notifiable event, and refuse to delegate new tasks.
	if exceeded, reason := c.budgetExceeded(); exceeded {
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

	duplicateWarnings, duplicateIndices, suppressedDuplicates := c.checkDuplicateTasks(ctx, tasks)
	if len(duplicateWarnings) > 0 {
		c.report(c.newEvent("loop_warning").withMessage(fmt.Sprintf("Duplicate task delegation detected: %v", duplicateWarnings)))
	}

	todoBatch := make([]TodoSpec, len(tasks))
	for i, t := range tasks {
		agentDef, _, resolveErr := c.resolveAgentName(t.Agent)
		var resolvedModel string
		if resolveErr != nil {
			c.report(c.newEvent("step").withMessage(fmt.Sprintf("warning: could not resolve agent %q: %v", t.Agent, resolveErr)))
		} else if agentDef != nil {
			overrideModel := t.Model
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
		sideEffect, recovery, reconcileTool := resolveTaskRecovery(agentDef, t)
		todoBatch[i] = TodoSpec{
			Agent:         strings.ToLower(t.Agent),
			Desc:          desc,
			Model:         resolvedModel,
			Source:        TaskSourceCoordinator,
			ParentID:      "",
			Verify:        t.Verify,
			VerifyMode:    t.VerifyMode,
			MaxRetries:    t.MaxRetries,
			SideEffect:    sideEffect,
			Recovery:      recovery,
			ReconcileTool: reconcileTool,
		}
	}
	todoItems := c.taskTracker.TodoList().AddBatch(todoBatch)
	if len(c.session.Config.Preflight) > 0 {
		if _, err := c.checkCapabilityRequirements(ctx, c.session.Config.Preflight); err != nil {
			if blocked, ok := isCapabilityBlockedError(err); ok {
				detail := blocked.Reason
				if detail == "" {
					detail = "capability preflight blocked"
				}
				for _, item := range todoItems {
					c.taskTracker.TodoList().UpdateStatus(item.ID, TaskBlocked, detail)
				}
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

	return formatTaskResults(results, len(tasks), duplicateWarnings)
}
