package team

// Conversation history, session checkpointing, and interrupted-task resume.

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
)

const maxConversationHistory = 100
const compactHistoryThreshold = 80

// maxMessageSize remains the persistence guard for legacy history helpers.
// appendHistory itself keeps messages intact until VerifiedCompactor runs.
const maxMessageSize = 50000

func (c *Coordinator) checkpointSTM() {
	c.stmWriteMu.Lock()
	defer c.stmWriteMu.Unlock()

	workspace := c.session.Workspace
	content := LoadSTM(workspace)
	if content == "" {
		return
	}
	histDir := filepath.Join(workspace, stmLogsDir)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		log.Printf("warning: stm checkpoint dir creation failed: %v", err)
		return
	}
	// Cumulative round number: the per-run counter resets on continue/restart,
	// which used to make later runs overwrite earlier runs' snapshots.
	fname := fmt.Sprintf("stm_r%d.md", c.totalRounds())
	path := filepath.Join(histDir, fname)
	if err := AtomicWriteFile(path, []byte(content), 0o644); err != nil {
		log.Printf("warning: stm checkpoint write failed: %v", err)
	}
}

func (c *Coordinator) autoWriteSTMASync(agentName, taskDesc, output, errMsg string, success bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[PANIC] autoWriteSTMASync recovered: %v", r)
			}
		}()
		c.autoWriteSTM(agentName, taskDesc, output, errMsg, success)
	}()
}

func (c *Coordinator) summarizeOutput(ctx context.Context, text string) string {
	s := c.AgentPool().Sidecar()
	if s == nil {
		return text
	}
	c.report(c.newEvent("sidecar_call").withMessage("summarize"))
	if c.think {
		c.emitThinkSidecar("Summarize", "summarizing agent output")
	}
	summarized, err := s.Summarize(ctx, text, 2000)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: sidecar summarize failed: %v\n", err)
		return text
	}
	return summarized
}

func (c *Coordinator) appendHistory(ctx context.Context, steps []fantasy.StepResult) {
	for _, step := range steps {
		for _, msg := range step.Messages {
			// Preserve the original event until verified compaction can extract
			// its diagnostics and validate tool-call/result evidence.
			c.conversationHistory = append(c.conversationHistory, msg)
			c.conversationHistorySourceCounts = append(c.conversationHistorySourceCounts, 1)
		}
	}
	if len(c.conversationHistorySourceCounts) < len(c.conversationHistory) {
		for i := len(c.conversationHistorySourceCounts); i < len(c.conversationHistory); i++ {
			c.conversationHistorySourceCounts = append(c.conversationHistorySourceCounts, 1)
		}
	} else if len(c.conversationHistorySourceCounts) > len(c.conversationHistory) {
		c.conversationHistorySourceCounts = c.conversationHistorySourceCounts[:len(c.conversationHistory)]
	}
	if len(c.conversationHistory) <= maxConversationHistory {
		return
	}
	compactCount := len(c.conversationHistory) - compactHistoryThreshold
	if compactCount <= 0 {
		compactCount = len(c.conversationHistory) / 3
	}
	if compactCount <= 0 {
		trimmed, trimmedCounts, removed := trimHistoryPreservingHead(c.conversationHistory, c.conversationHistorySourceCounts, maxConversationHistory)
		c.conversationHistory = trimmed
		c.conversationHistorySourceCounts = trimmedCounts
		c.conversationHistorySourceOffset += removed
		return
	}
	// Invariant 1: Ensure boundary never splits tool call and tool result.
	compactCount = AdjustBoundaryToPreserveToolPairs(c.conversationHistory, compactCount)
	if compactCount <= 0 || compactCount >= len(c.conversationHistory) {
		trimmed, trimmedCounts, removed := trimHistoryPreservingHead(c.conversationHistory, c.conversationHistorySourceCounts, maxConversationHistory)
		c.conversationHistory = trimmed
		c.conversationHistorySourceCounts = trimmedCounts
		c.conversationHistorySourceOffset += removed
		return
	}

	sourceOffset := c.conversationHistorySourceOffset
	sourceCounts := c.conversationHistorySourceCounts[:compactCount]
	compacted := c.compactMessages(ctx, c.conversationHistory[:compactCount], sourceOffset, sourceCounts)
	compactedSourceCount := sumSourceCounts(sourceCounts)
	c.conversationHistory = append(compacted, c.conversationHistory[compactCount:]...)
	c.conversationHistorySourceCounts = append(
		[]int{compactedSourceCount},
		c.conversationHistorySourceCounts[compactCount:]...,
	)
	if len(c.conversationHistory) > maxConversationHistory {
		// The summary message plus the retained tail still exceeds the limit
		// (the tail alone is near the cap). compactMessages always replaces the
		// compacted prefix with a structured summary — even without a sidecar —
		// so this branch means the retained segment is too large, not that
		// compaction was skipped. Keep the first few messages — which carry the
		// original goal and instructions — plus the most recent ones, instead of
		// dropping the head entirely.
		trimmed, trimmedCounts, removed := trimHistoryPreservingHead(c.conversationHistory, c.conversationHistorySourceCounts, maxConversationHistory)
		c.conversationHistory = trimmed
		c.conversationHistorySourceCounts = trimmedCounts
		c.conversationHistorySourceOffset += removed
	}
}

