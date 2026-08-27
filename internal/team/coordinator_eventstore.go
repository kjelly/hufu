package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

// RecordSessionUserMessage commits the event before advancing the in-memory
// session projection. This is deliberately event-first: an append failure
// cannot leave conversation history ahead of its canonical replay source.
func RecordSessionUserMessage(session *SessionData, es *EventStore, content string) error {
	return recordSessionMessage(session, es, "user", content)
}

// RecordSessionAssistantMessage commits the event before advancing the
// in-memory session projection.
func RecordSessionAssistantMessage(session *SessionData, es *EventStore, content string) error {
	return recordSessionMessage(session, es, "assistant", content)
}

func recordSessionMessage(session *SessionData, es *EventStore, role, content string) error {
	if session == nil || strings.TrimSpace(content) == "" {
		return nil
	}
	if es != nil {
		payload, _ := json.Marshal(map[string]string{
			"role":    role,
			"content": utils.RedactSecrets(content),
		})
		eventType := EventAssistantMessageAdded
		if role == "user" {
			eventType = EventUserMessageAdded
		}
		durable, err := es.AppendPersisted(RunEvent{
			Type:    string(eventType),
			Actor:   role,
			Payload: payload,
		})
		if err != nil {
			log.Printf("warning: append %s event failed: %v", eventType, err)
			return fmt.Errorf("append %s event: %w", eventType, err)
		}
		session.addEntryAt(role, content, durable.Timestamp)
		return nil
	}
	session.AddEntry(role, content)
	return nil
}

func (c *Coordinator) addSessionUserMessage(content string) {
	if c == nil {
		return
	}
	if err := c.recordSessionMessage(content, "user"); err != nil {
		c.dualWriteFailures.Add(1)
	}
}

func (c *Coordinator) addSessionAssistantMessage(content string) {
	if c == nil {
		return
	}
	if err := c.recordSessionMessage(content, "assistant"); err != nil {
		c.dualWriteFailures.Add(1)
	}
}

func (c *Coordinator) recordSessionMessage(content, role string) error {
	if c == nil || strings.TrimSpace(content) == "" {
		return nil
	}
	if !c.hasDurableEventJournal() {
		return c.mutateSessionData(func(sd *SessionData) error { sd.AddEntry(role, content); return nil })
	}
	payload, err := json.Marshal(SessionMessageEventPayload{Role: role, Content: utils.RedactSecrets(content)})
	if err != nil {
		return fmt.Errorf("marshal %s message: %w", role, err)
	}
	eventType := EventAssistantMessageAdded
	if role == "user" {
		eventType = EventUserMessageAdded
	}
	durable, err := c.EventJournal().Append(context.Background(), RunEvent{Type: string(eventType), Actor: role, Payload: payload})
	if err != nil {
		return fmt.Errorf("append %s event: %w", eventType, err)
	}
	return c.mutateSessionData(func(sd *SessionData) error { sd.addEntryAt(role, content, durable.Timestamp); return nil })
}

// initEventStore initializes the EventStore on Coordinator.
func (c *Coordinator) initEventStore() {
	if c.session == nil || c.session.Workspace == "" {
		return
	}
	_ = c.mutateSessionData(func(*SessionData) error { return nil })
	runID := c.executionRunID
	if runID == "" {
		runID = fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	sessionID := filepath.Base(c.session.Workspace)
	es, err := NewEventStore(c.session.Workspace, runID, sessionID)
	if err != nil {
		log.Printf("warning: init event store failed: %v", err)
		c.markSessionRecovery("event-store initialization failed: " + utils.RedactSecrets(err.Error()))
		return
	}
	// NewEventStore's strict rescan already validates every persisted record and
	// restores the durable chain head. Do not replay the complete history a
	// second time through VerifyHashChain during coordinator startup.
	st, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		log.Printf("warning: load session tree failed: %v", err)
		c.markSessionRecovery("session-tree load failed: " + utils.RedactSecrets(err.Error()))
		_ = es.Close()
		return
	}
	activeBranch := "main"
	if c.freshSession.CompareAndSwap(true, false) {
		branch, branchErr := st.CreateRootBranch(fmt.Sprintf("session-%d", time.Now().UTC().UnixNano()))
		if branchErr != nil {
			log.Printf("warning: create fresh event-store branch failed: %v", branchErr)
			c.markSessionRecovery("fresh event-store branch creation failed: " + utils.RedactSecrets(branchErr.Error()))
			_ = es.Close()
			return
		}
		st.ActiveBranch = branch.ID
		if saveErr := SaveSessionTree(c.session.Workspace, st); saveErr != nil {
			log.Printf("warning: save fresh event-store branch failed: %v", saveErr)
			c.markSessionRecovery("fresh event-store branch save failed: " + utils.RedactSecrets(saveErr.Error()))
			_ = es.Close()
			return
		}
		activeBranch = branch.ID
	} else if st != nil && st.ActiveBranch != "" {
		activeBranch = st.ActiveBranch
	}
	es.SetBranchID(activeBranch)
	c.eventStore = es
	c.SetEventJournal(eventStoreJournal{store: es})
	c.resetContextWindowTelemetrySummary()
	// Hydrate the active branch during every normal startup. Pending terminal
	// reconciliation is a recovery concern and must not also replay telemetry.
	if events, readErr := es.ReadEvents(); readErr != nil {
		c.markSessionRecovery("event-store telemetry hydration read failed: " + utils.RedactSecrets(readErr.Error()))
	} else {
		c.hydrateContextWindowTelemetry(FilterEventsForBranch(events, st, activeBranch))
	}
	// Reconcile and validate the canonical event binding before publishing any
	// restored history to the coordinator/provider path.
	if compactionErr := c.reconcileCompactionState(context.Background(), activeBranch); compactionErr != nil {
		c.markCompactionRecovery(compactionErr)
	} else if compactionErr := c.restoreCanonicalCompactionForBranch(activeBranch); compactionErr != nil {
		c.markCompactionRecovery(compactionErr)
	}
	pending, reconciled := c.reconcilePendingTerminalCommit(st, activeBranch)
	if pending && !reconciled {
		// A pending terminal append is an admission barrier. Do not let the
		// general projection shadow repair overwrite its recovery state.
		return
	}
	c.hydrateEmittedEventKeys(st, activeBranch)
	c.checkCanonicalProjectionShadow(st, activeBranch)
	c.repairMemoryLearningGaps(st, activeBranch)
}

