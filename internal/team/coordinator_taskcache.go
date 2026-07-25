package team

// Task result cache and duplicate-delegation detection, plus result
// formatting for the orchestrator.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

type agentTaskResult struct {
	agentName string
	todoID    string
	task      string
	output    string
	err       error
	planText  string
	idx       int
}

// cachedTaskEntry stores a previously completed task and its output for dedup.
type cachedTaskEntry struct {
	taskDesc   string
	verify     string
	verifyMode string
	output     string
	generation int64 // cacheGeneration at time of storage
	// pinned marks entries restored from a previous run (session.json or the
	// task journal); the per-round generation prune keeps them so they survive
	// until their first lookup. invalidateTaskCache removes them regardless.
	pinned   bool
	identity CacheIdentity
}

type duplicateTodoMatch struct {
	Item   *TodoItem
	Reason string
}

func normalizeTaskCacheKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// normalizeVerifyMode gives equivalent default and explicit success modes one
// cache identity while keeping all other verification semantics distinct.
func normalizeVerifyMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "success":
		return "success"
	default:
		return strings.ToLower(strings.TrimSpace(mode))
	}
}

func taskCacheIdentity(taskDesc, verify, verifyMode string) string {
	return normalizeTaskCacheKey(taskDesc) + "\nverify:" + normalizeTaskCacheKey(verify) + "\nverify_mode:" + normalizeVerifyMode(verifyMode)
}

func (e cachedTaskEntry) matches(taskDesc, verify, verifyMode string) bool {
	return normalizeTaskCacheKey(e.taskDesc) == normalizeTaskCacheKey(taskDesc) &&
		normalizeTaskCacheKey(e.verify) == normalizeTaskCacheKey(verify) &&
		normalizeVerifyMode(e.verifyMode) == normalizeVerifyMode(verifyMode)
}

// lookupTaskCache checks whether newTask has a semantically equivalent prior
// result for agentKey (lowercase agent name).
//
// Lookup order:
//  1. Exact match in current generation (fast path, same workspace state)
//  2. Exact match across all generations (fast path, workspace state may differ but goal is identical)
//  3. Sidecar semantic similarity in current generation only (slower, requires LLM call)
//
// Returns (cachedOutput, true) on a hit.
func (c *Coordinator) lookupTaskCache(ctx context.Context, agentKey, newTask string) (string, bool) {
	return c.lookupTaskCacheWithVerify(ctx, agentKey, newTask, "")
}

func (c *Coordinator) lookupTaskCacheWithVerify(ctx context.Context, agentKey, newTask, verify string) (string, bool) {
	return c.lookupTaskCacheWithVerification(ctx, agentKey, newTask, verify, "")
}

func (c *Coordinator) lookupTaskCacheWithVerification(ctx context.Context, agentKey, newTask, verify, verifyMode string) (string, bool) {
	policy := c.PolicyEngine().GetCachePolicy()
	if policy == CacheBypass || policy == CacheRefresh {
		return "", false
	}
	if c.IsCacheForbidden(newTask, verify) {
		return "", false
	}

	target := c.ComputeCacheIdentity(agentKey, newTask, verify, verifyMode)
	gen := c.cacheGeneration.Load()

	c.taskResultCacheMu.RLock()
	all := c.taskResultCache[agentKey]
	c.taskResultCacheMu.RUnlock()

	if len(all) == 0 {
		return "", false
	}

	// Step 1: exact match in current generation (newest entry first)
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if e.generation == gen && e.matches(newTask, verify, verifyMode) && c.PolicyEngine().IsCacheFresh(e, target) {
			return e.output, true
		}
	}

	// Step 2: exact match across all generations (newest entry first)
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if e.matches(newTask, verify, verifyMode) && c.PolicyEngine().IsCacheFresh(e, target) {
			return e.output, true
		}
	}

	// Step 3: sidecar semantic similarity in current generation only
	s := c.AgentPool().Sidecar()
	if s == nil {
		return "", false
	}

	var currentGenEntries []cachedTaskEntry
	for _, e := range all {
		if e.generation == gen && normalizeTaskCacheKey(e.verify) == normalizeTaskCacheKey(verify) && normalizeVerifyMode(e.verifyMode) == normalizeVerifyMode(verifyMode) && c.PolicyEngine().IsCacheFresh(e, target) {
			currentGenEntries = append(currentGenEntries, e)
		}
	}
	if len(currentGenEntries) == 0 {
		return "", false
	}

	pastDescs := make([]string, len(currentGenEntries))
	for i, e := range currentGenEntries {
		pastDescs[i] = e.taskDesc
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if c.think {
		c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking cache for semantically similar task: %.50s", newTask))
	}
	idx, err := s.SimilarTask(sidecarCtx, newTask, pastDescs)
	if err != nil {
		return "", false
	}
	if idx >= 0 && idx < len(currentGenEntries) {
		return currentGenEntries[idx].output, true
	}
	return "", false
}