// conversationHeadKeep is the number of earliest messages preserved when the
// conversation history is hard-trimmed. These usually contain the original goal
// and setup that later turns depend on.
const conversationHeadKeep = 1

// trimHistoryPreservingHead reduces msgs to at most max entries by keeping the
// first conversationHeadKeep messages and the most recent remainder. This avoids
// the "amnesia" failure where the original goal is dropped and the coordinator
// re-delegates already-completed work.
func trimHistoryPreservingHead(msgs []fantasy.Message, sourceCounts []int, max int) ([]fantasy.Message, []int, int) {
	if max <= 0 {
		return nil, nil, len(msgs)
	}
	if len(msgs) <= max {
		return msgs, sourceCounts, 0
	}

	sourceCounts = normalizeSourceCounts(len(msgs), sourceCounts)

	headKeep := conversationHeadKeep
	if headKeep >= max {
		headKeep = max / 4
	}

	for head := trimMinInt(headKeep, len(msgs)); head >= 0; head-- {
		tail := max - head
		if tail < 0 || head+tail > len(msgs) {
			continue
		}
		tailStart := len(msgs) - tail
		if head > tailStart {
			continue
		}
		if !isToolPairBoundaryClean(msgs, head) || !isToolPairBoundaryClean(msgs, tailStart) {
			continue
		}
		trimmed := make([]fantasy.Message, 0, head+tail)
		trimmed = append(trimmed, msgs[:head]...)
		trimmed = append(trimmed, msgs[tailStart:]...)

		trimmedCounts := make([]int, 0, head+tail)
		trimmedCounts = append(trimmedCounts, sourceCounts[:head]...)
		trimmedCounts = append(trimmedCounts, sourceCounts[tailStart:]...)
		return trimmed, trimmedCounts, sumSourceCounts(sourceCounts[head:tailStart])
	}

	start := len(msgs) - max
	start = AdjustBoundaryToPreserveToolPairs(msgs, start)

	minKeep := trimMinInt(max, len(msgs))
	if minKeep < 1 {
		minKeep = 1
	}
	maxStart := len(msgs) - minKeep
	if start > maxStart {
		start = maxStart
	}
	if start < 0 {
		start = 0
	}

	for probe := start; probe >= 0; probe-- {
		if isToolPairBoundaryClean(msgs, probe) {
			start = probe
			break
		}
	}
	if start < 0 {
		start = 0
	}

	trimmed := make([]fantasy.Message, len(msgs)-start)
	copy(trimmed, msgs[start:])
	trimmedCounts := make([]int, len(msgs)-start)
	copy(trimmedCounts, sourceCounts[start:])
	return trimmed, trimmedCounts, sumSourceCounts(sourceCounts[:start])
}

