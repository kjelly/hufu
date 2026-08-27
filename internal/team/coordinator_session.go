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

	"github.com/kjelly/hufu/internal/sidecar"
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
	// Task completion is not complete until its shared-memory receipt is
	// durable. Running this write in a detached goroutine let workspace cleanup
	// race an AtomicWriteFile temporary file, and also made a completed task's
	// handoff nondeterministically omit its STM entry.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[PANIC] autoWriteSTMASync recovered: %v", r)
		}
	}()
	c.autoWriteSTM(agentName, taskDesc, output, errMsg, success)
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
	history := cloneMessages(c.conversationHistory)
	sourceRanges := historySourceRanges(len(history), c.conversationHistorySourceOffset, c.conversationHistorySourceCounts, c.conversationHistorySourceRanges)
	nextSourceIndex := c.conversationHistoryNextSourceIndex
	if nextSourceIndex < maxSourceIndex(sourceRanges) {
		nextSourceIndex = maxSourceIndex(sourceRanges)
	}
	for _, step := range steps {
		for _, msg := range step.Messages {
			// Preserve the original event until verified compaction can extract
			// its diagnostics and validate tool-call/result evidence.
			history = append(history, msg)
			sourceRanges = append(sourceRanges, []CompactionRange{{StartIndex: nextSourceIndex, EndIndex: nextSourceIndex, MsgCount: 1}})
			nextSourceIndex++
		}
	}
	sourceCounts := sourceCountsForRanges(sourceRanges)
	policy := c.compactionPolicy()
	maxHistory := policy.MaxHistoryMessages
	retainHistory := policy.RetainHistoryMessages
	if len(history) <= maxHistory {
		if err := c.persistConversationCheckpointWithProvenance(history, c.conversationHistorySourceOffset, sourceCounts, sourceRanges, nextSourceIndex); err != nil {
			log.Printf("warning: durable conversation checkpoint failed: %v; retaining prior history", err)
			return
		}
		c.conversationHistory = history
		c.conversationHistorySourceCounts = sourceCounts
		c.conversationHistorySourceRanges = sourceRanges
		c.conversationHistoryNextSourceIndex = nextSourceIndex
		return
	}
	compactCount := len(history) - retainHistory
	if compactCount <= 0 {
		compactCount = len(history) / 3
	}
	if compactCount <= 0 {
		trimmed, trimmedRanges, removed := trimHistoryPreservingHeadWithProvenance(history, sourceRanges, maxHistory)
		trimmedCounts := sourceCountsForRanges(trimmedRanges)
		if err := c.persistConversationCheckpointWithProvenance(trimmed, c.conversationHistorySourceOffset+removed, trimmedCounts, trimmedRanges, nextSourceIndex); err != nil {
			log.Printf("warning: durable conversation checkpoint failed: %v; retaining prior history", err)
			return
		}
		c.conversationHistory = trimmed
		c.conversationHistorySourceCounts = trimmedCounts
		c.conversationHistorySourceRanges = trimmedRanges
		c.conversationHistorySourceOffset += removed
		c.conversationHistoryNextSourceIndex = nextSourceIndex
		return
	}
	// Invariant 1: Ensure boundary never splits tool call and tool result.
	compactCount = AdjustBoundaryToPreserveToolPairs(history, compactCount)
	if compactCount <= 0 || compactCount >= len(history) {
		trimmed, trimmedRanges, removed := trimHistoryPreservingHeadWithProvenance(history, sourceRanges, maxHistory)
		trimmedCounts := sourceCountsForRanges(trimmedRanges)
		if err := c.persistConversationCheckpointWithProvenance(trimmed, c.conversationHistorySourceOffset+removed, trimmedCounts, trimmedRanges, nextSourceIndex); err != nil {
			log.Printf("warning: durable conversation checkpoint failed: %v; retaining prior history", err)
			return
		}
		c.conversationHistory = trimmed
		c.conversationHistorySourceCounts = trimmedCounts
		c.conversationHistorySourceRanges = trimmedRanges
		c.conversationHistorySourceOffset += removed
		c.conversationHistoryNextSourceIndex = nextSourceIndex
		return
	}

	compactedRanges := sourceRanges[:compactCount]
	sourceOffset := sourceOffsetForRanges(compactedRanges, c.conversationHistorySourceOffset)
	compactedCounts := sourceCounts[:compactCount]
	projection := c.buildCompactionProjection(ctx, history[:compactCount], sourceOffset, compactedCounts, cloneStructuredSummary(c.lastCompactionSummary), c.AgentPool().Sidecar())
	if projection.summary == nil {
		if err := c.persistConversationCheckpointWithProvenance(history, sourceOffset, sourceCounts, sourceRanges, nextSourceIndex); err != nil {
			log.Printf("warning: durable conversation checkpoint failed: %v; retaining prior history", err)
			return
		}
		c.conversationHistory = history
		c.conversationHistorySourceCounts = sourceCounts
		c.conversationHistorySourceRanges = sourceRanges
		c.conversationHistoryNextSourceIndex = nextSourceIndex
		return
	}
	resultingHistory := append(cloneMessages(projection.messages), history[compactCount:]...)
	resultingRanges := append([][]CompactionRange{flattenSourceRanges(compactedRanges)}, cloneSourceRanges(sourceRanges[compactCount:])...)
	resultingCounts := sourceCountsForRanges(resultingRanges)
	if workspace := c.sessionWorkspace(); workspace != "" {
		record, err := c.commitCompactionCheckpointWithProvenance(ctx, resultingHistory, sourceOffset, resultingCounts, flattenSourceRanges(compactedRanges), resultingRanges, nextSourceIndex, projection)
		if err != nil && record.ID == "" {
			log.Printf("warning: durable compaction checkpoint failed before commit: %v; retaining original history", err)
			return
		}
		if err != nil {
			log.Printf("warning: durable compaction projection gap after canonical commit: %v", err)
		}
	}
	c.conversationHistory = resultingHistory
	c.conversationHistorySourceCounts = resultingCounts
	c.conversationHistorySourceRanges = resultingRanges
	c.conversationHistoryNextSourceIndex = nextSourceIndex
	c.lastCompactionSummary = cloneStructuredSummary(projection.summary)
	if workspace := c.sessionWorkspace(); workspace != "" {
		c.recordCompaction()
	}
	if len(c.conversationHistory) > maxHistory {
		// The summary message plus the retained tail still exceeds the limit
		// (the tail alone is near the cap). compactMessages always replaces the
		// compacted prefix with a structured summary — even without a sidecar —
		// so this branch means the retained segment is too large, not that
		// compaction was skipped. Keep the first few messages — which carry the
		// original goal and instructions — plus the most recent ones, instead of
		// dropping the head entirely.
		trimmed, trimmedRanges, removed := trimHistoryPreservingHeadWithProvenance(c.conversationHistory, c.conversationHistorySourceRanges, maxHistory)
		trimmedCounts := sourceCountsForRanges(trimmedRanges)
		c.conversationHistory = trimmed
		c.conversationHistorySourceCounts = trimmedCounts
		c.conversationHistorySourceRanges = trimmedRanges
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
	sourceRanges := historySourceRanges(len(msgs), 0, sourceCounts, nil)
	trimmed, trimmedRanges, removed := trimHistoryPreservingHeadWithProvenance(msgs, sourceRanges, max)
	return trimmed, sourceCountsForRanges(trimmedRanges), removed
}