// lookupTaskCacheAllGenerations checks for semantically similar tasks across ALL
// generations (not just current). This is used for duplicate detection before
// delegating tasks.
//
// Lookup order:
//  1. Exact match across all generations (fast path)
//  2. Sidecar semantic similarity across all generations (slower, requires LLM call)
//
// Returns (cachedOutput, cachedTaskDesc, true) on a hit.
func (c *Coordinator) lookupTaskCacheAllGenerations(ctx context.Context, agentKey, newTask string) (string, string, bool) {
	return c.lookupTaskCacheAllGenerationsWithVerify(ctx, agentKey, newTask, "")
}

func (c *Coordinator) lookupTaskCacheAllGenerationsWithVerify(ctx context.Context, agentKey, newTask, verify string) (string, string, bool) {
	return c.lookupTaskCacheAllGenerationsWithVerification(ctx, agentKey, newTask, verify, "")
}

func (c *Coordinator) lookupTaskCacheAllGenerationsWithVerification(ctx context.Context, agentKey, newTask, verify, verifyMode string) (string, string, bool) {
	c.taskResultCacheMu.RLock()
	all := c.taskResultCache[agentKey]
	c.taskResultCacheMu.RUnlock()

	return c.lookupTaskCacheIn(ctx, all, newTask, verify, verifyMode)
}

// lookupTaskCacheCurrentRunWithVerification is restricted to entries produced
// by this run: entries pinned from a previous
// run (session.json / task journal) are skipped. Duplicate *rejection* must
// use this variant — a user who explicitly asks to re-run a mission would
// otherwise have the new run's first task killed as a "duplicate" of work
// from the previous run. Cross-run reuse stays available through the
// non-restricted lookup, which returns the cached output instead of an error.
func (c *Coordinator) lookupTaskCacheCurrentRunWithVerification(ctx context.Context, agentKey, newTask, verify, verifyMode string) (string, string, bool) {
	c.taskResultCacheMu.RLock()
	all := c.taskResultCache[agentKey]
	c.taskResultCacheMu.RUnlock()

	thisRun := make([]cachedTaskEntry, 0, len(all))
	for _, e := range all {
		if !e.pinned {
			thisRun = append(thisRun, e)
		}
	}
	return c.lookupTaskCacheIn(ctx, thisRun, newTask, verify, verifyMode)
}

