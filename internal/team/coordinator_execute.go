package team

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/audit"
	"github.com/anomalyco/hufu/internal/hooks"
	"github.com/anomalyco/hufu/internal/skill"
	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

func (c *Coordinator) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
	if c.IsWrapUp() {
		c.report(c.newEvent("step").withMessage("Wrap-up: refusing to start new tasks"))
		return "", fmt.Errorf("wrap-up in progress: refusing to delegate new tasks. Call finish immediately with your best summary of work completed so far")
	}

	if detectTaskCycle(tasks) {
		return "", fmt.Errorf("tasks contain a dependency cycle — check depends_on indices")
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
		return "", fmt.Errorf("max rounds (%d) exceeded: call finish immediately with your best summary of work completed so far", c.session.Config.MaxRounds)
	}

	// Budget circuit-breaker: when running unattended there is no human to stop
	// a runaway. If a configured wall-clock or token budget is exceeded, force
	// wrap-up, emit a notifiable event, and refuse to delegate new tasks.
	if exceeded, reason := c.budgetExceeded(); exceeded {
		c.wrapUp.Store(1)
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
	callerID, _ := ctx.Value(todoIDKey{}).(string)
	if callerID == "" || callerID == CoordTodoID {
		newGen := c.cacheGeneration.Add(1)
		c.taskResultCacheMu.Lock()
		for key, entries := range c.taskResultCache {
			var fresh []cachedTaskEntry
			for _, e := range entries {
				if e.generation == newGen {
					fresh = append(fresh, e)
				}
			}
			c.taskResultCache[key] = fresh
		}
		c.taskResultCacheMu.Unlock()
	}

	c.report(c.newEvent("step").withMessage(fmt.Sprintf("Round %d: delegating %d task(s)", c.round, len(tasks))))

	todoBatch := make([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}, len(tasks))
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
		todoBatch[i] = struct {
			Agent    string
			Desc     string
			Model    string
			Source   string
			ParentID string
		}{Agent: strings.ToLower(t.Agent), Desc: desc, Model: resolvedModel, Source: TaskSourceCoordinator, ParentID: ""}
	}
	todoItems := c.taskTracker.TodoList().AddBatch(todoBatch)

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
				c.taskTracker.TodoList().UpdateStatus(item.ID, TaskSkipped, "cancelled by user")
			}
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.report(c.newEvent("step").withMessage("Steps: user declined task execution"))
			c.wrapUp.Store(1)
			return "", fmt.Errorf("user declined task execution: call finish immediately with your best summary of work completed so far")
		}
	}

	duplicateWarnings, duplicateIndices := c.checkDuplicateTasks(ctx, tasks)
	if len(duplicateWarnings) > 0 {
		c.report(c.newEvent("loop_warning").withMessage(fmt.Sprintf("Duplicate task delegation detected: %v", duplicateWarnings)))
	}

	// doneChs[i] is closed when task i completes (success, error, or dup).
	doneChs := make([]chan struct{}, len(tasks))
	for i := range tasks {
		doneChs[i] = make(chan struct{})
	}

	resultsCh := make(chan agentTaskResult, len(tasks))
	sem := make(chan struct{}, c.maxConcurrent)
	var wg sync.WaitGroup

	// in-flight dedup: prevents duplicate tasks in the same batch from running concurrently.
	// First task to acquire a key runs; subsequent tasks wait for and share the result.
	var inflightMu sync.Mutex
	inflight := make(map[string]chan agentTaskResult)

	for i, task := range tasks {
		wg.Add(1)
		go func(td TaskDef, tid string, idx int, dup bool) {
			defer wg.Done()
			defer close(doneChs[idx])

			// Compute effective description (goal takes precedence over task)
			desc := td.Goal
			if td.Constraints != "" {
				desc += "\nconstraints: " + td.Constraints
			}
			agentKey := strings.ToLower(td.Agent)
			cacheKey := agentKey + ":" + truncateTaskDesc(desc)
			var isOwner bool

			defer func() {
				if r := recover(); r != nil {
					log.Printf("[PANIC] task goroutine %q/%q recovered: %v", td.Agent, tid, r)
					c.taskTracker.TodoList().UpdateStatus(tid, TaskError, fmt.Sprintf("panic: %v", r))
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					res := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: fmt.Errorf("panic recovered: %v", r)}
					if isOwner {
						inflightMu.Lock()
						if ch, ok := inflight[cacheKey]; ok {
							select {
							case ch <- res:
							default:
							}
							delete(inflight, cacheKey)
						}
						inflightMu.Unlock()
					}
					resultsCh <- res
				}
			}()
			if dup {
				errMsg := fmt.Errorf("duplicate task: %s - reference existing completed task instead", truncateTaskDesc(td.Goal))
				c.taskTracker.TodoList().UpdateStatus(tid, TaskError, "duplicate task detected")
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.report(c.newEvent("step").withAgent(td.Agent).withMessage(fmt.Sprintf("duplicate detected: %s", truncateTaskDesc(td.Goal))))
				log.Printf("[WARN] duplicate task rejected: agent=%q, task=%q", td.Agent, td.Goal)
				resultsCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: "", err: errMsg}
				return
			}

			// Wait for dependencies before acquiring the semaphore.
			// Must be before sem<- to avoid deadlock when max_concurrent < len(tasks).
			for _, depIdx := range td.DependsOn {
				if depIdx < 0 || depIdx >= len(doneChs) || depIdx == idx {
					continue
				}
				select {
				case <-doneChs[depIdx]:
				case <-ctx.Done():
					c.taskTracker.TodoList().UpdateStatus(tid, TaskError, "dependency wait cancelled")
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					resultsCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: td.Goal, err: ctx.Err()}
					return
				}
			}

			sem <- struct{}{}
			semReleased := false
			defer func() {
				if !semReleased {
					<-sem
				}
			}()

			// Check in-flight dedup map — wait for first task with same key to complete.
			// Release the semaphore slot while waiting; we are not doing real work.
			inflightMu.Lock()
			if ch, ok := inflight[cacheKey]; ok {
				inflightMu.Unlock()
				semReleased = true
				<-sem
				select {
				case result := <-ch:
					resultsCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: result.output, err: result.err}
				case <-ctx.Done():
					resultsCh <- agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, err: ctx.Err()}
				}
				return
			}
			inflight[cacheKey] = make(chan agentTaskResult, 1)
			isOwner = true
			inflightMu.Unlock()

			// Check task result cache before running. Sidecar tasks and tasks
			// that explicitly request summarize always run fresh.
			if !td.Sidecar && !td.Summarize {
				if cached, ok := c.lookupTaskCache(ctx, agentKey, desc); ok {
					c.report(c.newEvent("cache_hit").withAgent(td.Agent).withMessage(desc).withTodoID(tid))
					c.taskTracker.TodoList().UpdateStatusAndOutput(tid, TaskDone, utils.TruncateRunes(cached, summaryMaxRunes), cached)
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					result := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: cached}
					inflightMu.Lock()
					inflight[cacheKey] <- result
					delete(inflight, cacheKey)
					inflightMu.Unlock()
					resultsCh <- result
					return
				}
			}

			var output string
			var err error
			if td.Sidecar {
				output, err = c.executeSidecarTask(ctx, td, tid)
			} else {
				output, err = c.executeTask(ctx, td, tid)
			}
			if err == nil {
				c.storeTaskCache(agentKey, desc, output)
			}
			result := agentTaskResult{agentName: td.Agent, todoID: tid, task: desc, output: output, err: err}
			if err == nil && td.PlanFirst && td.PlanID == "" {
				c.pendingPlansMu.Lock()
				if pe := c.pendingPlans[tid]; pe != nil && pe.Status == "submitted" {
					result.planText = pe.PlanText
				}
				c.pendingPlansMu.Unlock()
			}
			inflightMu.Lock()
			inflight[cacheKey] <- result
			delete(inflight, cacheKey)
			inflightMu.Unlock()
			resultsCh <- result
		}(task, todoItems[i].ID, i, duplicateIndices[i])
	}
	wg.Wait()
	close(resultsCh)

	var results []agentTaskResult
	for r := range resultsCh {
		results = append(results, r)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].agentName < results[j].agentName
	})

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