func trimHistoryPreservingHeadWithProvenance(msgs []fantasy.Message, sourceRanges [][]CompactionRange, max int) ([]fantasy.Message, [][]CompactionRange, int) {
	if max <= 0 {
		return nil, nil, len(msgs)
	}
	if len(msgs) <= max {
		return msgs, cloneSourceRanges(sourceRanges), 0
	}

	sourceRanges = historySourceRanges(len(msgs), 0, nil, sourceRanges)

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

		trimmedRanges := make([][]CompactionRange, 0, head+tail)
		trimmedRanges = append(trimmedRanges, cloneSourceRanges(sourceRanges[:head])...)
		trimmedRanges = append(trimmedRanges, cloneSourceRanges(sourceRanges[tailStart:])...)
		return trimmed, trimmedRanges, sumCompactionRanges(flattenSourceRanges(sourceRanges[head:tailStart]))
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
	trimmedRanges := cloneSourceRanges(sourceRanges[start:])
	return trimmed, trimmedRanges, sumCompactionRanges(flattenSourceRanges(sourceRanges[:start]))
}

type compactionProjection struct {
	messages     []fantasy.Message
	summary      *StructuredSummary
	tokensBefore int
	tokensAfter  int
	sourceOffset int
	sourceCount  int
}

type transientSidecarCompacter struct {
	sidecar *sidecar.Sidecar
}