func (c *Coordinator) compactMessages(ctx context.Context, messages []fantasy.Message, sourceOffset int, sourceCounts []int) []fantasy.Message {
	if len(messages) < 1 {
		return messages
	}
	sourceCounts = normalizeSourceCounts(len(messages), sourceCounts)
	sourceCount := sumSourceCounts(sourceCounts)

	s := c.AgentPool().Sidecar()
	workspace := ""
	if c.session != nil {
		workspace = c.session.Workspace
	}

	// Invariant 6: Load previous structured summary if available
	var prevSummary *StructuredSummary
	if c.lastCompactionSummary != nil {
		prevSummary = c.lastCompactionSummary
	} else if workspace != "" {
		prevSummary = GetLatestCompactionSummary(workspace)
	}

	// Model-aware token accounting: use the coordinator's resolved model so the
	// estimator family matches the one used for context budgeting (§5.3).
	compactionModel := c.coordinatorModelID()

	tokensBefore := countTokensInMessages(compactionModel, messages)
	if prevSummary != nil {
		tokensBefore += countTokensInText(compactionModel, prevSummary.RenderMarkdown())
	}

	// Invariant 2: Original user goal
	originalGoal := c.initialPrompt
	if originalGoal == "" && prevSummary != nil {
		originalGoal = prevSummary.Goal
	}
	if originalGoal == "" {
		originalGoal = extractFirstUserMessageText(messages)
	}

	verified, verifiedErr := c.compactVerifiedConversation(ctx, messages)
	if verifiedErr != nil {
		// Required evidence cannot safely fit the context budget. Do not replace
		// history with an unchecked/truncated summary.
		log.Printf("warning: verified history compaction failed: %v; retaining original messages", verifiedErr)
		return messages
	}

	var sidecarAdapter SidecarCompacter
	if s != nil {
		sidecarAdapter = s
	}

	summary, err := PerformStructuredCompaction(ctx, sidecarAdapter, messages, prevSummary, originalGoal)
	if err != nil || summary == nil {
		summary = EnforceCompactionInvariants(&StructuredSummary{}, prevSummary, originalGoal, messages)
	}
	var todoItems []*TodoItem
	if c.taskTracker != nil {
		todoItems = c.taskTracker.TodoList().Items()
		summary = mergeTypedTaskResultFacts(summary, todoItems)
	}

	var activeTaskIDs, failedTaskIDs []string
	if c.taskTracker != nil {
		for _, item := range todoItems {
			switch item.Status {
			case TaskInProgress, TaskVerifying, TaskPlanned, TaskPending:
				activeTaskIDs = append(activeTaskIDs, item.ID)
			case TaskError:
				failedTaskIDs = append(failedTaskIDs, item.ID)
			}
		}
	}
	if valErr := ValidateStructuredSummary(summary, prevSummary, messages, activeTaskIDs, failedTaskIDs); valErr != nil {
		log.Printf("warning: post-compaction validation failed (%v); building deterministic fallback", valErr)
		fallback := EnforceCompactionInvariants(&StructuredSummary{}, prevSummary, originalGoal, messages)
		fallback = mergeTypedTaskResultFacts(fallback, todoItems)
		if postValErr := ValidateStructuredSummary(fallback, prevSummary, messages, activeTaskIDs, failedTaskIDs); postValErr != nil {
			// Never replace history with a summary known to violate invariants.
			log.Printf("warning: deterministic compaction fallback failed validation (%v); retaining original messages", postValErr)
			return messages
		}
		summary = fallback
	}

	c.lastCompactionSummary = summary

	markdownSummary := summary.RenderMarkdown()
	tokensAfter := countTokensInText(compactionModel, markdownSummary)

	// Invariant 7: Persist compaction record with tokens_before, tokens_after, and source range
	if workspace != "" {
		rec := CompactionRecord{
			ID:           fmt.Sprintf("compact_%d", time.Now().UnixNano()),
			Timestamp:    time.Now(),
			TokensBefore: tokensBefore,
			TokensAfter:  tokensAfter,
			SourceRange: CompactionRange{
				StartIndex: sourceOffset,
				EndIndex:   sourceOffset + sourceCount - 1,
				MsgCount:   sourceCount,
			},
			Summary: *summary,
		}
		if err := c.SessionStore().SaveCompactionRecord(workspace, rec); err != nil {
			log.Printf("warning: failed to save compaction record: %v", err)
		}
		c.recordCompaction()
	}

	if c.think {
		c.emitThinkSidecar("Compact", fmt.Sprintf("compacted %d messages into structured summary (%d -> %d tokens)", len(messages), tokensBefore, tokensAfter))
	}

	return []fantasy.Message{
		fantasy.NewUserMessage(verifiedHistoryPrefix + verified.Content + "\n\n[Structured Compaction Summary]\n" + markdownSummary),
	}
}