// reconcilePendingTerminalCommit is the only restart repair for an uncertain
// terminal append. It accepts exactly one matching run_finished event from the
// active branch lineage, reduces that lineage, and then advances projections.
// It never appends or replays the terminal side effect.
func (c *Coordinator) reconcilePendingTerminalCommit(st *SessionTree, activeBranch string) (present, reconciled bool) {
	if c == nil || c.eventStore == nil {
		return false, false
	}
	var pending *PendingTerminalCommit
	c.viewSessionData(func(sd *SessionData) {
		if sd.PendingTerminalCommit != nil {
			copyPending := *sd.PendingTerminalCommit
			pending = &copyPending
		}
	})
	if pending == nil {
		return false, false
	}
	invalid := func(reason string) (bool, bool) {
		c.markTerminalRecoveryPending(reason, pending)
		return true, false
	}
	if strings.TrimSpace(pending.RunID) == "" || strings.TrimSpace(pending.IdempotencyKey) == "" || strings.TrimSpace(pending.BranchID) == "" {
		return invalid("pending terminal commit identity is incomplete")
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return invalid("pending terminal commit reconciliation read failed: " + utils.RedactSecrets(err.Error()))
	}
	lineage := FilterEventsForBranch(events, st, activeBranch)
	matchIndex := -1
	for index, event := range lineage {
		if event.Type != "run_finished" || event.RunID != pending.RunID || event.IdempotencyKey != pending.IdempotencyKey || event.BranchID != pending.BranchID {
			continue
		}
		if matchIndex != -1 {
			return invalid("pending terminal commit matched multiple run_finished events")
		}
		matchIndex = index
	}
	if matchIndex == -1 {
		return invalid("pending terminal commit has no matching run_finished event")
	}
	projected := ReduceToSessionData(lineage[:matchIndex+1])
	if projected == nil || projected.RunResult == nil || projected.RunResult.RunID != pending.RunID {
		return invalid("pending terminal commit run_finished result identity is invalid")
	}
	result := projected.RunResult
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.RunResult = result
		sd.PendingTerminalCommit = nil
		sd.RecoveryRequired = false
		sd.RecoveryReason = ""
		return nil
	}); err != nil {
		return invalid("persist reconciled terminal result failed: " + utils.RedactSecrets(err.Error()))
	}
	if err := c.persistSession("persist reconciled terminal result"); err != nil {
		return invalid("persist reconciled terminal result failed: " + utils.RedactSecrets(err.Error()))
	}
	c.setLastRunResultInMemory(result)
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		c.taskTracker.TodoList().SetRunID(result.RunID)
	}
	c.reconcileTerminalStatusProjection(result)
	return true, true
}

// clearTerminalRecoveryAfterCommit advances only canonical projections after
// the matching run_finished event has been durably appended. Recovery is
// cleared only when the pending marker belongs to this exact run, idempotency
// key, and branch; unrelated recovery state remains an admission barrier.
func (c *Coordinator) clearTerminalRecoveryAfterCommit(result *RunResult, committed *PendingTerminalCommit) {
	if c == nil || result == nil || committed == nil {
		return
	}
	store := c.SessionStore()
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.terminalLifecycleMu.Lock()
	if c.terminalLifecycleRunID != committed.RunID {
		c.terminalLifecycleMu.Unlock()
		return
	}
	if c.sessionData == nil {
		c.sessionData = NewSession()
	}
	if pending := c.sessionData.PendingTerminalCommit; pending != nil &&
		pending.RunID == committed.RunID &&
		pending.IdempotencyKey == committed.IdempotencyKey &&
		pending.BranchID == committed.BranchID {
		c.sessionData.PendingTerminalCommit = nil
		c.sessionData.RecoveryRequired = false
		c.sessionData.RecoveryReason = ""
	}
	// The event is now canonical, so publishing this detached snapshot into the
	// session projection is event-first and safe even when no recovery marker
	// was present.
	c.sessionData.RunResult = result
	c.terminalLifecycleMu.Unlock()
	if c.session != nil && c.session.Workspace != "" {
		if err := store.SaveSession(c.session.Workspace, c.sessionData); err != nil {
			log.Printf("warning: persist canonical terminal projection failed: %v", err)
		}
	}
}

func (c *Coordinator) markSessionRecovery(reason string) {
	if c == nil {
		return
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.RecoveryRequired = true
		sd.RecoveryReason = reason
		return nil
	}); err == nil {
		_ = c.persistSession("persist recovery state")
	}
}