func (c transientSidecarCompacter) CompactStructured(ctx context.Context, conversationText, prevSummaryText, originalGoal string) (string, error) {
	return c.sidecar.CompactStructuredTransient(ctx, conversationText, prevSummaryText, originalGoal)
}

// buildTransientCompactionProjection builds the same verified structured
// projection as durable compaction without committing any coordinator state.
// In particular, this method must not update conversation history, the last
// summary, compaction records, metrics, session state, or files. It is used by
// per-stream context admission, where a projection can be abandoned after a
// request is rejected or a stream fails.
func (c *Coordinator) buildTransientCompactionProjection(ctx context.Context, messages []fantasy.Message, sourceOffset int, sourceCounts []int, predecessor *StructuredSummary) compactionProjection {
	var compacter SidecarCompacter
	if s := c.AgentPool().Sidecar(); s != nil {
		compacter = transientSidecarCompacter{sidecar: s}
	}
	return c.buildCompactionProjection(ctx, messages, sourceOffset, sourceCounts, predecessor, compacter)
}

func (c *Coordinator) buildCompactionProjection(ctx context.Context, messages []fantasy.Message, sourceOffset int, sourceCounts []int, predecessor *StructuredSummary, compacter SidecarCompacter) compactionProjection {
	if len(messages) < 1 {
		return compactionProjection{messages: messages}
	}
	sourceCounts = normalizeSourceCounts(len(messages), sourceCounts)
	sourceCount := sumSourceCounts(sourceCounts)

	// The predecessor is explicit so transient projections stay stream-local.
	// Durable compaction selects its persisted predecessor at its commit owner.
	prevSummary := cloneStructuredSummary(predecessor)

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
		return compactionProjection{messages: messages}
	}

	summary, err := PerformStructuredCompaction(ctx, compacter, messages, prevSummary, originalGoal)
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
			return compactionProjection{messages: messages}
		}
		summary = fallback
	}

	markdownSummary := summary.RenderMarkdown()
	tokensAfter := countTokensInText(compactionModel, markdownSummary)
	projection := compactionProjection{
		messages:     []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + verified.Content + "\n\n[Structured Compaction Summary]\n" + markdownSummary)},
		summary:      cloneStructuredSummary(summary),
		tokensBefore: tokensBefore,
		tokensAfter:  tokensAfter,
		sourceOffset: sourceOffset,
		sourceCount:  sourceCount,
	}
	return projection
}