// checkSkillPatterns checks for repeating tool call patterns and auto-generates skill drafts
func (c *Coordinator) checkSkillPatterns() {
	if c.skillDetector == nil {
		return
	}

	candidates := c.skillDetector.FindCandidates(context.Background())
	if len(candidates) == 0 {
		return
	}

	// Apply per-session draft cap.
	if c.maxDrafts > 0 && len(candidates) > c.maxDrafts {
		candidates = candidates[:c.maxDrafts]
	}

	// Only report new patterns (more than previously detected)
	newPatterns := len(candidates) - c.skillPatternsDetected
	if newPatterns <= 0 {
		return
	}

	c.skillPatternsDetected = len(candidates)

	// Auto-generate skill drafts
	savedSkills := c.checkSkillPatternsAndSave()

	// Report skill pattern suggestions
	var msg strings.Builder
	msg.WriteString("─── SKILL SUGGESTIONS ───\n")
	fmt.Fprintf(&msg, "Detected %d new repeating pattern(s):\n", newPatterns)
	for i := 0; i < newPatterns && i < 3; i++ {
		cand := candidates[i]
		fmt.Fprintf(&msg, "  %d. [%s] ×%d - %s\n",
			i+1,
			strings.Join(cand.Sequence.Tools, " → "),
			cand.Sequence.Count,
			cand.SuggestedDesc)
	}
	if len(candidates) > 3 {
		fmt.Fprintf(&msg, "  ... and %d more\n", len(candidates)-3)
	}
	if len(savedSkills) > 0 {
		msg.WriteString("\nDraft skills saved to:\n")
		for _, path := range savedSkills {
			fmt.Fprintf(&msg, "  - %s\n", path)
		}
		msg.WriteString("\nReview and refine with: hufu skill review <skill-name>\n")
	}

	c.report(c.newEvent("step").withMessage(msg.String()))
}

// executeTaskWithExtraModels executes a task across multiple models when extra-models is configured.
// verify is the task's optional deliverable-verification command; it is propagated to each model so
// every model's output is verified (and retried) independently via executeTask.
func (c *Coordinator) executeTaskWithExtraModels(
	parentCtx context.Context,
	agentName string,
	agentDef *agent.AgentDef,
	taskDesc string,
	todoID string,
	verify string,
) (string, error) {
	// Limit concurrent models
	models := agentDef.ExtraModels
	if len(models) > maxConcurrentModels-1 { // -1 for main model
		models = models[:maxConcurrentModels-1]
	}

	totalModels := len(models) + 1 // +1 for main model
	results := make(chan *agentResult, totalModels)

	// Create a deep copy of agentDef with ExtraModels cleared for the main model
	// to prevent infinite recursion and data races.
	mainDef := cloneAgentDef(agentDef)
	mainDef.ExtraModels = nil
	mainModel := mainDef.Generation.Model

	go func() {
		output, err := c.executeSingleAgentWithModel(parentCtx, agentName, mainDef, taskDesc, todoID, verify)
		results <- &agentResult{model: mainModel, output: output, err: err}
	}()

	// Execute each extra model with its own deep copy
	for _, extraModel := range models {
		go func(model string) {
			extraDef := cloneAgentDef(agentDef)
			extraDef.ExtraModels = nil
			extraDef.Generation.Model = model
			output, err := c.executeSingleAgentWithModel(parentCtx, agentName, extraDef, taskDesc, todoID, verify)
			results <- &agentResult{model: model, output: output, err: err}
		}(extraModel)
	}

	// Collect all results (continue on error)
	var allResults []*agentResult
	for i := 0; i < totalModels; i++ {
		result := <-results
		allResults = append(allResults, result)
	}

	// Merge results
	merged := c.mergeAgentResults(allResults)
	return merged, nil
}