func (c *Coordinator) checkCanonicalProjectionShadow(st *SessionTree, activeBranch string) {
	if c == nil || c.eventStore == nil {
		return
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		c.markSessionRecovery("event-store read failed: " + utils.RedactSecrets(err.Error()))
		return
	}
	lineage := FilterEventsForBranch(events, st, activeBranch)
	if !hasCurrentCanonicalProjectionEvents(lineage) {
		return
	}

	replayedSD := ReduceToSessionData(lineage)
	replayedTasks := ReduceToTodoList(lineage)

	var emptyProjection bool
	c.viewSessionData(func(sd *SessionData) {
		emptyProjection = len(sd.Entries) == 0 && len(sd.Tasks) == 0
	})
	if emptyProjection {
		c.replaceCanonicalProjection(replayedSD, replayedTasks)
		c.applyLiveTaskProjection(replayedTasks)
		prof := c.ExecutionProfile()
		if !prof.DisableHistoricalMemory {
			c.hydrateConversationHistoryFromSessionData()
		}
		if c.initialDelegationPending() {
			c.conversationHistoryMu.Lock()
			c.conversationHistory = nil
			c.conversationHistorySourceCounts = nil
			c.conversationHistorySourceRanges = nil
			c.conversationHistorySourceOffset = 0
			c.conversationHistoryNextSourceIndex = 0
			c.conversationHistoryMu.Unlock()
		}
		_ = c.persistSession("persist rebuilt canonical projection")
		_ = c.emitEvent("projection_rebuilt", "coordinator", "", map[string]string{"source": "event_store", "status": "rebuilt"})
		return
	}

	var comparisonErr error
	var liveTasks []*TodoItem
	c.viewSessionData(func(sd *SessionData) {
		comparisonErr = CompareCanonicalProjection(sd, lineage)
		liveTasks = append([]*TodoItem(nil), sd.Tasks...)
	})
	if comparisonErr == nil {
		if len(liveTasks) > 0 && (c.taskTracker == nil || len(c.taskTracker.TodoList().Items()) == 0) {
			c.applyLiveTaskProjection(liveTasks)
		}
		_ = c.emitEvent("projection_rebuilt", "coordinator", "", map[string]string{"source": "event_store", "status": "matched"})
		_ = c.persistSession("persist matched canonical projection")
		return
	}

	var recoverable bool
	c.viewSessionData(func(sd *SessionData) {
		recoverable = isProjectionPrefixOrRecoverable(sd, replayedSD, replayedTasks)
	})
	if recoverable {
		c.replaceCanonicalProjection(replayedSD, replayedTasks)
		c.applyLiveTaskProjection(replayedTasks)
		prof := c.ExecutionProfile()
		if !prof.DisableHistoricalMemory {
			c.hydrateConversationHistoryFromSessionData()
		}
		if c.initialDelegationPending() {
			c.conversationHistoryMu.Lock()
			c.conversationHistory = nil
			c.conversationHistorySourceCounts = nil
			c.conversationHistorySourceRanges = nil
			c.conversationHistorySourceOffset = 0
			c.conversationHistoryNextSourceIndex = 0
			c.conversationHistoryMu.Unlock()
		}
		_ = c.persistSession("persist repaired canonical projection")
		_ = c.emitEvent("projection_rebuilt", "coordinator", "", map[string]string{"source": "event_store", "status": "repaired"})
		return
	}

	reason := "event-store projection mismatch: " + utils.RedactSecrets(comparisonErr.Error())
	_ = c.mutateSessionData(func(sd *SessionData) error {
		sd.RecoveryRequired = true
		sd.RecoveryReason = reason
		return nil
	})
	_ = c.emitEvent("projection_mismatch", "coordinator", "", map[string]string{"reason": reason})
	if saveErr := c.persistSession("persist projection mismatch"); saveErr != nil {
		_ = c.emitEvent("projection_write_failed", "coordinator", "", map[string]string{"reason": utils.RedactSecrets(saveErr.Error())})
	}
}

// replaceCanonicalProjection updates only the session projection. Its callers
// rebuild task and conversation projections after releasing sessionMu, because
// those operations may re-enter checkpointing paths that also acquire it.
func (c *Coordinator) replaceCanonicalProjection(replayedSD *SessionData, replayedTasks []*TodoItem) {
	if replayedSD == nil {
		return
	}
	_ = c.mutateSessionData(func(sd *SessionData) error {
		sd.Entries = replayedSD.Entries
		sd.Tasks = replayedTasks
		sd.WorksetReceipts = append([]WorksetExpansionReceipt(nil), replayedSD.WorksetReceipts...)
		sd.WorksetStates = append([]WorksetGroupState(nil), replayedSD.WorksetStates...)
		sd.RecoveryRequired = replayedSD.RecoveryRequired
		sd.RecoveryReason = replayedSD.RecoveryReason
		sd.CriterionResults = replayedSD.CriterionResults
		sd.CriterionCheckpoints = replayedSD.CriterionCheckpoints
		sd.LastCriterionProgressAt = replayedSD.LastCriterionProgressAt
		if len(replayedTasks) > 0 {
			sd.DelegationPhase = DelegationPhaseActive
		} else if replayedSD.DelegationPhase != "" {
			sd.DelegationPhase = replayedSD.DelegationPhase
		}
		return nil
	})
}

func isProjectionPrefixOrRecoverable(live, replayedSD *SessionData, replayedTasks []*TodoItem) bool {
	if live == nil || replayedSD == nil {
		return false
	}
	if len(live.Entries) > len(replayedSD.Entries) {
		return false
	}
	for i := range live.Entries {
		if live.Entries[i].Role != replayedSD.Entries[i].Role || live.Entries[i].Content != replayedSD.Entries[i].Content {
			return false
		}
	}
	if len(live.Tasks) > len(replayedTasks) {
		return false
	}
	for i := range live.Tasks {
		if err := compareSingleTaskProjection(live.Tasks[i], replayedTasks[i]); err != nil {
			return false
		}
	}
	return true
}

// hydrateEmittedEventKeys restores idempotency state after a process
// restart. Without this, the first checkpoint after resume re-emits every
// previously persisted transition despite the event already being present.
func (c *Coordinator) hydrateEmittedEventKeys(branchOpts ...any) {
	if c == nil || c.eventStore == nil {
		return
	}
	var st *SessionTree
	activeBranch := "main"
	for _, opt := range branchOpts {
		switch v := opt.(type) {
		case *SessionTree:
			st = v
		case string:
			if v != "" {
				activeBranch = v
			}
		}
	}
	if st != nil && st.ActiveBranch != "" && len(branchOpts) == 1 {
		activeBranch = st.ActiveBranch
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		log.Printf("warning: hydrate event idempotency state failed: %v", err)
		c.dualWriteFailures.Add(1)
		return
	}
	lineage := FilterEventsForBranch(events, st, activeBranch)
	c.eventOnceMu.Lock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	for _, event := range lineage {
		if event.IdempotencyKey != "" {
			c.emittedTaskTransitions[event.IdempotencyKey] = true
		}
	}
	c.eventOnceMu.Unlock()
	// A process can crash after the durable event append and before the SQLite
	// projection commit. Reapplying memory events at startup is safe because the
	// reducer transaction uses the same durable idempotency key.
	for _, event := range lineage {
		switch event.Type {
		case "memory_retrieved", "memory_usage_recorded", "memory_outcome_recorded":
			c.reduceMemoryEvent(event)
		}
	}
}