// compactMessages is the canonical durable compaction path. The shared
// projection builder above is intentionally followed here by the existing
// durable state, record, and metric commits.
func (c *Coordinator) compactMessages(ctx context.Context, messages []fantasy.Message, sourceOffset int, sourceCounts []int) []fantasy.Message {
	workspace := c.sessionWorkspace()
	var predecessor *StructuredSummary
	if c.lastCompactionSummary != nil {
		predecessor = cloneStructuredSummary(c.lastCompactionSummary)
	} else if workspace != "" {
		predecessor = cloneStructuredSummary(GetLatestCompactionSummary(workspace))
	}
	projection := c.buildCompactionProjection(ctx, messages, sourceOffset, sourceCounts, predecessor, c.AgentPool().Sidecar())
	if projection.summary == nil {
		return projection.messages
	}

	if workspace != "" {
		checkpointCounts := []int{sumSourceCounts(sourceCounts)}
		record, err := c.commitCompactionCheckpoint(ctx, projection.messages, sourceOffset, checkpointCounts, sourceCounts, projection)
		if err != nil && record.ID == "" {
			log.Printf("warning: failed to save compaction record before canonical commit: %v", err)
			return messages
		}
		if err != nil {
			log.Printf("warning: durable compaction projection gap after canonical commit: %v", err)
		}
		c.lastCompactionSummary = cloneStructuredSummary(projection.summary)
		c.recordCompaction()
	} else {
		c.lastCompactionSummary = cloneStructuredSummary(projection.summary)
	}

	if c.think {
		c.emitThinkSidecar("Compact", fmt.Sprintf("compacted %d messages into structured summary (%d -> %d tokens)", len(messages), projection.tokensBefore, projection.tokensAfter))
	}

	return projection.messages
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
	c.sessionMu.Lock()
	c.sessionData = sd
	c.sessionMu.Unlock()
	if sd == nil {
		return
	}
	// SessionData is the durable canonical object shared by checkpointing and
	// callers inspecting SessionData(). Normalize in place before deriving any
	// coordinator projection, so legacy packets cannot be re-persisted through
	// a later checkpoint.
	for i := range sd.DiagnosticPackets {
		sd.DiagnosticPackets[i] = normalizeDiagnosticPacket(sd.DiagnosticPackets[i])
	}
	if sd.RunResult != nil {
		c.SetLastRunResult(sd.RunResult)
	}
	if c.phaseWorkflow != nil {
		if err := c.phaseWorkflow.restore(sd.WorkflowState, sd.PhaseResults, sd.RuntimeWorkspace, sd.RetryState); err != nil {
			log.Printf("warning: runtime workflow checkpoint ignored: %v", err)
		}
	}
	c.diagnosticPacketsMu.Lock()
	c.diagnosticPackets = append([]DiagnosticPacket(nil), sd.DiagnosticPackets...)
	c.diagnosticPacketsMu.Unlock()
	c.planRevisionsMu.Lock()
	c.planRevisions = make([]PlanRevision, 0, len(sd.PlanRevisions))
	for _, revision := range sd.PlanRevisions {
		c.planRevisions = append(c.planRevisions, clonePlanRevision(revision))
	}
	c.planRevisionsMu.Unlock()
	c.planReviewsMu.Lock()
	c.planReviews = make(map[string]PlanReviewResult, len(sd.PlanReviews))
	for _, review := range sd.PlanReviews {
		c.planReviews[review.RevisionID] = review
	}
	c.planReviewsMu.Unlock()
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
	// Legacy session files predate delegation_phase. Their durable TODO list is
	// the only compatible evidence: any restored task means the initial batch
	// is no longer pending; an empty/corrupt replacement session remains
	// pending. Never infer this from STM/LTM or conversation prose.
	if len(sd.Tasks) > 0 && !prof.DisableHistoricalTaskReuse && !prof.DisableJournalRestore {
		sd.DelegationPhase = DelegationPhaseActive
	} else if sd.DelegationPhase == "" {
		sd.DelegationPhase = DelegationPhaseInitialPending
	}
	if c.initialDelegationPending() {
		// A fresh/replacement session must not send stale in-memory conversation
		// turns to the first coordinator model call. Session entries, STM, LTM,
		// and vector memory are all non-canonical for delegation state; the
		// prompt builder withholds their persisted forms until phase advances.
		c.conversationHistoryMu.Lock()
		c.conversationHistory = nil
		c.conversationHistorySourceCounts = nil
		c.conversationHistorySourceRanges = nil
		c.conversationHistorySourceOffset = 0
		c.conversationHistoryNextSourceIndex = 0
		c.conversationHistoryMu.Unlock()
	}

	// A resumed session carries rounds from earlier runs; without this the
	// saved count restarts at this run's round and understates the session.
	// When DisableHistoricalTaskReuse is enabled (e.g. fresh-session or
	// fresh-verification),
	// prior rounds are not inherited so execution starts fresh at round 0.
	if !prof.DisableHistoricalTaskReuse {
		c.baseRounds = sd.Rounds
	} else {
		c.baseRounds = 0
	}
	c.applyLiveTaskProjection(sd.Tasks)
	if !prof.DisableHistoricalMemory && c.compactionState == nil && c.compactionRecoveryError() == nil {
		c.hydrateConversationHistoryFromSessionData()
	}
	// Canonical compaction state is restored only after initEventStore has
	// reconciled its branch-bound attestation. Until then session.json is not a
	// safe source for provider-visible coordinator history.
}