func normalizeSourceCounts(length int, sourceCounts []int) []int {
	if len(sourceCounts) < length {
		normalized := make([]int, length)
		copy(normalized, sourceCounts)
		for i := len(sourceCounts); i < length; i++ {
			normalized[i] = 1
		}
		for i := range normalized {
			if normalized[i] <= 0 {
				normalized[i] = 1
			}
		}
		return normalized
	}

	normalized := make([]int, len(sourceCounts))
	copy(normalized, sourceCounts)
	if len(normalized) > length {
		normalized = normalized[:length]
	}
	for i := range normalized {
		if normalized[i] <= 0 {
			normalized[i] = 1
		}
	}
	return normalized
}

func sumSourceCounts(sourceCounts []int) int {
	total := 0
	for _, c := range sourceCounts {
		total += c
	}
	return trimMaxInt(1, total)
}

func trimMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func trimMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (c *Coordinator) SetSessionData(sd *SessionData) {
	c.sessionData = sd
	if sd == nil {
		return
	}
	if sd.RunResult != nil {
		c.SetLastRunResult(sd.RunResult)
	}
	if len(sd.AcceptanceContractRevisions) > 0 {
		latest := sd.AcceptanceContractRevisions[len(sd.AcceptanceContractRevisions)-1]
		c.acceptanceContractRevision = latest.Revision
		c.acceptanceContractFixed = true
		spec := cloneAcceptanceSpec(latest.NewSpec)
		c.acceptanceSpec = &spec
		if len(spec.Commands) > 0 {
			c.acceptanceCmd = spec.Commands[0]
		}
	}

	prof := c.ExecutionProfile()

	// A resumed session carries rounds from earlier runs; without this the
	// saved count restarts at this run's round and understates the session.
	// When DisableHistoricalTaskReuse is enabled (e.g. fresh-verification),
	// prior rounds are not inherited so execution starts fresh at round 0.
	if !prof.DisableHistoricalTaskReuse {
		c.baseRounds = sd.Rounds
	} else {
		c.baseRounds = 0
	}
	if len(sd.Tasks) > 0 && !prof.DisableHistoricalTaskReuse && !prof.DisableJournalRestore {
		c.taskTracker.TodoList().Restore(sd.Tasks)
	}
	c.rebuildAntiThrashingState()

	if !prof.DisableHistoricalTaskReuse && !prof.DisableJournalRestore {
		c.taskResultCacheMu.Lock()
		gen := c.cacheGeneration.Load()
		for _, t := range sd.Tasks {
			if t.Status == TaskDone && t.Output != "" {
				agentKey := strings.ToLower(t.Agent)
				c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], cachedTaskEntry{
					taskDesc:     t.Desc,
					verify:       t.Verify,
					verifyMode:   normalizeVerifyMode(t.VerifyMode),
					verifySpec:   cloneVerificationSpecPtr(t.VerifySpec),
					verification: cloneVerificationResult(t.VerifyResult),
					output:       t.Output,
					generation:   gen,
					pinned:       true,
				})
				if len(c.taskResultCache[agentKey]) > maxTaskCacheEntries {
					c.taskResultCache[agentKey] = c.taskResultCache[agentKey][1:]
				}
			}
		}
		c.taskResultCacheMu.Unlock()
	}

	// TodoList is the canonical lifecycle state. Every mutation callback must
	// persist the checkpoint and rebuild the derived status projection so
	// transitions made by any coordinator path (DAG, plan, delegate, recovery,
	// or direct execution) cannot leave status files stale.
	c.taskTracker.TodoList().onChange = func() {
		c.saveCheckpoint()
		c.reconcileTaskStatusProjection()
	}
	if !prof.DisableHistoricalMemory {
		c.hydrateConversationHistoryFromSessionData()
	}
}

// ContinuationCheckpoint returns a copy of the persisted continuation state.
func (c *Coordinator) ContinuationCheckpoint() *ContinuationCheckpoint {
	if c == nil || c.sessionData == nil || c.sessionData.ContinuationCheckpoint == nil {
		return nil
	}
	cp := *c.sessionData.ContinuationCheckpoint
	return &cp
}

