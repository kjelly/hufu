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
const maxMessageSize = 50000

func (c *Coordinator) checkpointSTM() {
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
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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
	s := c.Sidecar()
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
			c.conversationHistory = append(c.conversationHistory, truncateOversizedMessage(msg, maxMessageSize))
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
const conversationHeadKeep = 4

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

	s := c.Sidecar()
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

	var sidecarAdapter SidecarCompacter
	if s != nil {
		sidecarAdapter = s
	}

	summary, err := PerformStructuredCompaction(ctx, sidecarAdapter, messages, prevSummary, originalGoal)
	if err != nil || summary == nil {
		summary = EnforceCompactionInvariants(&StructuredSummary{}, prevSummary, originalGoal, messages)
	}

	var activeTaskIDs, failedTaskIDs []string
	if c.taskTracker != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			switch item.Status {
			case TaskInProgress, TaskVerifying, TaskPlanned, TaskPending:
				activeTaskIDs = append(activeTaskIDs, item.ID, item.Desc)
			case TaskError:
				failedTaskIDs = append(failedTaskIDs, item.ID, item.Desc)
			}
		}
	}
	if valErr := ValidateStructuredSummary(summary, prevSummary, messages, activeTaskIDs, failedTaskIDs); valErr != nil {
		log.Printf("warning: post-compaction validation failed (%v); retaining previous summary", valErr)
		if prevSummary != nil {
			summary = prevSummary
		}
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
		if err := SaveCompactionRecord(workspace, rec); err != nil {
			log.Printf("warning: failed to save compaction record: %v", err)
		}
	}

	if c.think {
		c.emitThinkSidecar("Compact", fmt.Sprintf("compacted %d messages into structured summary (%d -> %d tokens)", len(messages), tokensBefore, tokensAfter))
	}

	return []fantasy.Message{
		fantasy.NewUserMessage("[Structured Compacted History]\n" + markdownSummary),
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

	// A resumed session carries rounds from earlier runs; without this the
	// saved count restarts at this run's round and understates the session.
	c.baseRounds = sd.Rounds
	if len(sd.Tasks) > 0 {
		c.taskTracker.TodoList().Restore(sd.Tasks)
	}

	c.taskResultCacheMu.Lock()
	gen := c.cacheGeneration.Load()
	for _, t := range sd.Tasks {
		if t.Status == TaskDone && t.Output != "" {
			agentKey := strings.ToLower(t.Agent)
			c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], cachedTaskEntry{
				taskDesc:   t.Desc,
				verify:     t.Verify,
				verifyMode: normalizeVerifyMode(t.VerifyMode),
				output:     t.Output,
				generation: gen,
				pinned:     true,
			})
			if len(c.taskResultCache[agentKey]) > maxTaskCacheEntries {
				c.taskResultCache[agentKey] = c.taskResultCache[agentKey][1:]
			}
		}
	}
	c.taskResultCacheMu.Unlock()

	c.taskTracker.TodoList().onChange = c.saveCheckpoint
	c.hydrateConversationHistoryFromSessionData()
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
	_ = SaveSession(c.session.Workspace, c.sessionData)
	c.emitTaskEventsFromCheckpoint(c.sessionData.Tasks)
}

// isInterruptedStatus reports whether a restored task status indicates the task
// was left incomplete by an interrupted (crashed/killed) run and must be
// re-driven on resume. Terminal states (done/skipped) and definitively-failed
// tasks (error, which already exhausted their retries) are left untouched.
func isInterruptedStatus(s TaskStatus) bool {
	switch s {
	case TaskInProgress, TaskVerifying, TaskPaused, TaskPlanned, TaskPending:
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

// resetInterruptedTasks finds tasks left in-flight by a previous run, resets
// each to pending so it can be re-driven on its original todo ID, and returns
// them in dependency-safe (ascending ID) order. Split from execution so the
// selection/reset logic can be unit-tested without an LLM provider.
func (c *Coordinator) resetInterruptedTasks() []*TodoItem {
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
	for _, it := range interrupted {
		c.taskTracker.TodoList().ResetForRetry(it.ID, "resumed after interruption")
	}
	return interrupted
}

// ResumeInterruptedTasks re-drives the worker tasks that a previous run left
// in-flight (restored from the session checkpoint). Completed work is reused via
// the result cache prepopulated in SetSessionData; only interrupted tasks are
// re-executed, on their original todo IDs and in ascending-ID order so that
// dependencies (which carry lower IDs) run first. It is a no-op on a fresh run
// because the todo list is empty. Returns the number of tasks re-driven and the
// first error encountered, if any.
func (c *Coordinator) ResumeInterruptedTasks(ctx context.Context) (int, error) {
	interrupted := c.resetInterruptedTasks()
	if len(interrupted) == 0 {
		return 0, nil
	}
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
		task := TaskDef{Agent: it.Agent, Goal: it.Desc}
		if _, err := c.executeTask(ctx, task, it.ID); err != nil && firstErr == nil {
			firstErr = err
		}
		count++
	}
	return count, firstErr
}

func (c *Coordinator) SessionData() *SessionData {
	return c.sessionData
}

func (c *Coordinator) saveHistoryAndSession(ctx context.Context, steps []fantasy.StepResult) {
	c.conversationHistoryMu.Lock()
	c.appendHistory(ctx, steps)
	_ = SaveConversationHistory(c.session.Workspace, c.conversationHistory)
	c.syncConversationHistoryStateToSessionData()
	c.conversationHistoryMu.Unlock()
	if c.sessionData != nil {
		_ = SaveSession(c.session.Workspace, c.sessionData)
	}
}