// emitEventOnce appends an event at most once for a durable idempotency key.
// The same key set is hydrated from the event log at startup and is shared by
// task transitions, artifacts, and memory learning events.
func (c *Coordinator) emitEventOnce(idempotencyKey string, event RunEvent) (bool, error) {
	if c == nil || c.eventStore == nil {
		return false, nil
	}
	if idempotencyKey == "" {
		return false, errors.New("emit event once: empty idempotency key")
	}
	c.eventOnceMu.Lock()
	defer c.eventOnceMu.Unlock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	if c.emittedTaskTransitions[idempotencyKey] {
		return false, nil
	}
	event.IdempotencyKey = idempotencyKey
	if _, err := c.eventStore.AppendPersisted(event); err != nil {
		c.recordLearningGap(event, err)
		return false, err
	}
	c.emittedTaskTransitions[idempotencyKey] = true
	return true, nil
}

func (c *Coordinator) recordLearningGap(event RunEvent, appendErr error) {
	if c == nil || appendErr == nil {
		return
	}
	c.dualWriteFailures.Add(1)
	var repairEvent *RunEvent
	if strings.HasPrefix(event.Type, "memory_") && len(event.Payload) > 0 {
		copyEvent := event
		if redacted, err := utils.RedactJSON(event.Payload); err == nil {
			copyEvent.Payload = redacted
		}
		repairEvent = &copyEvent
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.LearningGaps = append(sd.LearningGaps, LearningGap{
			EventType: event.Type, TaskID: event.TaskID, IdempotencyKey: event.IdempotencyKey,
			Reason: utils.RedactSecrets(appendErr.Error()), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
			PendingRepair: true, RepairEvent: repairEvent,
		})
		return nil
	}); err != nil {
		return
	}
	// The gap must be durable before the failed emission path returns: a crash
	// before the next unrelated checkpoint would otherwise lose the repair
	// record and its event, leaving no way to rebuild the observation without
	// re-running the worker (spec §7 HF-MEM4-000 item 4, §9).
	if err := c.persistSession("persist learning gap"); err != nil {
		log.Printf("warning: persist learning gap checkpoint failed: %v", err)
	}
}

func (c *Coordinator) repairMemoryLearningGaps(branchOpts ...any) {
	if c == nil || c.eventStore == nil || c.sessionData == nil {
		return
	}
	var st *SessionTree
	activeBranch := "main"
	for _, opt := range branchOpts {
		switch v := opt.(type) {
		case *SessionTree:
			st = v
		case string:
			if v != "" {
				activeBranch = v
			}
		}
	}
	if st != nil && st.ActiveBranch != "" && len(branchOpts) == 1 {
		activeBranch = st.ActiveBranch
	}
	var gaps []LearningGap
	c.viewSessionData(func(sd *SessionData) { gaps = append(gaps, sd.LearningGaps...) })
	originalGapCount := len(gaps)
	changed := false
	for i := range gaps {
		gap := &gaps[i]
		if !gap.PendingRepair {
			continue
		}
		repaired := false
		if gap.RepairEvent != nil {
			event := *gap.RepairEvent
			_, err := c.emitEventOnce(gap.IdempotencyKey, event)
			if err == nil {
				events, readErr := c.eventStore.ReadEvents()
				if readErr == nil {
					lineage := FilterEventsForBranch(events, st, activeBranch)
					for _, durableEvent := range lineage {
						if durableEvent.IdempotencyKey == gap.IdempotencyKey {
							c.reduceMemoryEvent(durableEvent)
							repaired = true
							break
						}
					}
				}
			}
		} else if gap.EventType == "memory_aggregate_repair" {
			events, err := c.eventStore.ReadEvents()
			if err == nil {
				lineage := FilterEventsForBranch(events, st, activeBranch)
				for _, event := range lineage {
					if event.IdempotencyKey == gap.IdempotencyKey {
						c.reduceMemoryEvent(event)
						repaired = true
						break
					}
				}
			}
		}
		if repaired {
			gap.PendingRepair = false
			changed = true
		}
	}
	if !changed {
		return
	}
	_ = c.mutateSessionData(func(sd *SessionData) error {
		if len(sd.LearningGaps) > originalGapCount {
			gaps = append(gaps, sd.LearningGaps[originalGapCount:]...)
		}
		sd.LearningGaps = gaps
		return nil
	})
	_ = c.persistSession("persist repaired learning gaps")
}

// emitEvent logs a RunEvent to the coordinator's eventStore if initialized.
func (c *Coordinator) emitEvent(eventType, actor, taskID string, payload interface{}) error {
	if c == nil || c.eventStore == nil {
		return nil
	}
	var rawPayload json.RawMessage
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}
	// Pass the marshaled payload through directly; AppendPersisted performs
	// proper JSON-aware redaction (utils.RedactJSON) before persisting. Text
	// redaction here would both duplicate that pass and miss escaped secrets.
	rawPayload = json.RawMessage(data)
	if IsTerminalEvent(eventType) && IsEmptyPayload(rawPayload) {
		err := fmt.Errorf("terminal event %q produced empty payload after marshal", eventType)
		log.Printf("error: dual-write event emit failed for type %s: %v", eventType, err)
		c.dualWriteFailures.Add(1)
		return err
	}
	event := RunEvent{
		Type:    eventType,
		Actor:   actor,
		TaskID:  taskID,
		Payload: rawPayload,
	}
	if _, err := c.eventStore.AppendPersisted(event); err != nil {
		log.Printf("warning: dual-write event emit failed for type %s: %v", eventType, err)
		c.recordLearningGap(event, err)
		return err
	}
	return nil
}