func (c *Coordinator) lookupTaskCacheIn(ctx context.Context, all []cachedTaskEntry, newTask, verify, verifyMode string) (string, string, bool) {
	policy := c.PolicyEngine().GetCachePolicy()
	if policy == CacheBypass || policy == CacheRefresh {
		return "", "", false
	}
	if c.IsCacheForbidden(newTask, verify) {
		return "", "", false
	}

	if len(all) == 0 {
		return "", "", false
	}

	target := c.ComputeCacheIdentity("", newTask, verify, verifyMode)

	// Step 1: exact match across all generations (newest entry first)
	for i := len(all) - 1; i >= 0; i-- {
		e := all[i]
		if e.matches(newTask, verify, verifyMode) && c.PolicyEngine().IsCacheFresh(e, target) {
			return e.output, e.taskDesc, true
		}
	}

	// Step 2: sidecar semantic similarity across all generations
	s := c.AgentPool().Sidecar()
	if s == nil {
		return "", "", false
	}

	// Limit to last 100 entries to avoid overwhelming the sidecar
	startIdx := 0
	if len(all) > 100 {
		startIdx = len(all) - 100
	}
	recentEntries := make([]cachedTaskEntry, 0, len(all)-startIdx)
	for _, e := range all[startIdx:] {
		if normalizeTaskCacheKey(e.verify) == normalizeTaskCacheKey(verify) && normalizeVerifyMode(e.verifyMode) == normalizeVerifyMode(verifyMode) && c.PolicyEngine().IsCacheFresh(e, target) {
			recentEntries = append(recentEntries, e)
		}
	}
	if len(recentEntries) == 0 {
		return "", "", false
	}

	pastDescs := make([]string, len(recentEntries))
	for i, e := range recentEntries {
		pastDescs[i] = e.taskDesc
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if c.think {
		c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking semantic similarity across all history: %.50s", newTask))
	}
	idx, err := s.SimilarTask(sidecarCtx, newTask, pastDescs)
	if err != nil {
		return "", "", false
	}
	if idx >= 0 && idx < len(recentEntries) {
		return recentEntries[idx].output, recentEntries[idx].taskDesc, true
	}
	return "", "", false
}

const maxTaskCacheEntries = 50

// storeTaskCache saves a completed task result so future similar tasks within
// the same coordinator round (same cacheGeneration) can skip re-execution.
func (c *Coordinator) storeTaskCache(agentKey, taskDesc, output string) {
	c.storeTaskCacheWithVerify(agentKey, taskDesc, "", output)
}

func (c *Coordinator) storeTaskCacheWithVerify(agentKey, taskDesc, verify, output string) {
	c.storeTaskCacheWithVerification(agentKey, taskDesc, verify, "", output)
}

func (c *Coordinator) storeTaskCacheWithVerification(agentKey, taskDesc, verify, verifyMode, output string) {
	if c.PolicyEngine().GetCachePolicy() == CacheBypass {
		return
	}
	gen := c.cacheGeneration.Load()
	identity := c.ComputeCacheIdentity(agentKey, taskDesc, verify, verifyMode)
	c.taskResultCacheMu.Lock()
	c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], cachedTaskEntry{
		taskDesc:   taskDesc,
		verify:     verify,
		verifyMode: normalizeVerifyMode(verifyMode),
		output:     output,
		generation: gen,
		identity:   identity,
	})
	if len(c.taskResultCache[agentKey]) > maxTaskCacheEntries {
		c.taskResultCache[agentKey] = c.taskResultCache[agentKey][1:]
	}
	c.taskResultCacheMu.Unlock()

	c.journalAppend(journalRecord{
		Op:                 "put",
		Agent:              agentKey,
		Desc:               taskDesc,
		Verify:             verify,
		VerifyMode:         normalizeVerifyMode(verifyMode),
		Output:             output,
		TS:                 time.Now().Format(time.RFC3339),
		Round:              c.round,
		RepoCommit:         identity.RepoCommit,
		ProjectFingerprint: identity.ProjectFingerprint,
		Identity:           &identity,
	})
}

// invalidateTaskCache removes all cached results for the given agent whose
// task description matches taskDesc (normalized), across all generations.
// This forces a genuine re-execution when an on_failure DAG loop resets a
// previously completed task — otherwise lookupTaskCache would serve the stale
// output and the retry would be a no-op.
func (c *Coordinator) invalidateTaskCache(agentKey, taskDesc string) {
	c.invalidateTaskCacheWithVerify(agentKey, taskDesc, "")
}

func (c *Coordinator) invalidateTaskCacheWithVerify(agentKey, taskDesc, verify string) {
	c.invalidateTaskCacheWithVerification(agentKey, taskDesc, verify, "")
}

func (c *Coordinator) invalidateTaskCacheWithVerification(agentKey, taskDesc, verify, verifyMode string) {
	normalized := normalizeTaskCacheKey(taskDesc)
	normalizedVerify := normalizeTaskCacheKey(verify)
	c.taskResultCacheMu.Lock()
	entries := c.taskResultCache[agentKey]
	fresh := entries[:0]
	for _, e := range entries {
		if normalizeTaskCacheKey(e.taskDesc) != normalized || normalizeTaskCacheKey(e.verify) != normalizedVerify || normalizeVerifyMode(e.verifyMode) != normalizeVerifyMode(verifyMode) {
			fresh = append(fresh, e)
		}
	}
	c.taskResultCache[agentKey] = fresh
	c.taskResultCacheMu.Unlock()

	// Tombstone so a restart cannot resurrect the invalidated result.
	c.journalAppend(journalRecord{Op: "del", Agent: agentKey, Desc: taskDesc, Verify: verify, VerifyMode: normalizeVerifyMode(verifyMode), TS: time.Now().Format(time.RFC3339)})
}