// cloneAgentDef creates a deep copy of an AgentDef to prevent data races
// when running multiple models concurrently.
func cloneAgentDef(def *agent.AgentDef) *agent.AgentDef {
	clone := *def
	if len(def.ExtraModels) > 0 {
		clone.ExtraModels = make([]string, len(def.ExtraModels))
		copy(clone.ExtraModels, def.ExtraModels)
	}
	if len(def.AllowedPaths) > 0 {
		clone.AllowedPaths = make([]string, len(def.AllowedPaths))
		copy(clone.AllowedPaths, def.AllowedPaths)
	}
	if len(def.Guard) > 0 {
		clone.Guard = make([]string, len(def.Guard))
		copy(clone.Guard, def.Guard)
	}
	if len(def.MCPTools) > 0 {
		clone.MCPTools = make(map[string]agent.MCPToolConfig, len(def.MCPTools))
		for k, v := range def.MCPTools {
			clone.MCPTools[k] = v
		}
	}
	return &clone
}

// executeSingleAgentWithModel executes a single agent task with a pre-configured agentDef.
// The caller must ensure agentDef.ExtraModels is nil to prevent infinite recursion.
// Extra models execute in isolated Coordinator instances with cloned sessions
// to prevent data races on c.session.Workspace.
func (c *Coordinator) executeSingleAgentWithModel(
	parentCtx context.Context,
	agentName string,
	agentDef *agent.AgentDef,
	taskDesc string,
	todoID string,
	verify string,
) (string, error) {
	task := TaskDef{
		Agent:  agentDef.Name,
		Goal:   taskDesc,
		Model:  agentDef.Generation.Model,
		Verify: verify,
	}

	// Create isolated workspace for this model to prevent concurrent file conflicts.
	// Use unique token (timestamp + agentName) to prevent collision when multiple
	// agents use the same model simultaneously.
	token := fmt.Sprintf("%d-%d-%s", time.Now().UnixNano(), extraWSSeq.Add(1), sanitizeModel(agentName))
	subWS := filepath.Join(c.session.Workspace, "extra-models", sanitizeModel(agentDef.Generation.Model)+"-"+token)
	if err := os.MkdirAll(subWS, 0o755); err != nil {
		return "", fmt.Errorf("failed to create isolated workspace: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(subWS); rmErr != nil {
			log.Printf("warning: failed to remove isolated workspace %s: %v", subWS, rmErr)
		}
	}()

	// Copy shared/ directory from main workspace
	sharedSrc := filepath.Join(c.session.Workspace, "shared")
	sharedDst := filepath.Join(subWS, "shared")
	if _, err := os.Stat(sharedSrc); err == nil {
		if err := copyDir(sharedSrc, sharedDst); err != nil {
			log.Printf("warning: failed to copy shared dir to isolated workspace: %v", err)
		}
	}

	// Clone Coordinator with isolated session to prevent data race on c.session.Workspace.
	// Direct mutation of c.session.Workspace is unsafe because multiple goroutines
	// execute this function concurrently for different models.
	isolatedSession := cloneSession(c.session, subWS)
	isolatedCoord := cloneCoordinator(c, isolatedSession)

	result, err := isolatedCoord.executeTask(parentCtx, task, todoID)

	// Merge skill usage statistics from the isolated coordinator back into the main coordinator
	c.skillUsageMu.Lock()
	isolatedCoord.skillUsageMu.Lock()
	for name, state := range isolatedCoord.skillUsage {
		if mainState, ok := c.skillUsage[name]; ok {
			mainState.Count += state.Count
			if mainState.Agents == nil {
				mainState.Agents = make(map[string]bool)
			}
			for agent := range state.Agents {
				mainState.Agents[agent] = true
			}
		} else {
			var agentsCopy map[string]bool
			if state.Agents != nil {
				agentsCopy = make(map[string]bool, len(state.Agents))
				for ak, av := range state.Agents {
					agentsCopy[ak] = av
				}
			}
			c.skillUsage[name] = &skillUsageState{
				Name:   state.Name,
				Count:  state.Count,
				Agents: agentsCopy,
			}
		}
	}
	isolatedCoord.skillUsageMu.Unlock()
	c.skillUsageMu.Unlock()

	return result, err
}