func (c *Coordinator) emitTaskEventsFromCheckpoint(tasks []*TodoItem) error {
	if c == nil || c.eventStore == nil {
		return nil
	}
	c.eventOnceMu.Lock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	c.eventOnceMu.Unlock()

	for _, item := range tasks {
		if item == nil {
			continue
		}
		var eventType string
		switch item.Status {
		case TaskPending:
			eventType = "task_created"
		case TaskPlanned:
			eventType = "task_planned"
		case TaskInProgress:
			eventType = "task_started"
		case TaskVerifying:
			eventType = "task_verifying"
		case TaskPaused:
			eventType = "task_paused"
		case TaskDone:
			eventType = "task_completed"
		case TaskError:
			eventType = "task_failed"
		case TaskSkipped:
			eventType = "task_skipped"
		case TaskBlocked:
			eventType = "task_blocked"
		case TaskProtocolIncomplete:
			eventType = "task_protocol_incomplete"
		}
		if eventType == "" {
			continue
		}

		transitionKey := taskTransitionEventKey(item)
		c.eventOnceMu.Lock()
		alreadyEmitted := c.emittedTaskTransitions[transitionKey]
		c.eventOnceMu.Unlock()

		if alreadyEmitted {
			// The task transition may already be durable while its artifact
			// side-event was lost. Revisit artifact emission on every checkpoint
			// so a transient append failure is retryable.
			c.emitArtifactEvents(item)
			if isMemoryOutcomeTerminalEvent(eventType) {
				c.recordMemoryOutcomeForTask(item, eventType)
			}
			continue
		}

		payload := c.taskTransitionPayloadWithCoordinator(item)
		data, err := json.Marshal(payload)
		if err != nil {
			c.dualWriteFailures.Add(1)
			return fmt.Errorf("marshal canonical task event for %s (%s): %w", item.ID, eventType, err)
		}
		rawPayload := json.RawMessage(data)
		if IsTerminalEvent(eventType) && IsEmptyPayload(rawPayload) {
			c.dualWriteFailures.Add(1)
			return fmt.Errorf("canonical task event for %s (%s) has an empty payload", item.ID, eventType)
		}

		if _, err := c.emitEventOnce(transitionKey, RunEvent{
			Type:    eventType,
			Actor:   item.Agent,
			TaskID:  item.ID,
			Payload: rawPayload,
		}); err != nil {
			return fmt.Errorf("append canonical task event for %s (%s): %w", item.ID, eventType, err)
		}

		c.emitArtifactEvents(item)
		if isMemoryOutcomeTerminalEvent(eventType) {
			c.recordMemoryOutcomeForTask(item, eventType)
		}
	}
	return nil
}

// CommitTaskTransition is the event-first transition boundary for task
// lifecycle states that have an EventStore representation. It appends the
// complete, redacted projection payload before changing TodoList; a crash
// after append and before checkpoint is therefore recoverable by replay.
//
// Verification, receipts, and typed results should be installed on TodoList
// before a terminal call so their values are carried by the same transition.
func (c *Coordinator) CommitTaskTransition(ctx context.Context, taskID string, expected, next TaskStatus, detail, output string, metadata map[string]interface{}) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fmt.Errorf("commit task transition: task tracker is unavailable")
	}
	current := todoItemByID(c.taskTracker.TodoList().Items(), taskID)
	if current == nil {
		return fmt.Errorf("commit task transition: task %s not found", taskID)
	}
	if current.Status != expected {
		return fmt.Errorf("commit task transition: task %s expected %s, got %s", taskID, expected, current.Status)
	}
	eventType := eventTypeForTaskStatus(next)
	if eventType == "" || !c.hasDurableEventJournal() {
		return c.taskTracker.TodoList().TryUpdateStatusAndOutput(taskID, next, detail, output)
	}

	projected := *current
	projected.Status = next
	if detail != "" {
		projected.Detail = detail
	}
	if output != "" {
		projected.Output = output
	}
	if fe, ok := metadata["failure_event"].(*FailureEventPayload); ok && fe != nil {
		projected.FailureEvent = fe
	}
	if fo, ok := metadata["failure_output"].(string); ok && fo != "" {
		projected.Output = fo
	}
	payload := c.taskTransitionPayloadWithCoordinator(&projected)
	for key, value := range metadata {
		payload[key] = value
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("commit task transition payload: %w", err)
	}
	key := taskTransitionEventKey(&projected)
	// A terminal/cancellation transition is itself recovery evidence. Do not
	// let the already-cancelled worker context suppress its durable record.
	appendCtx := context.WithoutCancel(ctx)
	if _, err := c.EventJournal().Append(appendCtx, RunEvent{
		Type:           eventType,
		Actor:          projected.Agent,
		TaskID:         projected.ID,
		IdempotencyKey: key,
		Payload:        rawPayload,
	}); err != nil {
		return fmt.Errorf("commit task transition append: %w", err)
	}
	c.eventOnceMu.Lock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	c.emittedTaskTransitions[key] = true
	c.emittedTaskTransitions[taskTransitionEventKey(&projected)] = true
	c.emittedTaskTransitions[fmt.Sprintf("%s:%s:%d", projected.ID, projected.Status, projected.Retries)] = true
	c.eventOnceMu.Unlock()
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(taskID, next, detail, output); err != nil {
		return fmt.Errorf("apply task transition after durable append: %w", err)
	}
	if fe, ok := metadata["failure_event"].(*FailureEventPayload); ok && fe != nil {
		_ = c.taskTracker.TodoList().SetFailureEventAndOutput(taskID, fe, output)
	}
	return nil
}