// SetFreshSession marks the next execution as a new session rather than a
// recovery of the active event-store lineage. The marker is consumed when the
// event store is initialized, where a new root branch is created.
func (c *Coordinator) SetFreshSession(v bool) {
	if c == nil {
		return
	}
	c.freshSession.Store(v)
	c.freshSessionMemory.Store(v)
	if v {
		// NewCoordinator can restore chat_history.md before the CLI applies
		// --new. Clear this in-memory projection defensively as well: a fresh
		// event-store branch without a fresh coordinator prompt is not a fresh
		// session from the user's perspective.
		c.conversationHistoryMu.Lock()
		c.conversationHistory = nil
		c.conversationHistorySourceCounts = nil
		c.conversationHistorySourceRanges = nil
		c.conversationHistorySourceOffset = 0
		c.conversationHistoryNextSourceIndex = 0
		if c.sessionData != nil {
			c.sessionData.ConversationHistorySourceCounts = nil
			c.sessionData.ConversationHistorySourceRanges = nil
			c.sessionData.ConversationHistorySourceOffset = 0
			c.sessionData.ConversationHistoryNextSourceIndex = 0
		}
		c.conversationHistoryMu.Unlock()
	}
}

// historicalMemoryDisabled reports whether this coordinator may consume
// context from an earlier session. --new retains that session as a durable
// archive, but it must not influence the fresh run that created the archive.
func (c *Coordinator) historicalMemoryDisabled() bool {
	return c == nil || c.ExecutionProfile().DisableHistoricalMemory || c.freshSessionMemory.Load()
}