// cloneCoordinator creates a Coordinator copy sharing all references except session.
// Mutex/atomic fields are zero-initialized (each clone gets independent locks).
// This avoids the go vet "copies lock value" warning and prevents data races
// on lock state while keeping all shared resources accessible.
func cloneCoordinator(orig *Coordinator, newSession *TeamSession) *Coordinator {
	// Deep copy/clone all shared mutable maps and slices under their original locks
	var agentCacheClone map[string]fantasy.Agent
	orig.agentCacheMu.RLock()
	if orig.agentCache != nil {
		agentCacheClone = make(map[string]fantasy.Agent, len(orig.agentCache))
		for k, v := range orig.agentCache {
			agentCacheClone[k] = v
		}
	}
	orig.agentCacheMu.RUnlock()

	var conversationHistoryClone []fantasy.Message
	orig.conversationHistoryMu.Lock()
	if orig.conversationHistory != nil {
		conversationHistoryClone = make([]fantasy.Message, len(orig.conversationHistory))
		copy(conversationHistoryClone, orig.conversationHistory)
	}
	orig.conversationHistoryMu.Unlock()

	var skillsClone []*skill.SkillDef
	orig.skillsMu.RLock()
	if orig.skills != nil {
		skillsClone = make([]*skill.SkillDef, len(orig.skills))
		copy(skillsClone, orig.skills)
	}
	orig.skillsMu.RUnlock()

	var autoLoadedSkillsClone []*skill.SkillDef
	orig.autoLoadedSkillsMu.RLock()
	if orig.autoLoadedSkills != nil {
		autoLoadedSkillsClone = make([]*skill.SkillDef, len(orig.autoLoadedSkills))
		copy(autoLoadedSkillsClone, orig.autoLoadedSkills)
	}
	orig.autoLoadedSkillsMu.RUnlock()

	var skillUsageClone map[string]*skillUsageState
	orig.skillUsageMu.Lock()
	if orig.skillUsage != nil {
		skillUsageClone = make(map[string]*skillUsageState, len(orig.skillUsage))
		for k, v := range orig.skillUsage {
			var agentsCopy map[string]bool
			if v.Agents != nil {
				agentsCopy = make(map[string]bool, len(v.Agents))
				for ak, av := range v.Agents {
					agentsCopy[ak] = av
				}
			}
			skillUsageClone[k] = &skillUsageState{
				Name:   v.Name,
				Count:  v.Count,
				Agents: agentsCopy,
			}
		}
	}
	orig.skillUsageMu.Unlock()

	var delegatedTasksClone map[string]int
	orig.delegatedTasksMu.Lock()
	if orig.delegatedTasks != nil {
		delegatedTasksClone = make(map[string]int, len(orig.delegatedTasks))
		for k, v := range orig.delegatedTasks {
			delegatedTasksClone[k] = v
		}
	}
	orig.delegatedTasksMu.Unlock()

	var taskResultCacheClone map[string][]cachedTaskEntry
	orig.taskResultCacheMu.RLock()
	if orig.taskResultCache != nil {
		taskResultCacheClone = make(map[string][]cachedTaskEntry, len(orig.taskResultCache))
		for k, v := range orig.taskResultCache {
			var sliceCopy []cachedTaskEntry
			if v != nil {
				sliceCopy = make([]cachedTaskEntry, len(v))
				copy(sliceCopy, v)
			}
			taskResultCacheClone[k] = sliceCopy
		}
	}
	orig.taskResultCacheMu.RUnlock()

	var forcedSkillNamesClone map[string]bool
	if orig.forcedSkillNames != nil {
		forcedSkillNamesClone = make(map[string]bool, len(orig.forcedSkillNames))
		for k, v := range orig.forcedSkillNames {
			forcedSkillNamesClone[k] = v
		}
	}

	var pendingPlansClone map[string]*PlanEntry
	var approvedOutputsClone map[string]string
	var approvedErrorsClone map[string]error
	orig.pendingPlansMu.Lock()
	if orig.pendingPlans != nil {
		pendingPlansClone = make(map[string]*PlanEntry, len(orig.pendingPlans))
		for k, v := range orig.pendingPlans {
			pendingPlansClone[k] = &PlanEntry{
				TodoID:      v.TodoID,
				Agent:       v.Agent,
				Goal:        v.Goal,
				PlanText:    v.PlanText,
				Status:      v.Status,
				ReviewCount: v.ReviewCount,
				Task:        cloneTaskDef(v.Task),
			}
		}
	}
	if orig.approvedOutputs != nil {
		approvedOutputsClone = make(map[string]string, len(orig.approvedOutputs))
		for k, v := range orig.approvedOutputs {
			approvedOutputsClone[k] = v
		}
	}
	if orig.approvedErrors != nil {
		approvedErrorsClone = make(map[string]error, len(orig.approvedErrors))
		for k, v := range orig.approvedErrors {
			approvedErrorsClone[k] = v
		}
	}
	orig.pendingPlansMu.Unlock()

	var sessionToolPermissionsClone map[string]bool
	orig.sessionToolPermissionsMu.RLock()
	if orig.sessionToolPermissions != nil {
		sessionToolPermissionsClone = make(map[string]bool, len(orig.sessionToolPermissions))
		for k, v := range orig.sessionToolPermissions {
			sessionToolPermissionsClone[k] = v
		}
	}
	orig.sessionToolPermissionsMu.RUnlock()

	var workerSummariesClone map[string]string
	orig.workerSummariesMu.Lock()
	if orig.workerSummaries != nil {
		workerSummariesClone = make(map[string]string, len(orig.workerSummaries))
		for k, v := range orig.workerSummaries {
			workerSummariesClone[k] = v
		}
	}
	orig.workerSummariesMu.Unlock()

	orig.sidecarInitMu.Lock()
	sidecarInitCopy := orig.sidecarInit
	sidecarInstCopy := orig.sidecarInst
	orig.sidecarInitMu.Unlock()

	orig.guardInitMu.Lock()
	guardInitCopy := orig.guardInit
	guardInstCopy := orig.guardInst
	orig.guardInitMu.Unlock()

	var sessionDataClone *SessionData
	if orig.sessionData != nil {
		var entriesCopy []SessionEntry
		if orig.sessionData.Entries != nil {
			entriesCopy = make([]SessionEntry, len(orig.sessionData.Entries))
			copy(entriesCopy, orig.sessionData.Entries)
		}
		sessionDataClone = &SessionData{
			CreatedAt: orig.sessionData.CreatedAt,
			UpdatedAt: orig.sessionData.UpdatedAt,
			Rounds:    orig.sessionData.Rounds,
			Entries:   entriesCopy,
		}
	}

	var coreToolsClone []fantasy.AgentTool
	if orig.coreTools != nil {
		coreToolsClone = make([]fantasy.AgentTool, len(orig.coreTools))
		copy(coreToolsClone, orig.coreTools)
	}

	orig.stepConfirmFnMu.RLock()
	stepConfirmFnCopy := orig.stepConfirmFn
	orig.stepConfirmFnMu.RUnlock()

	return &Coordinator{
		session:                newSession,
		providerManager:        orig.providerManager,
		mcpManager:             orig.mcpManager,
		coreTools:              coreToolsClone,
		agentCache:             agentCacheClone,
		round:                  orig.round,
		verbose:                orig.verbose,
		think:                  orig.think,
		reportStatus:           orig.reportStatus,
		sessionData:            sessionDataClone,
		taskTracker:            orig.taskTracker,
		skills:                 skillsClone,
		conversationHistory:    conversationHistoryClone,
		projectDir:             orig.projectDir,
		auditLogger:            orig.auditLogger,
		sshSessionMgr:          orig.sshSessionMgr,
		skillUsage:             skillUsageClone,
		delegatedTasks:         delegatedTasksClone,
		taskResultCache:        taskResultCacheClone,
		memoryStore:            orig.memoryStore,
		modelList:              orig.modelList,
		sidecarModel:           orig.sidecarModel,
		sidecarInst:            sidecarInstCopy,
		sidecarInit:            sidecarInitCopy,
		guardModel:             orig.guardModel,
		guardInst:              guardInstCopy,
		guardInit:              guardInitCopy,
		cachedWorkerContext:    orig.cachedWorkerContext,
		autoLoadedSkills:       autoLoadedSkillsClone,
		forcedSkillNames:       forcedSkillNamesClone,
		maxConcurrent:          orig.maxConcurrent,
		sessionTime:            orig.sessionTime,
		skillDetector:          orig.skillDetector,
		skillGenerator:         orig.skillGenerator,
		skillPatternsDetected:  orig.skillPatternsDetected,
		hooks:                  orig.hooks,
		rbashMode:              orig.rbashMode,
		restrictedPath:         orig.restrictedPath,
		noNet:                  orig.noNet,
		forceMCP:               orig.forceMCP,
		pendingPlans:           pendingPlansClone,
		approvedOutputs:        approvedOutputsClone,
		approvedErrors:         approvedErrorsClone,
		forcePlanFirst:         orig.forcePlanFirst,
		autoSkillsEnabled:      orig.autoSkillsEnabled,
		sessionToolPermissions: sessionToolPermissionsClone,
		workerSummaries:        workerSummariesClone,
		stepConfirmFn:          stepConfirmFnCopy,
	}
}