func (c *Coordinator) findExistingTodoDuplicate(ctx context.Context, agentKey, desc, verify, verifyMode string) *duplicateTodoMatch {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) == 0 {
		return nil
	}

	normalizedNew := taskCacheIdentity(desc, verify, verifyMode)
	exactEligible := make([]*TodoItem, 0, len(items))
	semanticEligible := make([]*TodoItem, 0, len(items))
	for _, item := range items {
		if item == nil || strings.ToLower(item.Agent) != agentKey {
			continue
		}

		switch item.Status {
		case TaskPending, TaskPlanned, TaskInProgress, TaskPaused:
			exactEligible = append(exactEligible, item)
			semanticEligible = append(semanticEligible, item)
		case TaskError:
			exactEligible = append(exactEligible, item)
			if isPermissionBlockedFailureDetail(item.Detail) {
				semanticEligible = append(semanticEligible, item)
			}
		}
	}

	for _, item := range exactEligible {
		if taskCacheIdentity(item.Desc, item.Verify, item.VerifyMode) == normalizedNew {
			return &duplicateTodoMatch{
				Item:   item,
				Reason: fmt.Sprintf("existing task %s already has status %s", item.ID, item.Status),
			}
		}
	}

	if len(semanticEligible) == 0 {
		return nil
	}

	s := c.AgentPool().Sidecar()
	if s == nil {
		return nil
	}

	pastDescs := make([]string, len(semanticEligible))
	for i, item := range semanticEligible {
		pastDescs[i] = taskCacheIdentity(item.Desc, item.Verify, item.VerifyMode)
	}

	sidecarCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if c.think {
		c.emitThinkSidecar("SimilarTask", fmt.Sprintf("checking todo similarity against active/failed tasks: %.50s", desc))
	}
	idx, err := s.SimilarTask(sidecarCtx, taskCacheIdentity(desc, verify, verifyMode), pastDescs)
	if err != nil || idx < 0 || idx >= len(semanticEligible) {
		return nil
	}

	item := semanticEligible[idx]
	reason := fmt.Sprintf("similar to existing task %s with status %s", item.ID, item.Status)
	if item.Status == TaskError && isPermissionBlockedFailureDetail(item.Detail) {
		reason = fmt.Sprintf("similar to blocked task %s; previous failure was permission-related", item.ID)
	}
	return &duplicateTodoMatch{Item: item, Reason: reason}
}