func (c *Coordinator) saveContinuationCheckpoint(turn, maxTurns int, reason, status string) {
	if c == nil || c.sessionData == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	c.sessionData.ContinuationCheckpoint = &ContinuationCheckpoint{TurnCount: turn, MaxTurns: maxTurns, Reason: reason, Status: status}
	_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	c.emitEvent("coordinator_continuation_checkpoint", "coordinator", "", map[string]interface{}{"turn_count": turn, "max_turns": maxTurns, "reason": reason, "status": status})
}

// ResumeContinuationCheckpoint persists the transition from an interrupted
// continuation to a new run, allowing callers to query the restart boundary.
func (c *Coordinator) ResumeContinuationCheckpoint() *ContinuationCheckpoint {
	cp := c.ContinuationCheckpoint()
	if cp == nil || (cp.Status != "pending" && cp.Status != "aborted") {
		return cp
	}
	resume := *cp
	c.continuationResume = &resume
	c.saveContinuationCheckpoint(cp.TurnCount, cp.MaxTurns, cp.Reason, "resumed")
	return c.ContinuationCheckpoint()
}

func (c *Coordinator) hydrateConversationHistoryFromSessionData() {
	if c.sessionData == nil || len(c.conversationHistory) == 0 {
		return
	}

	if len(c.sessionData.ConversationHistorySourceCounts) > 0 {
		c.conversationHistorySourceCounts = normalizeSourceCounts(len(c.conversationHistory), c.sessionData.ConversationHistorySourceCounts)
	} else if len(c.conversationHistorySourceCounts) != len(c.conversationHistory) {
		c.conversationHistorySourceCounts = make([]int, len(c.conversationHistory))
		for i := range c.conversationHistorySourceCounts {
			c.conversationHistorySourceCounts[i] = 1
		}
	}

	if c.sessionData.ConversationHistorySourceOffset > 0 || len(c.conversationHistorySourceCounts) > 0 {
		c.conversationHistorySourceOffset = c.sessionData.ConversationHistorySourceOffset
	}
	if c.conversationHistorySourceOffset < 0 {
		c.conversationHistorySourceOffset = 0
	}
}

func (c *Coordinator) syncConversationHistoryStateToSessionData() {
	if c.sessionData == nil {
		return
	}
	if c.conversationHistorySourceOffset < 0 {
		c.conversationHistorySourceOffset = 0
	}
	c.sessionData.ConversationHistorySourceOffset = c.conversationHistorySourceOffset

	if len(c.conversationHistorySourceCounts) == 0 {
		c.sessionData.ConversationHistorySourceCounts = nil
		return
	}
	c.sessionData.ConversationHistorySourceCounts = append([]int(nil), c.conversationHistorySourceCounts...)
}

func (c *Coordinator) saveCheckpoint() {
	if c.sessionData == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	c.sessionData.Tasks = c.taskTracker.TodoList().Items()
	_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	c.emitTaskEventsFromCheckpoint(c.sessionData.Tasks)
	c.updateBranchState()
}

// updateBranchState snapshots the coordinator's live state (task plan, active
// model, selected team, latest compaction summary) into the active session
// branch, so `hufu session` checkout/time-travel can restore it later (§8).
// Best-effort: any failure leaves the checkpoint path unaffected.
func (c *Coordinator) updateBranchState() {
	st, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return
	}
	b := st.Branches[st.ActiveBranch]
	if b == nil {
		return
	}
	if len(c.sessionData.Tasks) > 0 {
		plan := make([]*TodoItem, len(c.sessionData.Tasks))
		for i, t := range c.sessionData.Tasks {
			plan[i] = cloneTodoItem(t)
		}
		b.State.TaskPlan = plan
	}
	b.State.ActiveModel = c.session.Config.Generation.Model
	b.State.SelectedTeam = c.session.Config.Name
	if c.lastCompactionSummary != nil {
		b.State.Compaction = cloneStructuredSummary(c.lastCompactionSummary)
	}
	_ = SaveSessionTree(c.session.Workspace, st)
}

// isInterruptedStatus reports whether a restored task status indicates the task
// was left incomplete by an interrupted (crashed/killed) run and must be
// re-driven on resume. Terminal states (done/skipped) and definitively-failed
// tasks (error, which already exhausted their retries) are left untouched.
func isInterruptedStatus(s TaskStatus) bool {
	switch s {
	case TaskInProgress, TaskVerifying, TaskPaused, TaskPlanned, TaskPending, TaskProtocolIncomplete:
		return true
	default:
		return false
	}
}