func (c *Coordinator) applyLiveTaskProjection(tasks []*TodoItem) {
	if c == nil {
		return
	}
	prof := c.ExecutionProfile()
	if len(tasks) > 0 && !prof.DisableHistoricalTaskReuse && !prof.DisableJournalRestore {
		if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			c.taskTracker.TodoList().Restore(tasks)
		}
	}
	c.rebuildAntiThrashingState()

	if !prof.DisableHistoricalTaskReuse && !prof.DisableJournalRestore {
		c.taskResultCacheMu.Lock()
		if c.taskResultCache == nil {
			c.taskResultCache = make(map[string][]cachedTaskEntry)
		}
		gen := c.cacheGeneration.Load()
		for _, t := range tasks {
			if t != nil && t.Status == TaskDone && t.Output != "" {
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

	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		c.taskTracker.TodoList().onChange = func() {
			c.saveCheckpoint()
			c.reconcileTaskStatusProjection()
		}
	}
	c.reconcileTaskStatusProjection()
}

// ContinuationCheckpoint returns a copy of the persisted continuation state.
func (c *Coordinator) ContinuationCheckpoint() *ContinuationCheckpoint {
	if c == nil || c.sessionData == nil || c.sessionData.ContinuationCheckpoint == nil {
		return nil
	}
	cp := *c.sessionData.ContinuationCheckpoint
	if c.sessionData.ContinuationCheckpoint.NoProgress != nil {
		progress := *c.sessionData.ContinuationCheckpoint.NoProgress
		cp.NoProgress = &progress
	}
	return &cp
}

func (c *Coordinator) saveContinuationCheckpoint(turn, maxTurns int, reason, status string) {
	if c == nil || c.sessionData == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	progress := c.noProgressCounters()
	replanPending := c.noProgressReplanPending()
	c.sessionData.ContinuationCheckpoint = &ContinuationCheckpoint{
		TurnCount:               turn,
		MaxTurns:                maxTurns,
		Reason:                  reason,
		Status:                  status,
		NoProgress:              &progress,
		NoProgressReplanPending: replanPending,
	}
	_ = c.SessionStore().SaveSession(c.session.Workspace, c.sessionData)
	c.emitEvent("coordinator_continuation_checkpoint", "coordinator", "", map[string]interface{}{"turn_count": turn, "max_turns": maxTurns, "reason": reason, "status": status, "no_progress": progress, "no_progress_replan_pending": replanPending})
}

// ResumeContinuationCheckpoint persists the transition from an interrupted
// continuation to a new run, allowing callers to query the restart boundary.
func (c *Coordinator) ResumeContinuationCheckpoint() *ContinuationCheckpoint {
	cp := c.ContinuationCheckpoint()
	if cp == nil || (cp.Status != "pending" && cp.Status != "aborted") {
		return cp
	}
	resume := *cp
	if cp.NoProgress != nil {
		progress := *cp.NoProgress
		c.metricsMu.Lock()
		c.tokensSinceCriterionProgress = progress.Tokens
		c.turnsSinceCriterionProgress = progress.Turns
		c.tasksSinceCriterionProgress = progress.Tasks
		c.noProgressReplanTripped = cp.NoProgressReplanPending
		c.noProgressStopTripped = false
		c.reliabilityUsageByAttempt = make(map[string]int)
		c.metricsMu.Unlock()
	}
	c.continuationResume = &resume
	c.saveContinuationCheckpoint(cp.TurnCount, cp.MaxTurns, cp.Reason, "resumed")
	return c.ContinuationCheckpoint()
}

func (c *Coordinator) hydrateConversationHistoryFromSessionData() {
	if c.sessionData == nil || len(c.conversationHistory) == 0 {
		return
	}

	c.conversationHistorySourceRanges = historySourceRanges(len(c.conversationHistory), c.sessionData.ConversationHistorySourceOffset, c.sessionData.ConversationHistorySourceCounts, c.sessionData.ConversationHistorySourceRanges)
	c.conversationHistorySourceCounts = sourceCountsForRanges(c.conversationHistorySourceRanges)

	if c.sessionData.ConversationHistorySourceOffset > 0 || len(c.conversationHistorySourceCounts) > 0 {
		c.conversationHistorySourceOffset = c.sessionData.ConversationHistorySourceOffset
	}
	c.conversationHistoryNextSourceIndex = c.sessionData.ConversationHistoryNextSourceIndex
	if c.conversationHistoryNextSourceIndex < maxSourceIndex(c.conversationHistorySourceRanges) {
		c.conversationHistoryNextSourceIndex = maxSourceIndex(c.conversationHistorySourceRanges)
	}
	if c.conversationHistorySourceOffset < 0 {
		c.conversationHistorySourceOffset = 0
	}
}

func (c *Coordinator) syncConversationHistoryStateToSessionData() {
	if c.conversationHistorySourceOffset < 0 {
		c.conversationHistorySourceOffset = 0
	}
	_ = c.mutateSessionData(func(sd *SessionData) error {
		sd.ConversationHistorySourceOffset = c.conversationHistorySourceOffset
		sd.ConversationHistorySourceRanges = cloneSourceRanges(c.conversationHistorySourceRanges)
		sd.ConversationHistoryNextSourceIndex = c.conversationHistoryNextSourceIndex
		if len(c.conversationHistorySourceCounts) == 0 {
			sd.ConversationHistorySourceCounts = nil
		} else {
			sd.ConversationHistorySourceCounts = append([]int(nil), c.conversationHistorySourceCounts...)
		}
		return nil
	})
}

// saveCheckpoint commits the durable task projection to the event store and
// reflects it into session.json. Parallel task goroutines call this
// concurrently, so the sessionData read-modify-write (Tasks, WorkflowState,
// PhaseResults, RetryState) and the subsequent SaveSession are serialized
// through sessionMu: mutate under the write lock, snapshot under the read
// lock, then persist the snapshot and update the branch state outside the lock.
func (c *Coordinator) saveCheckpoint() {
	var hasSessionData bool
	c.viewSessionData(func(*SessionData) {
		hasSessionData = true
	})
	if !hasSessionData || c.session == nil || c.session.Workspace == "" {
		return
	}
	// Task state is canonical in the event store.  Commit its complete replay
	// projection before allowing session.json to advertise the newer state.
	// In particular, a completed task must never reach the checkpoint unless
	// its terminal event (including receipt and typed result) is durable.
	tasks := c.taskTracker.TodoList().Items()
	c.emitPendingDiagnosticPackets()
	if err := c.emitTaskEventsFromCheckpoint(tasks); err != nil {
		log.Printf("warning: checkpoint deferred until canonical task events are durable: %v", err)
		return
	}

	// Resolve the store before taking sessionMu: SessionStore() acquires c.mu
	// internally, and nesting it under the sessionMu write lock would deadlock.
	// Hold sessionMu across the mutate and SaveSession so the marshal cannot
	// race a concurrent writer.
	store := c.SessionStore()
	c.sessionMu.Lock()
	c.sessionData.Tasks = tasks
	worksetReceipts, receiptErr := c.worksetReceiptsFromTasks(tasks)
	if receiptErr != nil {
		c.sessionData.WorksetReceipts = nil
		c.sessionData.RecoveryRequired = true
		c.sessionData.RecoveryReason = receiptErr.Error()
	} else {
		c.sessionData.WorksetReceipts = worksetReceipts
	}
	c.sessionData.WorksetStates = c.WorksetGroupStates()
	if c.phaseWorkflow != nil {
		c.sessionData.WorkflowState, c.sessionData.PhaseResults, c.sessionData.RuntimeWorkspace, c.sessionData.RetryState = c.phaseWorkflow.snapshot()
	}
	_ = store.SaveSession(c.session.Workspace, c.sessionData)
	c.sessionMu.Unlock()
	c.updateBranchState()
}

func (c *Coordinator) worksetReceiptsFromTasks(tasks []*TodoItem) ([]WorksetExpansionReceipt, error) {
	visible := make([]*WorksetExpansionReceipt, 0, len(tasks))
	for _, item := range tasks {
		if item == nil || item.WorksetReceipt == nil {
			continue
		}
		visible = append(visible, item.WorksetReceipt)
	}
	indexed, conflicts := collectWorksetReceipts(visible)
	if err := worksetReceiptConflictError(conflicts); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(indexed))
	receipts := make([]WorksetExpansionReceipt, 0, len(indexed))
	for _, item := range tasks {
		if item == nil || item.WorksetReceipt == nil {
			continue
		}
		id := item.WorksetReceipt.WorksetID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		receipts = append(receipts, *cloneWorksetReceipt(indexed[id]))
	}
	return receipts, nil
}

