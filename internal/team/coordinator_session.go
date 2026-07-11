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
	fname := fmt.Sprintf("stm_r%d.md", c.round)
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
		}
	}
	if len(c.conversationHistory) <= maxConversationHistory {
		return
	}
	compactCount := len(c.conversationHistory) - compactHistoryThreshold
	if compactCount <= 0 {
		compactCount = len(c.conversationHistory) / 3
	}
	if compactCount <= 0 {
		c.conversationHistory = trimHistoryPreservingHead(c.conversationHistory, maxConversationHistory)
		return
	}
	compacted := c.compactMessages(ctx, c.conversationHistory[:compactCount])
	c.conversationHistory = append(compacted, c.conversationHistory[compactCount:]...)
	if len(c.conversationHistory) > maxConversationHistory {
		// Compaction did not shrink enough (e.g. sidecar unavailable so
		// compactMessages returned the input unchanged). Keep the first few
		// messages — which carry the original goal and instructions — plus the
		// most recent ones, instead of dropping the head entirely.
		c.conversationHistory = trimHistoryPreservingHead(c.conversationHistory, maxConversationHistory)
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
func trimHistoryPreservingHead(msgs []fantasy.Message, max int) []fantasy.Message {
	if max <= 0 {
		return nil
	}
	if len(msgs) <= max {
		return msgs
	}
	headKeep := conversationHeadKeep
	if headKeep >= max {
		headKeep = max / 4
	}
	tailKeep := max - headKeep
	trimmed := make([]fantasy.Message, 0, max)
	trimmed = append(trimmed, msgs[:headKeep]...)
	trimmed = append(trimmed, msgs[len(msgs)-tailKeep:]...)
	return trimmed
}

func (c *Coordinator) compactMessages(ctx context.Context, messages []fantasy.Message) []fantasy.Message {
	s := c.Sidecar()
	if s == nil || len(messages) < 2 {
		return messages
	}
	var b strings.Builder
	for _, msg := range messages {
		for _, part := range msg.Content {
			if txt, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				b.WriteString(txt.Text)
				b.WriteString("\n")
			}
		}
	}
	if b.Len() == 0 {
		return messages
	}
	if c.think {
		c.emitThinkSidecar("Compact", fmt.Sprintf("compacting %d messages", len(messages)))
	}
	result, err := s.Compact(ctx, b.String(), "Compress the following conversation into a concise summary while preserving key facts, decisions, and results.")
	if err != nil || result == "" {
		return messages
	}
	return []fantasy.Message{
		fantasy.NewUserMessage("[Compacted history]\n" + result),
	}
}

func (c *Coordinator) SetSessionData(sd *SessionData) {
	c.sessionData = sd
	if sd != nil {
		if len(sd.Tasks) > 0 {
			c.taskTracker.TodoList().Restore(sd.Tasks)

			c.taskResultCacheMu.Lock()
			gen := c.cacheGeneration.Load()
			for _, t := range sd.Tasks {
				if t.Status == TaskDone && t.Output != "" {
					agentKey := strings.ToLower(t.Agent)
					c.taskResultCache[agentKey] = append(c.taskResultCache[agentKey], cachedTaskEntry{
						taskDesc:   t.Desc,
						verify:     t.Verify,
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
		}
		c.taskTracker.TodoList().onChange = c.saveCheckpoint
	}
}

func (c *Coordinator) saveCheckpoint() {
	if c.sessionData == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	c.sessionData.Tasks = c.taskTracker.TodoList().Items()
	_ = SaveSession(c.session.Workspace, c.sessionData)
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
	c.conversationHistoryMu.Unlock()
	if c.sessionData != nil {
		_ = SaveSession(c.session.Workspace, c.sessionData)
	}
}
