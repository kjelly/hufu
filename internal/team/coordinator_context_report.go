package team

// Context budget reporting for the execution report (§5.4).
//
// buildSystemPrompt captures the token cost of each prompt subsystem (core
// orchestrator instructions, project context, STM/LTM/RAG) as it assembles the
// coordinator system prompt. recordContextBreakdown folds those captures
// together with tool-schema, conversation, compacted-history, task-dependency
// and reply-reserve measurements into a ContextUsageBreakdown, and ContextUsageReport
// exposes the last recorded breakdown so cmd/hufu/report.go can render it.

import (
	"context"
	"strings"
)

// recordContextBreakdown computes and stores the model-aware token breakdown for
// the most recently assembled coordinator prompt. It is cheap (estimator-based,
// no network) and safe to call on every buildSystemPrompt.
func (c *Coordinator) recordContextBreakdown(ctx context.Context, modelID, coreText, projectText, memoryText string) {
	if modelID == "" {
		modelID = c.coordinatorModelID()
	}
	spec := globalRegistry.GetSpec(modelID).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(nil))
	counter := defaultCounter

	systemTokens, _ := counter.CountText(ctx, modelID, coreText)
	projectTokens, _ := counter.CountText(ctx, modelID, projectText)
	memoryTokens, _ := counter.CountText(ctx, modelID, memoryText)

	toolsTokens, _ := counter.CountTools(ctx, modelID, c.coreTools)

	c.conversationHistoryMu.Lock()
	history := c.conversationHistory
	c.conversationHistoryMu.Unlock()
	recentTokens, _ := counter.CountMessages(ctx, modelID, history)

	compactedTokens := 0
	// lastCompactionSummary is written during the run (compactMessages) and read
	// here while assembling the prompt; the report-time read occurs after the run
	// completes. A direct read matches the existing access pattern in
	// coordinator_session.go.
	if c.lastCompactionSummary != nil {
		compactedTokens, _ = counter.CountText(ctx, modelID, c.lastCompactionSummary.RenderMarkdown())
	}

	// Task dependency results: token cost of completed task outputs (truncated
	// per task so one verbose task does not dominate the estimate).
	taskDepTokens := 0
	if c.taskTracker != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item == nil || item.Status != TaskDone {
				continue
			}
			out := item.Output
			if r := []rune(out); len(r) > taskOutputBreakdownCap {
				out = string(r[:taskOutputBreakdownCap])
			}
			if out == "" {
				continue
			}
			n, _ := counter.CountText(ctx, modelID, out)
			taskDepTokens += n
		}
	}

	replyReserve := spec.MaxOutputTokens
	if replyReserve <= 0 {
		replyReserve = 4096
	}

	usage := ContextUsageBreakdown{
		SystemInstructions:    systemTokens,
		ToolSchemas:           toolsTokens,
		RecentConversation:    recentTokens,
		CompactedHistory:      compactedTokens,
		ProjectContext:        projectTokens,
		StmLtmRag:             memoryTokens,
		TaskDependencyResults: taskDepTokens,
		ReplyReserve:          replyReserve,
	}

	window := spec.ContextWindow
	if window <= 0 {
		window = 128000
	}
	budget := ContextBudget{Window: window}

	c.ctxReportMu.Lock()
	c.lastCtxBreakdown = usage
	c.lastCtxBudget = budget
	c.lastCtxModel = modelID
	c.lastCtxReportReady = true
	c.ctxReportMu.Unlock()
}

// taskOutputBreakdownCap limits how much of a single task's output is counted
// toward the "Task dependency results" breakdown column.
const taskOutputBreakdownCap = 1000

// ContextUsageReport returns the most recently recorded context budget and
// usage breakdown for the coordinator, plus the model ID the estimate was made
// for. ready is false when no prompt has been assembled yet (e.g. a dry run or
// before the first coordinator step), in which case the caller should omit the
// section.
func (c *Coordinator) ContextUsageReport() (budget ContextBudget, usage ContextUsageBreakdown, modelID string, ready bool) {
	c.ctxReportMu.RLock()
	defer c.ctxReportMu.RUnlock()
	return c.lastCtxBudget, c.lastCtxBreakdown, c.lastCtxModel, c.lastCtxReportReady
}

// EstimatedContextModel reports whether the model used for the last context
// breakdown relies on an estimator fallback rather than a known registry spec.
// Exposed so reports can annotate estimated counts (§5.3).
func (c *Coordinator) EstimatedContextModel() bool {
	c.ctxReportMu.RLock()
	defer c.ctxReportMu.RUnlock()
	if !c.lastCtxReportReady || c.lastCtxModel == "" {
		return false
	}
	return globalRegistry.GetSpec(c.lastCtxModel).IsEstimated
}

// RenderContextUsageSection returns the markdown for the "Context Usage"
// report section, or "" when no breakdown is ready (e.g. before the first
// coordinator step or in a dry run).
func (c *Coordinator) RenderContextUsageSection() string {
	budget, usage, modelID, ready := c.ContextCompiler().ContextUsageReport()
	if !ready {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Context Usage\n\n")
	b.WriteString(budget.BreakdownReport(usage))
	b.WriteString("\n")
	if c.EstimatedContextModel() {
		spec := globalRegistry.GetSpec(modelID)
		b.WriteString("> _Token counts are **estimated** using the `")
		b.WriteString(spec.Estimator)
		b.WriteString("` estimator fallback for model `")
		b.WriteString(modelID)
		b.WriteString("` (no exact tokenizer available). Counts are conservative._\n\n")
	} else {
		b.WriteString("> _Token counts are based on the `")
		b.WriteString(globalRegistry.GetSpec(modelID).Estimator)
		b.WriteString("` estimator for model `")
		b.WriteString(modelID)
		b.WriteString("`._\n\n")
	}
	b.WriteString("---\n\n")
	return b.String()
}