// sanitizeModel removes characters unsafe for file paths from a model name.
func sanitizeModel(model string) string {
	s := strings.ReplaceAll(model, "/", "-")
	s = safeNameRegex.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "default"
	}
	return s
}

// copyDir recursively copies a directory tree, preserving file permissions and symlinks.
func copyDir(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		// Get full file info to preserve permissions
		info, err := entry.Info()
		if err != nil {
			return err
		}

		// Handle symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.Symlink(linkTarget, dstPath); err != nil {
				return err
			}
			continue
		}

		// Handle directories
		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}

		// Handle regular files with original permissions
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dstPath, data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// mergeAgentResults concatenates outputs from multiple models
func (c *Coordinator) mergeAgentResults(results []*agentResult) string {
	var b strings.Builder

	b.WriteString("## Multi-Model Response Integration\n\n")

	for _, r := range results {
		modelLabel := r.model
		if modelLabel == "" {
			modelLabel = "main"
		}

		fmt.Fprintf(&b, "### Model: %s\n\n", modelLabel)

		if r.err != nil {
			fmt.Fprintf(&b, "*Error: %v*\n\n", r.err)
		} else {
			b.WriteString(r.output)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("---\n")

	return b.String()
}

// checkSkillPatternsAndSave checks for patterns and auto-generates skill drafts (requires user confirmation)
func (c *Coordinator) checkSkillPatternsAndSave() []string {
	if c.skillDetector == nil || c.skillGenerator == nil {
		return nil
	}

	candidates := c.skillDetector.FindCandidates(context.Background())
	if len(candidates) == 0 {
		return nil
	}

	// Auto-promotion: top-tier candidates (quality >= 0.95, count >= 15) are
	// generated and promoted automatically without user confirmation. This
	// eliminates manual intervention for obviously high-quality patterns.
	const autoPromoteQuality = 0.95
	const autoPromoteCount = 15
	var autoPromoted []string
	var needConfirmation []skill.PatternCandidate
	for _, cand := range candidates {
		if cand.QualityScore >= autoPromoteQuality && cand.Sequence.Count >= autoPromoteCount {
			path, err := c.skillGenerator.GenerateSkill(cand)
			if err != nil {
				log.Printf("[WARN] auto-promote: failed to generate skill draft: %v", err)
				continue
			}
			// Try to promote from drafts/ to the team skill dir
			skillsDir := filepath.Dir(filepath.Dir(path)) // drafts/<name>/SKILL.md -> drafts -> skills
			skillName := filepath.Base(filepath.Dir(path))
			promoted, promErr := skill.PromoteDraft(skillsDir, skillName)
			if promErr != nil {
				log.Printf("[INFO] auto-promote: draft saved but promote failed (may already exist): %v", promErr)
				autoPromoted = append(autoPromoted, path)
			} else {
				autoPromoted = append(autoPromoted, promoted)
				c.report(c.newEvent("step").withMessage(fmt.Sprintf(
					"⚡ Auto-promoted skill: %s (quality %.2f, ×%d)",
					skillName, cand.QualityScore, cand.Sequence.Count)))
			}
		} else {
			needConfirmation = append(needConfirmation, cand)
		}
	}

	var savedSkills []string
	savedSkills = append(savedSkills, autoPromoted...)

	// Multi-select confirm remaining candidates: user picks which drafts to keep.
	if len(needConfirmation) > 0 {
		selected := c.displaySkillPreviewMultiSelect(needConfirmation)
		for _, cand := range selected {
			path, err := c.skillGenerator.GenerateSkill(cand)
			if err != nil {
				log.Printf("[WARN] failed to generate skill draft: %v", err)
				continue
			}
			savedSkills = append(savedSkills, path)
		}
	}

	return savedSkills
}

// displaySkillPreviewMultiSelect shows the candidate list and asks the user
// to pick which drafts to generate. Returns the filtered list.
// Returns nil if the user declines all (empty input, "n", or invalid).
// Uses the TUI ask_user infrastructure when available (so it works in
// Bubble Tea mode); falls back to a stderr prompt + stdin read otherwise.
func (c *Coordinator) displaySkillPreviewMultiSelect(candidates []skill.PatternCandidate) []skill.PatternCandidate {
	var msg strings.Builder
	msg.WriteString("\n─── SKILL GENERATION PREVIEW ───\n")
	fmt.Fprintf(&msg, "Detected %d high-quality patterns:\n\n", len(candidates))

	for i, cand := range candidates {
		fmt.Fprintf(&msg, "%d. **%s** (quality %.2f, ×%d)\n",
			i+1, cand.SuggestedName, cand.QualityScore, cand.Sequence.Count)
		fmt.Fprintf(&msg, "   Tools: %s\n", strings.Join(cand.Sequence.Tools, " → "))
		if cand.GeneralizationReason != "" {
			fmt.Fprintf(&msg, "   %s\n", cand.GeneralizationReason)
		}
		msg.WriteString("\n")
	}

	msg.WriteString("Keep which drafts? Type numbers (e.g. \"1,3\"), \"a\" for all, \"n\" for none.\n")
	msg.WriteString("Default: n. ")

	// Try the TUI path first. It is a no-op when no TUI callback is registered
	// (i.e. plain CLI mode) and returns ok=false; we then fall back to stdin.
	response, handled := tools.TryAskUserTUI(context.Background(), msg.String(), "free_text", nil, true)
	if !handled {
		fmt.Fprint(os.Stderr, msg.String())
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(line))
	} else {
		response = strings.TrimSpace(strings.ToLower(response))
	}

	if response == "" || response == "n" {
		return nil
	}
	if response == "a" {
		return candidates
	}

	parts := strings.Split(response, ",")
	seen := map[int]bool{}
	var selected []skill.PatternCandidate
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > len(candidates) {
			log.Printf("[WARN] invalid selection %q, ignoring", p)
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		selected = append(selected, candidates[n-1])
	}
	return selected
}

func (c *Coordinator) executeTask(parentCtx context.Context, task TaskDef, todoID string) (string, error) {
	taskDesc := task.Goal
	if task.Constraints != "" {
		taskDesc += "\nconstraints: " + task.Constraints
	}

	agentDef, _, err := c.resolveAgentName(task.Agent)
	if err != nil {
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
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
		return c.executeTaskWithExtraModels(parentCtx, agentName, agentDef, taskDesc, todoID, task.Verify)
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
			c.report(c.newEvent("error").withAgent(agentName).withMessage(planErr.Error()).withTodoID(todoID))
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, planErr.Error())
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			_ = writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, "")
			_ = writeStatus(c.session.Workspace, agentName, "error", taskDesc)
			return "", planErr
		}
		ag = planAg
	} else {
		var err error
		ag, err = c.getOrCreateAgent(parentCtx, agentDef, task.Model)
		if err != nil {
			c.report(c.newEvent("error").withAgent(agentName).withMessage(err.Error()).withTodoID(todoID))
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, err.Error())
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			_ = writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, "")
			_ = writeStatus(c.session.Workspace, agentName, "error", taskDesc)
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
	for attempt := 1; attempt <= maxRetries; attempt++ {
		currentPrompt := prompt
		if attempt > 1 {
			c.report(c.newEvent("step").withAgent(agentName).withMessage(fmt.Sprintf("retry %d/%d — continuing from previous progress", attempt, maxRetries)))
			if lastErr != nil {
				if hint := c.reflectOnFailure(parentCtx, agentName, task.Goal, lastErr.Error()); hint != "" {
					currentPrompt += hint
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
			c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("repeated failure after %d attempts: %v", attempt, err))
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			break
		}

		lastErr = err
		c.report(c.newEvent("error").withAgent(agentName).withMessage(fmt.Sprintf("attempt %d failed: %v", attempt, err)).withModel(resolvedModel).withTodoID(todoID))
		c.taskTracker.TodoList().UpdateStatus(todoID, TaskError, fmt.Sprintf("attempt %d failed: %v", attempt, err))
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

		if parentCtx.Err() != nil {
			break
		}
	}

	_, modelTime, toolTime := timing.snapshot()
	c.updateTodoTiming(todoID, modelTime, toolTime)
	if err := writeTaskFile(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, ""); err != nil {
		log.Printf("warning: failed to write task file: %v", err)
	}
	_ = writeStatus(c.session.Workspace, agentName, "error", taskDesc)
	c.autoWriteSTMASync(agentName, taskDesc, "", lastErr.Error(), false)
	return "", fmt.Errorf("agent %q failed after %d attempts (model: %s): %w", agentName, maxRetries, resolvedModel, lastErr)
}