// updateBranchState snapshots the coordinator's live state (task plan, active
// model, selected team, latest compaction summary) into the active session
// branch, so `hufu session` checkout/time-travel can restore it later (§8).
// Best-effort: any failure leaves the checkpoint path unaffected. The task
// plan is read from a session snapshot so a concurrent checkpoint cannot race
// the read.
func (c *Coordinator) updateBranchState() {
	st, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return
	}
	b := st.Branches[st.ActiveBranch]
	if b == nil {
		return
	}
	c.viewSessionData(func(sd *SessionData) {
		if len(sd.Tasks) > 0 {
			plan := make([]*TodoItem, len(sd.Tasks))
			for i, t := range sd.Tasks {
				plan[i] = cloneTodoItem(t)
			}
			b.State.TaskPlan = plan
		}
	})
	b.State.ActiveModel = c.session.Config.Generation.Model
	b.State.SelectedTeam = c.session.Config.Name
	if c.lastCompactionSummary != nil {
		b.State.Compaction = cloneStructuredSummary(c.lastCompactionSummary)
	}
	_ = SaveSessionTree(c.session.Workspace, st)
}

// isInterruptedStatus reports whether a restored task status indicates the task
// was left incomplete by an interrupted (crashed/killed) run and needs resume
// handling. TaskProtocolIncomplete is included for selection, but it is
// handled by the result-only repair gate rather than ordinary worker replay.
// Terminal states (done/skipped) and definitively-failed tasks (error, which
// already exhausted their retries) are left untouched.
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