// CommitTaskResetForRetry is the event-first transition boundary for resetting
// a task back to TaskPending for DAG or crash-recovery retries. It appends the
// complete, reset projection payload (with incremented retries and cleared
// output/timing/runtime errors) before mutating TodoList; if the append fails,
// the task status, retry count, and checkpoint remain untouched.
func (c *Coordinator) CommitTaskResetForRetry(ctx context.Context, taskID string, detail string) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fmt.Errorf("commit task reset for retry: task tracker is unavailable")
	}
	current := todoItemByID(c.taskTracker.TodoList().Items(), taskID)
	if current == nil {
		return fmt.Errorf("commit task reset for retry: task %s not found", taskID)
	}
	if !c.hasDurableEventJournal() {
		c.taskTracker.TodoList().ResetForRetry(taskID, detail)
		return nil
	}

	projected := *current
	projected.Status = TaskPending
	projected.Detail = detail
	projected.Output = ""
	projected.VerifyResult = nil
	projected.RuntimeError = nil
	projected.FailureEvent = nil
	projected.RecoveryState = RecoveryStateNotStarted
	projected.LastOperation = ""
	projected.Progress = ProgressUnknown
	projected.ProgressCriteria = nil
	projected.StartedAt = time.Time{}
	projected.EndedAt = time.Time{}
	projected.ModelTime = 0
	projected.ToolTime = 0
	projected.Retries = current.Retries + 1

	payload := c.taskTransitionPayloadWithCoordinator(&projected)
	payload["reset_for_retry"] = true
	payload["previous_status"] = string(current.Status)

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("commit task reset for retry payload: %w", err)
	}
	key := taskTransitionEventKey(&projected)
	appendCtx := context.WithoutCancel(ctx)
	if _, err := c.EventJournal().Append(appendCtx, RunEvent{
		Type:           string(EventTaskCreated),
		Actor:          projected.Agent,
		TaskID:         projected.ID,
		IdempotencyKey: key,
		Payload:        rawPayload,
	}); err != nil {
		return fmt.Errorf("commit task reset for retry append: %w", err)
	}
	c.eventOnceMu.Lock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	c.emittedTaskTransitions[key] = true
	c.emittedTaskTransitions[taskTransitionEventKey(&projected)] = true
	c.emittedTaskTransitions[fmt.Sprintf("%s:%s:%d", projected.ID, projected.Status, projected.Retries)] = true
	c.eventOnceMu.Unlock()

	c.taskTracker.TodoList().ResetForRetry(taskID, detail)
	return nil
}

// CommitTaskCreation is the event-first creation boundary for new tasks. It
// reserves IDs and delegates to CommitTaskCreationResolved, which appends
// complete task_created payloads and makes each successfully appended task
// visible in the projection before continuing. A crash after append and before
// checkpoint is recoverable by replay; an append failure returns the partial
// result (the tasks whose events are already durable and visible) together
// with the error, so no durable event is ever left without a projection entry.
func (c *Coordinator) CommitTaskCreation(ctx context.Context, specs []TodoSpec) ([]*TodoItem, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil, fmt.Errorf("commit task creation: task tracker is unavailable")
	}
	if len(specs) == 0 {
		return nil, nil
	}
	ids := c.taskTracker.TodoList().ReserveIDs(len(specs))
	return c.CommitTaskCreationResolved(ctx, specs, ids)
}

// CommitTaskCreationResolved is the event-first creation boundary for a batch
// whose IDs were already reserved by the caller. Reserving IDs first lets the
// caller resolve index-based DAG/recovery edges (DependsOn, OnFailure) to real
// todo IDs before the durable task_created payload is serialized, so the
// initial event carries the complete execution contract (PR-05).
//
// Each item is appended to the journal and then immediately added to TodoList
// (firing the checkpoint callback). A failure on the Nth append therefore
// leaves the first N-1 tasks durable AND visible: the returned slice is the
// explicit partial result and the error reports the failed append. Callers
// that cannot proceed with a partial batch must treat the returned items as
// already-committed work, never as orphans to be recreated.
func (c *Coordinator) CommitTaskCreationResolved(ctx context.Context, specs []TodoSpec, ids []string) ([]*TodoItem, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil, fmt.Errorf("commit task creation: task tracker is unavailable")
	}
	if len(specs) == 0 {
		return nil, nil
	}
	if len(ids) != len(specs) {
		return nil, fmt.Errorf("commit task creation: reserved %d IDs for %d specs", len(ids), len(specs))
	}
	tl := c.taskTracker.TodoList()
	items := make([]*TodoItem, len(specs))
	for i, spec := range specs {
		items[i] = todoItemFromSpec(spec, ids[i])
	}
	if !c.hasDurableEventJournal() {
		tl.AddReserved(items)
		return items, nil
	}

	appendCtx := context.WithoutCancel(ctx)
	var created []*TodoItem
	for _, item := range items {
		payload := c.taskTransitionPayloadWithCoordinator(item)
		rawPayload, err := json.Marshal(payload)
		if err != nil {
			// created items were already added incrementally below; they are
			// durable AND visible, so return them as the explicit partial result.
			return created, fmt.Errorf("commit task creation payload: %w", err)
		}
		key := taskTransitionEventKey(item)
		if _, err := c.EventJournal().Append(appendCtx, RunEvent{
			Type:           string(EventTaskCreated),
			Actor:          item.Agent,
			TaskID:         item.ID,
			IdempotencyKey: key,
			Payload:        rawPayload,
		}); err != nil {
			// created items were already added incrementally below; they are
			// durable AND visible, so return them as the explicit partial result.
			return created, fmt.Errorf("commit task creation append: %w", err)
		}
		c.eventOnceMu.Lock()
		if c.emittedTaskTransitions == nil {
			c.emittedTaskTransitions = make(map[string]bool)
		}
		c.emittedTaskTransitions[key] = true
		c.emittedTaskTransitions[taskTransitionEventKey(item)] = true
		c.emittedTaskTransitions[fmt.Sprintf("%s:%s:%d", item.ID, item.Status, item.Retries)] = true
		c.eventOnceMu.Unlock()
		created = append(created, item)
		// Make the item visible immediately so a partial append failure (or a
		// crash mid-loop) never leaves a durable event without a projection
		// entry and checkpoint.
		tl.AddReserved([]*TodoItem{item})
	}
	return created, nil
}