func (c *Coordinator) runAgentWithStatus(ctx context.Context, ag fantasy.Agent, agentName, prompt string, timing *taskTiming) (string, error) {
	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, agentName, prompt, nil, timing)
	return output, err
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

	var lastToolCall *lastToolCallEntry
	consecutiveErrCount := 0

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
			if lastToolCall != nil && lastToolCall.toolName == tc.ToolName && lastToolCall.input == tc.Input {
				if consecutiveErrCount >= 2 {
					return fmt.Errorf("agent %s is stuck in a loop executing the same failing command: %s (args: %s)", agentName, tc.ToolName, argsPreview)
				}
			} else {
				lastToolCall = &lastToolCallEntry{
					toolName: tc.ToolName,
					input:    tc.Input,
				}
				consecutiveErrCount = 0
			}

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
				if txt, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Result); ok {
					resultPreview = txt.Text
				}
			}
			resolvedModel, _ := ctx.Value(modelKey{}).(string)
			reportFn(c.newEvent("tool_result").withAgent(agentName).withTodoID(todoID).withToolResult(tr.ToolName, resultPreview).withModel(resolvedModel))
			llmLogStreamEvent(logWrite, "tool_result", formatToolResultContent(tr))
			_, isErrResult := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result)
			audit.LogToolResult(agentName, tr.ToolName, resultPreview, isErrResult)
			c.SetCurrentTool("")

			// 🔁 Track error count
			if lastToolCall != nil && lastToolCall.toolName == tr.ToolName {
				if isErrResult {
					consecutiveErrCount++
				} else {
					consecutiveErrCount = 0
				}
			}

			c.saveCheckpoint()
			return nil
		},
		OnTextDelta: func(id, text string) error {
			eventType := "text"
			if c.think {
				eventType = "think_text"
			}
			reportFn(c.newEvent(eventType).withAgent(agentName).withTodoID(todoID).withMessage(text))
			logWrite(text)
			return nil
		},
		OnReasoningDelta: func(id, text string) error {
			logWrite(text)
			return nil
		},
		OnStreamFinish: func(usage fantasy.Usage, finishReason fantasy.FinishReason, providerMetadata fantasy.ProviderMetadata) error {
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
	c.SetCurrentTool("")
	if result != nil {
		c.addStepTokens(result.Steps)
	}
	if err != nil {
		return "", nil, err
	}
	return result.Response.Content.Text(), result.Steps, nil
}