// ResumeInterruptedTasks resumes tasks that a previous run left in-flight
// (restored from the session checkpoint). Ordinary interrupted tasks are
// evaluated against side-effect class and recovery policy (§11.2-11.4), while
// TaskProtocolIncomplete is always routed through result-only repair. Tasks
// with policy 'retry' or reconciled as 'not_started' are re-executed;
// 'manual' tasks are blocked and flagged for human review; 'never' tasks are
// skipped (left as-is) since the policy declares they must not be re-driven.
//
//nolint:gocyclo // The recovery matrix is intentionally explicit to preserve fail-closed side-effect semantics.
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
		if it.Status == TaskProtocolIncomplete {
			c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{
				"policy":        string(pol),
				"decision":      "protocol_result_only_repair",
				"worker_replay": false,
			})
			if _, err := c.resumeProtocolIncompleteTask(ctx, task, it); err != nil && firstErr == nil {
				firstErr = err
			}
			count++
			continue
		}
		switch pol {
		case RecoveryRetry:
			if !IsTaskReplayable(task) {
				detail := fmt.Sprintf("task blocked by replay policy; side_effect=%s or allows_replay=false", it.SideEffect)
				c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, ReconcileOnly, FailurePolicy, TaskBlocked)
				c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{"policy": string(pol), "decision": "replay_blocked", "reason": detail})
				c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
				continue
			}
			if err := c.CommitTaskResetForRetry(ctx, it.ID, "resumed after interruption"); err != nil {
				return count, err
			}
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
			c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, NeedsHuman, FailurePolicy, TaskBlocked)
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
			if err := c.commitTaskTransitionFromCurrent(ctx, it.ID, TaskSkipped, detail, "", nil); err != nil {
				return count, err
			}
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
				// Reconciliation establishes that the operation completed; it is
				// not acceptance proof. Re-run affected criteria now so external
				// state changed after the old checkpoint cannot be marked done.
				if err := c.revalidateRecoveryCriteria(ctx, it); err != nil {
					detail := fmt.Sprintf("reconciliation completed but criterion re-validation failed: %v", err)
					c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, NeedsHuman, FailureVerify, TaskBlocked)
					c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
					c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
					c.emitEvent("criterion_checkpoint_rejected", "coordinator", it.ID, map[string]interface{}{"reason": detail})
					continue
				}
				if err := c.commitTaskTransitionFromCurrent(ctx, it.ID, TaskDone, "reconciliation confirmed task was completed", "", nil); err != nil {
					return count, err
				}
				c.reconcileTaskStatusProjection()
				current := it
				for _, candidate := range c.taskTracker.TodoList().Items() {
					if candidate.ID == it.ID {
						current = candidate
						break
					}
				}
				if requiresCriterionCheckpoint(current) {
					if c.sessionData == nil {
						return count, fmt.Errorf("criterion checkpoint recovery requires session data")
					}
					c.recordCriterionCheckpoints(current, c.sessionData.CriterionResults)
					if err := c.validateCriterionCheckpoint(current); err != nil {
						detail := fmt.Sprintf("fresh criterion checkpoint rejected: %v", err)
						c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, NeedsHuman, FailureVerify, TaskBlocked)
						c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
						continue
					}
				}
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
					c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, ReconcileOnly, FailurePolicy, TaskBlocked)
					c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{"policy": string(pol), "recovery_state": state, "decision": "replay_blocked", "reason": detail})
					c.report(c.newEvent("needs_human").withMessage(detail).withTodoID(it.ID))
					continue
				}
				if err := c.CommitTaskResetForRetry(ctx, it.ID, "reconciliation allowed retry"); err != nil {
					return count, err
				}
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
				c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, NeedsHuman, FailurePolicy, status)
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
			c.PersistFailureWithClassAndStatus(it.Agent, it.Desc, it.ID, detail, NeedsHuman, FailurePolicy, TaskBlocked)
			c.emitEvent("recovery_decision", "coordinator", it.ID, map[string]interface{}{"policy": string(pol), "decision": "unknown_policy_blocked"})
		}
	}
	return count, firstErr
}

func taskDefFromTodoItem(it *TodoItem) TaskDef {
	if it == nil {
		return TaskDef{}
	}
	id := it.PlanTaskID
	if id == "" {
		id = it.ID
	}
	return TaskDef{
		ID: id, Phase: it.Phase, Action: cloneActionPtr(it.Action), Agent: it.Agent, Goal: it.Desc, Verify: it.Verify, VerifyMode: it.VerifyMode,
		VerifySpec: cloneVerificationSpecPtr(it.VerifySpec), SideEffect: it.SideEffect,
		Recovery: it.Recovery, ReconcileTool: it.ReconcileTool, Execution: it.Execution,
		Kind: it.Kind, Advances: append([]string(nil), it.Advances...),
		ExpectedStateChange: it.ExpectedStateChange, RecoveryHypothesis: cloneRecoveryHypothesis(it.RecoveryHypothesis),
	}
}

func (c *Coordinator) SessionData() *SessionData {
	var snapshot *SessionData
	c.viewSessionData(func(sd *SessionData) {
		copySD := *sd
		copySD.Entries = append([]SessionEntry(nil), sd.Entries...)
		copySD.Tasks = append([]*TodoItem(nil), sd.Tasks...)
		snapshot = &copySD
	})
	return snapshot
}

func (c *Coordinator) saveHistoryAndSession(ctx context.Context, steps []fantasy.StepResult) {
	c.conversationHistoryMu.Lock()
	c.appendHistory(ctx, steps)
	if c.session != nil && c.session.Workspace != "" {
		_ = SaveConversationHistory(c.session.Workspace, c.conversationHistory)
	}
	c.syncConversationHistoryStateToSessionData()
	c.conversationHistoryMu.Unlock()
	_ = c.persistSession("persist conversation history")
}