// CommitTaskRemoval is the event-first boundary for removing tasks from the
// projection (for example suppressed duplicate delegation). It appends a
// task_removed event per live task before DeleteIDs mutates TodoList, so a
// crash between append and checkpoint cannot resurrect a suppressed task on
// replay.
func (c *Coordinator) CommitTaskRemoval(ctx context.Context, taskIDs ...string) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fmt.Errorf("commit task removal: task tracker is unavailable")
	}
	if len(taskIDs) == 0 {
		return nil
	}
	if !c.hasDurableEventJournal() {
		c.taskTracker.TodoList().DeleteIDs(taskIDs...)
		return nil
	}

	appendCtx := context.WithoutCancel(ctx)
	var appended []string
	for _, id := range taskIDs {
		item := todoItemByID(c.taskTracker.TodoList().Items(), id)
		if item == nil {
			continue
		}
		payload := map[string]interface{}{
			"id":     item.ID,
			"status": string(item.Status),
			"desc":   item.Desc,
			"agent":  item.Agent,
		}
		rawPayload, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("commit task removal payload: %w", err)
		}
		key := "removed:" + taskTransitionEventKey(item)
		if _, err := c.EventJournal().Append(appendCtx, RunEvent{
			Type:           string(EventTaskRemoved),
			Actor:          item.Agent,
			TaskID:         item.ID,
			IdempotencyKey: key,
			Payload:        rawPayload,
		}); err != nil {
			// Keep the projection aligned with the durable events: remove the
			// items whose removal was already appended before surfacing the
			// error, so replay and checkpoint cannot disagree.
			if len(appended) > 0 {
				c.taskTracker.TodoList().DeleteIDs(appended...)
			}
			return fmt.Errorf("commit task removal append: %w", err)
		}
		appended = append(appended, id)
	}
	c.taskTracker.TodoList().DeleteIDs(taskIDs...)
	return nil
}

// CommitTaskResolution is the event-first boundary for reconcile_task. It
// appends a task_resolution event carrying the full projected payload before
// SetTaskResolution mutates TodoList, so the resolution and its evidence
// survive restart and branch replay. The tool request is rejected when the
// append fails.
func (c *Coordinator) CommitTaskResolution(ctx context.Context, taskID string, resolution *TaskResolution) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fmt.Errorf("commit task resolution: task tracker is unavailable")
	}
	current := todoItemByID(c.taskTracker.TodoList().Items(), taskID)
	if current == nil {
		return fmt.Errorf("commit task resolution: task %s not found", taskID)
	}
	// Validate before appending so an invalid resolution never becomes durable
	// while the projection rejects it.
	if resolution != nil {
		if err := ValidateResolution(resolution, taskID, c.taskTracker.TodoList().Items(), c.taskTracker.TodoList().RunID()); err != nil {
			return err
		}
	}
	if !c.hasDurableEventJournal() {
		return c.taskTracker.TodoList().SetTaskResolution(taskID, resolution)
	}

	projected := *current
	projected.Resolution = resolution
	payload := c.taskTransitionPayloadWithCoordinator(&projected)
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("commit task resolution payload: %w", err)
	}
	key := "resolution:" + taskTransitionEventKey(current)
	appendCtx := context.WithoutCancel(ctx)
	if _, err := c.EventJournal().Append(appendCtx, RunEvent{
		Type:           string(EventTaskResolution),
		Actor:          current.Agent,
		TaskID:         current.ID,
		IdempotencyKey: key,
		Payload:        rawPayload,
	}); err != nil {
		return fmt.Errorf("commit task resolution append: %w", err)
	}
	return c.taskTracker.TodoList().SetTaskResolution(taskID, resolution)
}

func (c *Coordinator) hasDurableEventJournal() bool {
	if c == nil {
		return false
	}
	if c.eventStore != nil {
		return true
	}
	switch journal := c.eventJournal.(type) {
	case nil:
		return false
	case eventStoreJournal:
		return journal.store != nil
	case *eventStoreJournal:
		return journal != nil && journal.store != nil
	default:
		return true
	}
}

func (c *Coordinator) commitTaskTransitionFromCurrent(ctx context.Context, taskID string, next TaskStatus, detail, output string, metadata map[string]interface{}) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return fmt.Errorf("commit task transition: task tracker is unavailable")
	}
	item := todoItemByID(c.taskTracker.TodoList().Items(), taskID)
	if item == nil {
		return fmt.Errorf("commit task transition: task %s not found", taskID)
	}
	return c.CommitTaskTransition(ctx, taskID, item.Status, next, detail, output, metadata)
}

func todoItemByID(items []*TodoItem, id string) *TodoItem {
	for _, item := range items {
		if item != nil && item.ID == id {
			return item
		}
	}
	return nil
}

func eventTypeForTaskStatus(status TaskStatus) string {
	switch status {
	case TaskPending:
		return string(EventTaskCreated)
	case TaskPlanned:
		return string(EventTaskPlanned)
	case TaskInProgress:
		return string(EventTaskStarted)
	case TaskVerifying:
		return string(EventTaskVerifying)
	case TaskPaused:
		return string(EventTaskPaused)
	case TaskDone:
		return string(EventTaskCompleted)
	case TaskError:
		return string(EventTaskFailed)
	case TaskSkipped:
		return string(EventTaskSkipped)
	case TaskBlocked:
		return string(EventTaskBlocked)
	case TaskProtocolIncomplete:
		return string(EventTaskProtocolIncomplete)
	default:
		return ""
	}
}

func taskTransitionPayload(item *TodoItem) map[string]interface{} {
	return taskTransitionPayloadWithCoordinator(item, nil)
}

func (c *Coordinator) taskTransitionPayloadWithCoordinator(item *TodoItem) map[string]interface{} {
	return taskTransitionPayloadWithCoordinator(item, c)
}