func agentCacheKey(def *agent.AgentDef, overrideModel string) string {
	if overrideModel != "" {
		return def.Name + "|" + overrideModel
	}
	return def.Name
}

func (c *Coordinator) getOrCreateAgent(ctx context.Context, def *agent.AgentDef, overrideModel string) (fantasy.Agent, error) {
	cacheKey := agentCacheKey(def, overrideModel)
	c.agentCacheMu.RLock()
	if ag, ok := c.agentCache[cacheKey]; ok {
		c.agentCacheMu.RUnlock()
		return ag, nil
	}
	c.agentCacheMu.RUnlock()

	c.agentCacheMu.Lock()
	defer c.agentCacheMu.Unlock()

	if ag, ok := c.agentCache[cacheKey]; ok {
		return ag, nil
	}

	agentDef := def
	if overrideModel != "" {
		overriddenDef := *def
		overriddenDef.Generation.Model = overrideModel
		agentDef = &overriddenDef
	}

	agentDef = c.injectWorkerContext(ctx, agentDef)

	// Inject SSH session manager into context
	ctx = tools.SetSSHSessionManager(ctx, c.sshSessionMgr)

	agentTools := agent.SelectTools(c.coreTools, agentDef.Tools)
	if c.mcpManager != nil {
		agentTools = append(agentTools, c.mcpManager.AsAgentTools()...)

		// Load agent-specific MCP tools if defined
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

	getAgModelID := c.resolveAgentModel(agentDef, "")
	ag, err := agent.CreateAgent(ctx, c.providerManager.GetProvider(getAgModelID), agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   agent.DefaultMaxSteps,
	}, agentTools)
	if err != nil {
		return nil, err
	}

	c.agentCache[cacheKey] = ag
	return ag, nil
}

// resolveAgentName resolves an agent name (exact, case-insensitive, or fuzzy match)
// to its AgentDef.
//
// Thread Safety: c.session is set once in NewCoordinator and never modified.
// c.session.Agents is populated during team loading and remains read-only during
// execution. Therefore, no mutex protection is needed for reading session fields.
// If future changes require runtime mutation of session, add RWMutex protection.
func (c *Coordinator) resolveAgentName(input string) (*agent.AgentDef, string, error) {
	if c.session == nil {
		return nil, "", fmt.Errorf("session not initialized")
	}
	key := strings.ToLower(input)
	if def, ok := c.session.Agents[key]; ok {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			return nil, "", fmt.Errorf("cannot delegate to coordinator agent %q", input)
		}
		return def, key, nil
	}

	var matches []*agent.AgentDef
	seenNames := make(map[string]bool)
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seenNames[def.Name] {
			continue
		}
		// Match the full display name case-insensitively (e.g. "Helper" → "helper")
		if strings.ToLower(def.Name) == key {
			matches = append(matches, def)
			seenNames[def.Name] = true
			continue
		}
		for _, word := range strings.Fields(def.Name) {
			if strings.ToLower(word) == key {
				matches = append(matches, def)
				seenNames[def.Name] = true
				break
			}
		}
		if seenNames[def.Name] {
			continue
		}
		if def.FileAlias != "" {
			for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
				if seg != "" && seg == key {
					matches = append(matches, def)
					seenNames[def.Name] = true
					break
				}
			}
		}
	}

	if len(matches) == 1 {
		def := matches[0]
		return def, strings.ToLower(def.Name), nil
	}
	if len(matches) > 1 {
		var names []string
		for _, m := range matches {
			names = append(names, m.Name)
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("ambiguous agent name %q matches multiple agents: %v", input, names)
	}

	var available []string
	seenAvail := make(map[string]bool)
	for _, def := range c.session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" && !seenAvail[def.Name] {
			seenAvail[def.Name] = true
			available = append(available, def.Name)
		}
	}
	sort.Strings(available)
	return nil, "", fmt.Errorf("unknown agent %q (available: %v)", input, available)
}

func (c *Coordinator) uniqueWorkerDefs() []*agent.AgentDef {
	seen := make(map[string]bool)
	var defs []*agent.AgentDef
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		defs = append(defs, def)
	}
	return defs
}

func (c *Coordinator) buildWorkerNamesAndDescs() (names []string, descs []string) {
	for _, def := range c.uniqueWorkerDefs() {
		desc := def.Name
		aliases := c.agentAliasesFor(def)
		if aliases != "" {
			desc += " (aliases: " + aliases + ")"
		}
		if def.Description != "" {
			desc += ": " + def.Description
		}
		if def.Tools != "" {
			desc += fmt.Sprintf(" (tools: %s)", def.Tools)
		}
		names = append(names, def.Name)
		descs = append(descs, desc)
	}
	return names, descs
}