func (c *Coordinator) checkDuplicateTasks(ctx context.Context, tasks []TaskDef) ([]string, map[int]bool, map[int]*duplicateTodoMatch) {
	var warnings []string
	duplicates := make(map[int]bool)
	suppressed := make(map[int]*duplicateTodoMatch)

	// First pass: build local counts for this batch to handle duplicates within the batch
	localCounts := make(map[string]int)
	for _, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + normalizeTaskDesc(desc)
		localCounts[key]++
	}
	_ = localCounts

	c.delegatedTasksMu.Lock()
	// Second pass: check exact duplicates and increment global counts
	// Track how many we've seen in this batch so far (for in-batch dedup: first instance proceeds, rest are duplicates)
	batchSeen := make(map[string]int)
	for i, t := range tasks {
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + normalizeTaskDesc(desc)
		batchSeen[key]++

		// Check if this exact task was already delegated in a previous round
		if c.delegatedTasks[key] > 0 {
			warnings = append(warnings, fmt.Sprintf("EXACT DUPLICATE: %s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, c.delegatedTasks[key]+batchSeen[key]))
			duplicates[i] = true
			continue
		}

		// Check for duplicates within the current batch (first instance proceeds, rest are duplicates)
		if batchSeen[key] > 1 {
			warnings = append(warnings, fmt.Sprintf("EXACT DUPLICATE (in batch): %s (agent=%s, count=%d)", truncateTaskDesc(desc), t.Agent, batchSeen[key]))
			duplicates[i] = true
			continue
		}
	}

	// Increment global counts for all non-duplicate tasks
	for i, t := range tasks {
		if duplicates[i] {
			continue
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		key := strings.ToLower(t.Agent) + ":" + normalizeTaskDesc(desc)
		c.delegatedTasks[key]++
	}
	c.delegatedTasksMu.Unlock()

	// Third pass: current todo-list duplicate check (active work and recent failures).
	for i, t := range tasks {
		if duplicates[i] {
			continue
		}
		desc := t.Goal
		if t.Constraints != "" {
			desc += "\nconstraints: " + t.Constraints
		}
		agentKey := strings.ToLower(t.Agent)
		if match := c.findExistingTodoDuplicate(ctx, agentKey, desc, t.Verify, t.VerifyMode); match != nil {
			duplicates[i] = true
			suppressed[i] = match
			warnings = append(warnings, fmt.Sprintf("SUPPRESSED DUPLICATE: %s (agent=%s, %s)", truncateTaskDesc(desc), t.Agent, match.Reason))
		}
	}

	// Fourth pass: semantic duplicate check against completed history from
	// THIS run only. Entries pinned from a previous run must not reject new
	// tasks: "re-run the verification" would have its first task errored as a
	// duplicate of last run's work.
	if !c.ExecutionProfile().DisableSemanticDedup {
		for i, t := range tasks {
			if duplicates[i] {
				continue
			}
			desc := t.Goal
			if t.Constraints != "" {
				desc += "\nconstraints: " + t.Constraints
			}
			agentKey := strings.ToLower(t.Agent)
			dupCtx, dupCancel := context.WithTimeout(ctx, 5*time.Second)
			cachedOutput, cachedDesc, cacheOK := c.lookupTaskCacheCurrentRunWithVerification(dupCtx, agentKey, desc, t.Verify, t.VerifyMode)
			dupCancel()
			if cacheOK {
				warnings = append(warnings, fmt.Sprintf("SEMANTIC DUPLICATE: %s (similar to completed task: %q)", truncateTaskDesc(desc), truncateTaskDesc(cachedDesc)))
				duplicates[i] = true
				log.Printf("[WARN] duplicate task detected: agent=%q, task=%q, similar to=%q", t.Agent, desc, cachedDesc)
			} else {
				_ = cachedOutput
			}
		}
	}
	return warnings, duplicates, suppressed
}

func formatTaskResults(results []agentTaskResult, totalTasks int, duplicateWarnings []string) (string, error) {
	var b strings.Builder
	successCount := 0
	errorCount := 0
	planCount := 0
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n---\n\n")
		}
		if r.err != nil {
			errorCount++
			fmt.Fprintf(&b, "## Agent: %s\n**Status**: ERROR\n**Error**: %s", r.agentName, r.err)
		} else if r.planText != "" {
			// Plan submitted - don't count as success, just informational
			planCount++
			fmt.Fprintf(&b, "## Agent: %s\n**Status**: PLAN SUBMITTED\n**Todo ID**: %s\n\n%s", r.agentName, r.todoID, r.planText)
		} else {
			successCount++
			fmt.Fprintf(&b, "## Agent: %s\n**Status**: Success\n\n%s", r.agentName, r.output)
		}
	}
	summary := fmt.Sprintf("\n\n---\nSummary: %d/%d tasks completed successfully", successCount, totalTasks)
	if errorCount > 0 {
		summary += fmt.Sprintf(", %d failed", errorCount)
	}
	b.WriteString(summary)
	if len(duplicateWarnings) > 0 {
		b.WriteString("\n\n**Warning**: You have delegated the same task to the same agent multiple times. This suggests you may be stuck in a loop. Consider using a different approach, agent, or calling `finish` with your best answer so far:\n")
		for _, w := range duplicateWarnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	// Error only if all tasks failed AND no plans were submitted
	if successCount == 0 && errorCount > 0 && planCount == 0 {
		return b.String(), fmt.Errorf("all %d tasks failed", len(results))
	}
	return b.String(), nil
}

func truncateTaskDesc(task string) string {
	const maxLen = 80
	if utf8.RuneCountInString(task) > maxLen {
		runes := []rune(task)
		return string(runes[:maxLen])
	}
	return task
}

func normalizeTaskDesc(task string) string {
	return strings.Join(strings.Fields(strings.ToLower(task)), " ")
}