func taskTransitionPayloadWithCoordinator(item *TodoItem, c *Coordinator) map[string]interface{} {
	if item == nil {
		return nil
	}
	payload := map[string]interface{}{
		"id":                    item.ID,
		"phase":                 item.Phase,
		"action":                item.Action,
		"plan_task_id":          item.PlanTaskID,
		"contract_id":           item.ContractID,
		"contract_hash":         item.ContractHash,
		"contract_revision":     item.ContractRevision,
		"status":                string(item.Status),
		"detail":                item.Detail,
		"max_retries":           item.MaxRetries,
		"retries":               item.Retries,
		"agent":                 item.Agent,
		"model":                 item.Model,
		"skills":                item.Skills,
		"injected_skills":       item.InjectedSkills,
		"loaded_skills":         item.LoadedSkills,
		"source":                item.Source,
		"parent_id":             item.ParentID,
		"depends_on":            item.DependsOn,
		"on_failure":            item.OnFailure,
		"kind":                  item.Kind,
		"advances":              item.Advances,
		"expected_state_change": item.ExpectedStateChange,
		"progress":              item.Progress,
		"progress_criteria":     item.ProgressCriteria,
		"failure_fingerprints":  item.FailureFingerprints,
		"execution":             item.Execution,
		"recovery_hypothesis":   item.RecoveryHypothesis,
		"side_effect":           item.SideEffect,
		"recovery":              item.Recovery,
		"reconcile_tool":        item.ReconcileTool,
		"recovery_state":        item.RecoveryState,
		"runtime_error":         item.RuntimeError,
		"resolution":            item.Resolution,
		"diagnostic_hints":      item.DiagnosticHints,
		"last_operation":        item.LastOperation,
		"attempt":               item.Retries + 1,
	}
	failureTransition := item.Status == TaskError || item.Status == TaskBlocked || item.Status == TaskProtocolIncomplete
	if !failureTransition {
		payload["desc"] = item.Desc
		payload["summary"] = item.Detail
		payload["output"] = item.Output
	} else {
		failureClass := classifyTaskFailure(errors.New(item.Detail))
		disposition := RetryNone
		if item.Status == TaskProtocolIncomplete {
			failureClass = FailureProtocol
			disposition = ReconcileOnly
		}
		var failure *FailureEventPayload
		if item.FailureEvent != nil {
			failure = cloneFailureEventPayload(item.FailureEvent)
		} else if c != nil {
			failure = c.failureEventForItem(item, failureClass, disposition, item.Detail, FailureFingerprint{}, item.ID)
		} else {
			var coord *Coordinator
			failure = coord.failureEventForItem(item, failureClass, disposition, item.Detail, FailureFingerprint{}, item.ID)
		}
		for key, value := range failureEventPayloadMap(failure) {
			payload[key] = value
		}
		payload["summary"] = failureSummary(item)
		payload["output"] = utils.TruncateString(utils.RedactSecrets(item.Output), 2000)
	}
	if item.Verify != "" {
		payload["verify"] = item.Verify
		payload["verify_mode"] = item.VerifyMode
	}
	if item.VerifySpec != nil {
		payload["verify_spec"] = item.VerifySpec
	}
	if item.WorksetBinding != nil {
		payload["workset_binding"] = item.WorksetBinding
	}
	if item.WorksetReceipt != nil {
		payload["workset_receipt"] = item.WorksetReceipt
	}
	if item.VerifyResult != nil {
		payload["verify_result"] = item.VerifyResult
	}
	if item.TypedResult != nil {
		payload["typed_result"] = item.TypedResult
	}
	if item.ExecutionReceipt != nil {
		payload["execution_receipt"] = item.ExecutionReceipt
	}
	if len(item.ExecutionReceipts) > 0 {
		payload["execution_receipts"] = item.ExecutionReceipts
	}
	if len(item.MemoryManifests) > 0 {
		payload["memory_manifests"] = item.MemoryManifests
	}
	if len(item.ContextManifests) > 0 {
		payload["context_manifests"] = item.ContextManifests
	}
	return payload
}

func isMemoryOutcomeTerminalEvent(eventType string) bool {
	switch eventType {
	case "task_completed", "task_failed", "task_blocked", "task_protocol_incomplete":
		return true
	default:
		return false
	}
}

// taskTransitionEventKey identifies the exact canonical task shadow already
// emitted. Keeping only ID/status/retry suppresses receipts, typed results,
// and other same-status projection updates; a later checkpoint would then be
// ahead of replay after a crash. The digest makes every replay-relevant task
// mutation a new durable projection event while preserving idempotency for an
// unchanged shadow.
func taskTransitionEventKey(item *TodoItem) string {
	if item == nil {
		return ""
	}
	base := fmt.Sprintf("%s:%s:%d", item.ID, item.Status, item.Retries)
	data, err := json.Marshal(toCanonicalTaskShadow(item))
	if err != nil {
		// The payload marshal on the durable path will fail closed immediately
		// afterwards. Keep this deterministic fallback only so the caller can
		// surface that original marshal error.
		return base + ":canonical-unserializable"
	}
	redacted, err := utils.RedactJSON(data)
	if err != nil {
		return base + ":canonical-unserializable"
	}
	sum := sha256.Sum256(redacted)
	return base + ":canonical-" + hex.EncodeToString(sum[:16])
}

// emitArtifactEvents dual-writes one artifact_created event per artifact path
// declared by a completed task's typed result. Emission is idempotent per
// (task, path) within this coordinator instance.
func (c *Coordinator) emitArtifactEvents(item *TodoItem) {
	if c.eventStore == nil || item == nil || item.TypedResult == nil {
		return
	}
	for _, art := range item.TypedResult.Artifacts {
		if art.Path == "" {
			continue
		}
		key := fmt.Sprintf("artifact:%s:%s", item.ID, art.Path)
		c.eventOnceMu.Lock()
		alreadyEmitted := c.emittedTaskTransitions[key]
		c.eventOnceMu.Unlock()
		if alreadyEmitted {
			continue
		}

		payload, _ := json.Marshal(map[string]interface{}{
			"artifact":    art,
			"path":        art.Path,
			"description": art.Description,
			"task_id":     item.ID,
		})
		if _, err := c.emitEventOnce(key, RunEvent{
			Type:    "artifact_created",
			Actor:   item.Agent,
			TaskID:  item.ID,
			Payload: payload,
		}); err != nil {
			log.Printf("warning: dual-write artifact event emit failed for %s (%s): %v", item.ID, art.Path, err)
			continue
		}
	}
}

// EventStore returns the active EventStore for the coordinator (may be nil).
func (c *Coordinator) EventStore() *EventStore {
	if c == nil {
		return nil
	}
	return c.eventStore
}

// DualWriteFailures returns the count of failed dual-write event store appends.
func (c *Coordinator) DualWriteFailures() int64 {
	if c == nil {
		return 0
	}
	return c.dualWriteFailures.Load()
}