func (c *Coordinator) agentAliasesFor(def *agent.AgentDef) string {
	nameLower := strings.ToLower(def.Name)
	var parts []string
	if def.FileAlias != "" && strings.ToLower(def.FileAlias) != nameLower {
		parts = append(parts, def.FileAlias)
	}
	for _, word := range strings.Fields(def.Name) {
		w := strings.ToLower(word)
		if w != nameLower && !containsPart(parts, w) {
			parts = append(parts, w)
		}
	}
	if def.FileAlias != "" {
		for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
			if seg != nameLower && seg != "" && !containsPart(parts, seg) {
				parts = append(parts, seg)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func containsPart(parts []string, s string) bool {
	for _, p := range parts {
		if p == s {
			return true
		}
	}
	return false
}

func (c *Coordinator) workerNameList() []string {
	names, _ := c.buildWorkerNamesAndDescs()
	return names
}

const maxAgentsMDSize = 50000
const defaultWorkerContextSize = 12000

func (c *Coordinator) loadProjectContext() string {
	path := filepath.Join(c.projectDir, "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if utf8.RuneCountInString(string(data)) > maxAgentsMDSize {
		runes := []rune(string(data))
		data = []byte(string(runes[:maxAgentsMDSize]) + "\n\n... [AGENTS.md truncated]")
	}
	return string(data)
}

func (c *Coordinator) getWorkerContext(ctx context.Context) string {
	c.workerCtxOnce.Do(func() {
		raw := c.loadProjectContext()
		if raw == "" {
			return
		}
		ctxSize := c.getWorkerContextSize()
		if s := c.Sidecar(); s != nil && utf8.RuneCountInString(raw) > ctxSize/2 {
			if c.think {
				c.emitThinkSidecar("Compact", "compacting worker context (AGENTS.md)")
			}
			compacted, err := s.Compact(ctx, raw, "Extract the essential project context: tech stack, language, framework, key conventions, and directory structure. Preserve all factual details but remove verbose descriptions.")
			if err == nil && compacted != "" {
				raw = compacted
			}
		}
		if utf8.RuneCountInString(raw) > ctxSize {
			raw = string([]rune(raw)[:ctxSize]) + "\n\n... [truncated]"
		}
		c.cachedWorkerContext = raw
	})
	return c.cachedWorkerContext
}

func (c *Coordinator) getWorkerContextSize() int {
	if c.session != nil && c.session.Config.WorkerContextSize > 0 {
		return c.session.Config.WorkerContextSize
	}
	return defaultWorkerContextSize
}

func (c *Coordinator) getWorkerSummary(name string) string {
	c.workerSummariesMu.Lock()
	defer c.workerSummariesMu.Unlock()
	if c.workerSummaries == nil {
		return ""
	}
	return c.workerSummaries[name]
}

func (c *Coordinator) computeWorkerSummaries(ctx context.Context) {
	c.workerSummariesOnce.Do(func() {
		c.workerSummaries = make(map[string]string)
		for _, def := range c.uniqueWorkerDefs() {
			if def.System == "" {
				continue
			}
			c.workerSummariesMu.Lock()
			summary := c.summarizeSystem(ctx, def.System)
			c.workerSummaries[def.Name] = summary
			c.workerSummariesMu.Unlock()
		}
	})
}

func (c *Coordinator) summarizeSystem(ctx context.Context, system string) string {
	if s := c.Sidecar(); s != nil {
		if c.think {
			c.emitThinkSidecar("Compact", "summarizing worker system prompt for coordinator")
		}
		compacted, err := s.Compact(ctx, system, "Summarize this agent's role, key behavioral guidelines, and unique instructions in 2-3 concise sentences. Preserve what makes this agent distinct.")
		if err == nil && compacted != "" {
			return compacted
		}
	}
	if utf8.RuneCountInString(system) > 500 {
		runes := []rune(system)
		return string(runes[:500]) + "..."
	}
	return system
}

func (c *Coordinator) buildCoreReminder(def *agent.AgentDef) string {
	if def.System == "" {
		return ""
	}
	lines := strings.Split(def.System, "\n")
	var keyLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		keyLines = append(keyLines, trimmed)
		if len(keyLines) >= 5 {
			break
		}
	}
	if len(keyLines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Core Instructions Reminder\n\n")
	fmt.Fprintf(&b, "You are **%s**", def.Name)
	if def.Description != "" {
		fmt.Fprintf(&b, ": %s", def.Description)
	}
	b.WriteString(". Your core directives:\n")
	for _, line := range keyLines {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	b.WriteString("\nFollow these as your primary directive — they define your identity and priorities.\n")
	return b.String()
}

// injectWorkerContext prepends the project context to the agent's system prompt
// if the agent is not an orchestrator/coordinator and a worker context is available.
// It returns a new AgentDef pointer (either the original unchanged or a copy with
// the modified System field) to avoid mutating shared state.
func (c *Coordinator) injectWorkerContext(ctx context.Context, def *agent.AgentDef) *agent.AgentDef {
	if def.Role == "orchestrator" || def.Role == "coordinator" {
		return def
	}
	wc := c.getWorkerContext(ctx)

	var b strings.Builder
	b.WriteString("## Project Context\n\n")
	if wc != "" {
		b.WriteString(wc)
		b.WriteString("\n\n---\n\n")
	}
	wsPath := c.session.Workspace
	sharedPath := filepath.Join(wsPath, sharedDir)
	b.WriteString("## Environment & Rules\n\n")
	fmt.Fprintf(&b, "- CWD: %s | Workspace: %s | Shared: %s | Time: %s\n", c.projectDir, wsPath, sharedPath, c.sessionTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "- ALL intermediate files (drafts, logs, notes, etc.) MUST be written to workspace: %s\n", wsPath)
	fmt.Fprintf(&b, "- Use %s to share files between agents. NEVER write outside workspace.\n\n", sharedPath)
	b.WriteString("---\n\n")

	if memSuffix := c.buildMemorySuffix(def.Role); memSuffix != "" {
		b.WriteString(memSuffix)
		b.WriteString("\n")
	}

	injectedDef := *def
	injectedDef.System = injectedDef.System + "\n\n---\n\n" + b.String()

	if reminder := c.buildCoreReminder(def); reminder != "" {
		injectedDef.System += "\n\n" + reminder
	}

	return &injectedDef
}