// todoIDLess orders numeric todo IDs ("1","2",...) numerically, falling back to
// string comparison for non-numeric IDs.
func todoIDLess(a, b string) bool {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a < b
}

// getInterruptedTasks finds tasks left in-flight by a previous run and returns
// them in dependency-safe (ascending ID) order without mutating status.
func (c *Coordinator) getInterruptedTasks() []*TodoItem {
	items := c.taskTracker.TodoList().Items()
	var interrupted []*TodoItem
	for _, it := range items {
		if isInterruptedStatus(it.Status) {
			interrupted = append(interrupted, it)
		}
	}
	sort.SliceStable(interrupted, func(i, j int) bool {
		return todoIDLess(interrupted[i].ID, interrupted[j].ID)
	})
	return interrupted
}

// ResumeInterruptedTasks re-drives the worker tasks that a previous run left
// in-flight (restored from the session checkpoint). Interrupted tasks are
// evaluated against side-effect class and recovery policy (§11.2-11.4).
// Tasks with policy 'retry' or reconciled as 'not_started' are re-executed;
// 'manual' tasks are blocked and flagged for human review; 'never' tasks are
// skipped (left as-is) since the policy declares they must not be re-driven.
func (c *Coordinator) ResumeInterruptedTasks(ctx context.Context) (int, error) {
	prof := c.ExecutionProfile()
	if prof.DisableHistoricalTaskReuse || prof.DisableJournalRestore {
		return 0, nil
	}
	interrupted := c.getInterruptedTasks()
	if len(interrupted) == 0 {
		return 0, nil
	}
	isUnattended := (c.session != nil && c.session.Config.Unattended) || c.unattended
	c.report(c.newEvent("step").withMessage(fmt.Sprintf("resuming %d interrupted task(s) from checkpoint", len(interrupted))))
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	var firstErr error
	count := 0
	for _, it := range interrupted {
		if ctx.Err() != nil {
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			break
		}

		pol := ResolveRecoveryPolicy(it.Recovery, it.SideEffect, isUnattended, c.ExecutionProfile())
		task := taskDefFromTodoItem(it)
		switch pol {
		case RecoveryRetry:
			if !IsTaskReplayable(task) {
				detail := fmt.Sprintf("task blocked by replay policy; side_effect=%s or allows_replay=false", it.SideEffect)
				c.taskTracker.TodoList().UpdateStatus(it.ID, TaskBlocked, detail)
				c.reconcileTaskStatusProjection()
				c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{"policy": string(pol), "decision": "replay_blocked", "reason": detail})
				c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
				continue
			}
			c.taskTracker.TodoList().ResetForRetry(it.ID, "resumed after interruption")
			c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
				"side_effect": string(it.SideEffect),
				"policy":      string(pol),
				"decision":    "retry",
			})
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			if _, err := c.executeTask(ctx, task, it.ID); err != nil && firstErr == nil {
				firstErr = err
			}
			count++

		case RecoveryManual:
			detail := fmt.Sprintf("task halted by side-effect recovery policy (%s, side_effect=%s); requires manual intervention", pol, it.SideEffect)
			c.taskTracker.TodoList().UpdateStatus(it.ID, TaskBlocked, detail)
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
				"side_effect": string(it.SideEffect),
				"policy":      string(pol),
				"decision":    "manual_blocked",
			})
			c.rememberFailureContext("coordinator", "recovery policy blocked execution", it.ID, detail)

		case RecoveryNever:
			// 'never' means the task must not be automatically re-driven on
			// resume — leave it untouched and mark it skipped. Unlike 'manual'
			// this does not emit needs_human: the policy is a deliberate
			// "do not touch" declaration, not a request for human action.
			detail := fmt.Sprintf("task skipped by side-effect recovery policy (%s, side_effect=%s); left as-is, not re-driven", pol, it.SideEffect)
			c.taskTracker.TodoList().UpdateStatus(it.ID, TaskSkipped, detail)
			c.reconcileTaskStatusProjection()
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
			c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
				"side_effect": string(it.SideEffect),
				"policy":      string(pol),
				"decision":    "never_skipped",
			})

		case RecoveryReconcile:
			state := c.reconcileInterruptedTask(ctx, it)
			it.RecoveryState = state
			c.taskTracker.TodoList().SetRecoveryState(it.ID, state)
			switch state {
			case RecoveryStateComplete:
				c.taskTracker.TodoList().UpdateStatus(it.ID, TaskDone, "reconciliation confirmed task was completed")
				c.reconcileTaskStatusProjection()
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
					"side_effect":    string(it.SideEffect),
					"policy":         string(pol),
					"recovery_state": state,
					"decision":       "mark_done",
				})
			case RecoveryStateNotStarted:
				if !IsTaskReplayable(task) {
					detail := fmt.Sprintf("task blocked by replay policy after reconciliation; side_effect=%s or allows_replay=false", it.SideEffect)
					c.taskTracker.TodoList().UpdateStatus(it.ID, TaskBlocked, detail)
					c.reconcileTaskStatusProjection()
					c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{"policy": string(pol), "recovery_state": state, "decision": "replay_blocked", "reason": detail})
					c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
					continue
				}
				c.taskTracker.TodoList().ResetForRetry(it.ID, "reconciliation allowed retry")
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
					"side_effect":    string(it.SideEffect),
					"policy":         string(pol),
					"recovery_state": state,
					"decision":       "retry",
				})
				if _, err := c.executeTask(ctx, task, it.ID); err != nil && firstErr == nil {
					firstErr = err
				}
				count++
			case RecoveryStatePartial, RecoveryStateUnknown:
				prof := c.ExecutionProfile()
				status := TaskBlocked
				decision := state + "_blocked"
				if state == RecoveryStateUnknown && prof.FailOnUnknownState {
					status = TaskError
					decision = "failed_on_unknown_state"
				}
				detail := fmt.Sprintf("task halted by side-effect recovery policy (%s, side_effect=%s); reconciliation state: %s", pol, it.SideEffect, state)
				c.taskTracker.TodoList().UpdateStatus(it.ID, status, detail)
				c.reconcileTaskStatusProjection()
				if status == TaskBlocked {
					c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
				}
				c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
				c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
					"side_effect":    string(it.SideEffect),
					"policy":         string(pol),
					"recovery_state": state,
					"decision":       decision,
				})
				if status == TaskError {
					err := fmt.Errorf("task %s failed due to unknown state (FailOnUnknownState profile setting)", it.ID)
					if firstErr == nil {
						firstErr = err
					}
					c.rememberFailureContext("coordinator", "reconciliation failed execution on unknown state", it.ID, detail)
				} else {
					c.rememberFailureContext("coordinator", "reconciliation blocked execution", it.ID, detail)
				}
			}
		}
		if pol != RecoveryRetry && pol != RecoveryReconcile && pol != RecoveryManual && pol != RecoveryNever {
			detail := fmt.Sprintf("task blocked by unknown recovery policy %q", pol)
			c.taskTracker.TodoList().UpdateStatus(it.ID, TaskBlocked, detail)
			c.reconcileTaskStatusProjection()
			c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{"policy": string(pol), "decision": "unknown_policy_blocked"})
		}
	}
	return count, firstErr
}

func taskDefFromTodoItem(it *TodoItem) TaskDef {
	if it == nil {
		return TaskDef{}
	}
	return TaskDef{
		Agent: it.Agent, Goal: it.Desc, Verify: it.Verify, VerifyMode: it.VerifyMode,
		VerifySpec: cloneVerificationSpecPtr(it.VerifySpec), SideEffect: it.SideEffect,
		Recovery: it.Recovery, ReconcileTool: it.ReconcileTool, Execution: it.Execution,
		Kind: it.Kind, Advances: append([]string(nil), it.Advances...),
		ExpectedStateChange: it.ExpectedStateChange, RecoveryHypothesis: cloneRecoveryHypothesis(it.RecoveryHypothesis),
	}
}

func (c *Coordinator) SessionData() *SessionData {
	return c.sessionData
}

func (c *Coordinator) saveHistoryAndSession(ctx context.Context, steps []fantasy.StepResult) {
	c.conversationHistoryMu.Lock()
	c.appendHistory(ctx, steps)
	if c.session != nil && c.session.Workspace != "" {
		_ = SaveConversationHistory(c.session.Workspace, c.conversationHistory)
	}
	c.syncConversationHistoryStateToSessionData()
	c.conversationHistoryMu.Unlock()
	if c.sessionData != nil && c.session != nil && c.session.Workspace != "" {
		_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	}
}
