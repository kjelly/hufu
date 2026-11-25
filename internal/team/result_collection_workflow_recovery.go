package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Failed workflows remain closed to delegation. An explicit collection repair
// settles only the explicitly selected protocol task. Match typed
// failure provenance rather than message text, which can change during repair.
func resultRepairWorkflowPhase(session *SessionData, taskID string) (Phase, error) {
	return resultRepairWorkflowPhaseWithDeferrals(session, taskID, true)
}

func resultRepairWorkflowPhaseWithDeferrals(session *SessionData, taskID string, allowDeferred bool) (Phase, error) {
	if session == nil || session.WorkflowState != PhaseFailed {
		return blockedCollectionWorkflowPhase(session, taskID, validateCollectionRecoveryTarget)
	}
	item := todoItemByID(session.Tasks, taskID)
	if err := validateCollectionRecoveryTarget(item); err != nil {
		return "", err
	}
	branchID := currentResultCollection(item).BranchID
	var failed []*TodoItem
	for _, candidate := range session.Tasks {
		if candidate == nil {
			continue
		}
		if isUnresolvedTaskStatus(candidate.Status) {
			if allowDeferred && collectionDispatchDeferred(candidate, item) {
				continue
			}
			if candidate.Phase != item.Phase || validateCollectionRecoveryTarget(candidate) != nil {
				return "", fmt.Errorf("failed workflow contains an unrelated unresolved task")
			}
			failed = append(failed, candidate)
		} else if candidate.Phase == item.Phase && hasCommittedWorkflowCollectionRepair(candidate, branchID) {
			failed = append(failed, candidate)
		}
	}
	if !collectionFailuresMatchTasks(session.PhaseResults[item.Phase], item.Phase, failed) {
		return "", fmt.Errorf("workflow failure is not attributable to its collection tasks")
	}
	return item.Phase, nil
}

func hasCommittedWorkflowCollectionRepair(item *TodoItem, branchID string) bool {
	if item == nil || item.Status != TaskDone {
		return false
	}
	cp := currentResultCollection(item)
	// Retention can carry accepted results across branches. A collection
	// repair grant remains authority only on the branch where it was issued.
	if cp == nil || cp.BranchID != branchID || cp.State != ResultCollectionCommitted {
		return false
	}
	return slices.ContainsFunc(collectionRepairGrants(item.ExecutionReceipt), func(grant ResultRepairGrant) bool {
		return isWorkflowCollectionRepairGrant(grant, cp)
	})
}

// resultRepairLocalCommitOnly permits settling a prepared producer when its
// downstream verifier failed or was blocked before the source was accepted.
// This grants authority only to commit the saved result.
func resultRepairLocalCommitOnly(session *SessionData, taskID string, allowStranded bool) (bool, error) {
	_, workflowErr := resultRepairWorkflowPhase(session, taskID)
	if workflowErr == nil || session == nil || session.WorkflowState != PhaseFailed {
		return false, workflowErr
	}
	item := todoItemByID(session.Tasks, taskID)
	if validateCollectionRecoveryTarget(item) != nil || !metadataReadOnlyTask(item.SideEffect, item.Execution) {
		return false, workflowErr
	}
	cp := currentResultCollection(item)
	if cp.State != ResultCollectionPrepared || item.ExecutionReceipt.RepairProvenance == nil || item.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureCommit {
		return false, workflowErr
	}
	result := session.PhaseResults[item.Phase]
	status := result.Status
	if status != PhaseStatusCancelled && status != PhaseStatusBlocked {
		return false, workflowErr
	}
	strandedPhase := allowStranded && collectionFailuresMatchTasks(result, item.Phase, []*TodoItem{item})
	failedConsumer := false
	for _, peer := range session.Tasks {
		if peer == nil || peer.ID == taskID || !isUnresolvedTaskStatus(peer.Status) {
			continue
		}
		if !readOnlyCollectionVerifier(peer, item) {
			return false, workflowErr
		}
		failed := peer.Status == TaskError && peer.FailureEvent != nil && peer.FailureEvent.FailureClass == FailureVerify
		if !failed && (!strandedPhase || !strandedCollectionVerifier(peer, item)) {
			return false, workflowErr
		}
		failedConsumer = true
	}
	if !failedConsumer {
		return false, workflowErr
	}
	return true, nil
}

func readOnlyCollectionVerifier(verifier, producer *TodoItem) bool {
	return verifier != nil && producer != nil && verifier.Phase == producer.Phase && verifier.Action != nil &&
		metadataReadOnlyTask(verifier.SideEffect, verifier.Execution) && slices.Contains(verifier.DependsOn, producer.ID)
}

// Historical scheduler blocks have no typed dependency cause. Match the
// fingerprint of the runtime-generated detail against the frozen dependency
// and producer failure, and require that the verifier never dispatched.
func strandedCollectionVerifier(verifier, producer *TodoItem) bool {
	failure := verifier.FailureEvent
	if verifier.Status != TaskBlocked || !collectionUndispatched(verifier) || verifier.CheckpointPause ||
		failure == nil || failure.TaskID != verifier.ID || producer.FailureEvent == nil || len(verifier.FailureFingerprints) != 1 {
		return false
	}
	fingerprint := verifier.FailureFingerprints[0]
	if fingerprint.Digest == "" || fingerprint.Digest != failure.Fingerprint || fingerprint.Occurrences != 1 {
		return false
	}
	dep := strandedDependency{taskID: producer.ID, status: producer.Status, verify: producer.VerifyResult, fail: producer.FailureEvent}
	full, class := formatStrandedDependencies([]strandedDependency{dep})
	if matchesStrandedFailureFingerprint(verifier, fingerprint, full, class) {
		return true
	}
	// Resuming an interrupted task records the producer identity and status only.
	header := strandedDependencyPrefix + strandedDependencyHeader(producer.ID, producer.Status)
	return matchesStrandedFailureFingerprint(verifier, fingerprint, header, producer.FailureEvent.FailureClass)
}

func matchesStrandedFailureFingerprint(verifier *TodoItem, actual FailureFingerprint, detail string, class TaskFailureClass) bool {
	expected := NewFailureFingerprint(actual.CriterionID, verifier.Agent, stableOperation(verifier), class, failureDetailWithoutCoordinatorFields(detail))
	return verifier.FailureEvent.FailureClass == class && SameFailureFingerprint(actual, expected)
}

func isWorkflowCollectionRepairGrant(grant ResultRepairGrant, cp *ResultCollectionCheckpoint) bool {
	// Reference selection repairs accepted work. It grants no authority to
	// reopen a later phase failure, including on a descendant branch.
	return grant.Recovery != nil && grant.Recovery.ValidatorRevision >= 4 &&
		grant.Recovery.Disposition != RecoverySelectReferences && validateStoredRecoveryGrant(grant, cp) == nil
}

// Match as a multiset: observe may record only the first blocked task, and
// parallel tasks may have the same agent and category. Every recorded failure
// must match a distinct task; additional tasks still need independent repair.
func collectionFailuresMatchTasks(result PhaseResult, phase Phase, items []*TodoItem) bool {
	if result.Status != PhaseStatusBlocked || len(result.Errors) == 0 || len(result.Errors) > len(items) {
		return false
	}
	remaining := slices.Clone(items)
	for _, failure := range result.Errors {
		i := slices.IndexFunc(remaining, func(item *TodoItem) bool {
			return failure.Category == blockedTaskFailureCategory(item.Optional) && failure.Phase == phase && failure.Component == item.Agent && failure.Source == item.Agent
		})
		if i < 0 {
			return false
		}
		remaining = slices.Delete(remaining, i, i+1)
	}
	return true
}

func blockedCollectionWorkflowPhase(session *SessionData, taskID string, validateTarget func(*TodoItem) error) (Phase, error) {
	if session == nil || session.WorkflowState == "" {
		return "", nil
	}
	if session.WorkflowState == PhaseDone {
		return "", fmt.Errorf("result-only repair cannot reopen a completed workflow")
	}
	if session.WorkflowState != PhaseFailed {
		return "", nil
	}
	item := todoItemByID(session.Tasks, taskID)
	unresolved := UnresolvedTaskReferences(session.Tasks)
	if item == nil || len(unresolved) != 1 || unresolved[0].ID != taskID || validateTarget(item) != nil {
		return "", fmt.Errorf("failed workflow recovery requires its sole blocked collection task")
	}
	result, exists := session.PhaseResults[item.Phase]
	if !exists || result.Status != PhaseStatusBlocked || len(result.Errors) != 1 {
		return "", fmt.Errorf("workflow failure is not attributable to the selected collection")
	}
	failure := result.Errors[0]
	if failure.Category != blockedTaskFailureCategory(item.Optional) || failure.Phase != item.Phase || failure.Component != item.Agent || failure.Source != item.Agent {
		return "", fmt.Errorf("workflow failure does not match the selected task's canonical failure")
	}
	return item.Phase, nil
}

func blockedTaskFailureCategory(optional bool) string {
	if optional {
		return "OPTIONAL_TASK_BLOCKED"
	}
	return "TASK_BLOCKED"
}

func (c *Coordinator) collectionWorkflowSnapshot() *SessionData {
	state := &SessionData{Tasks: c.taskTracker.TodoList().Items()}
	c.viewSessionData(func(sd *SessionData) {
		state.WorkflowState, state.PhaseResults = sd.WorkflowState, sd.PhaseResults
	})
	if w := c.phaseWorkflow; w != nil && w.Enabled() && w.State() != PhaseInit {
		state.WorkflowState, state.PhaseResults, _, _ = w.snapshot()
	}
	return state
}

// This is admission to result collection, not authority to run worker tools or
// reopen the phase. The existing operator grant still owns every repair turn.
func (c *Coordinator) admitFailedWorkflowResultRepair(ctx context.Context) error {
	id, _ := ctx.Value(resultOnlyRepairKey{}).(string)
	if id == "" {
		return fmt.Errorf("workflow is failed; use an eligible explicit result-only repair or targeted reconciliation")
	}
	_, err := resultRepairLocalCommitOnly(c.collectionWorkflowSnapshot(), id, true)
	return err
}

// Derive the reopening proof from the grant's original canonical journal head.
// No schema, receipt, historical result or failed phase is rewritten. This also
// handles a crash after task completion but before workflow reconciliation.
func (c *Coordinator) reconcileCollectedWorkflow(ctx context.Context) error {
	w := c.phaseWorkflow
	if w == nil || !w.Enabled() || w.State() != PhaseFailed {
		return nil
	}
	proof, err := c.completedCollectionWorkflowProof(ctx)
	if err != nil || proof == nil {
		return err
	}
	for _, id := range proof.deferred {
		item := c.todoItemByID(id)
		if collectionDispatchRequeued(item) {
			continue
		}
		metadata := map[string]any{"reset_for_resume": true, "release_deferred_dispatch": true}
		if err := c.CommitTaskTransition(ctx, id, TaskBlocked, TaskPending, collectionDispatchResumeDetail, "", metadata); err != nil {
			return err
		}
	}
	c.rebuildAntiThrashingState()
	if !w.reconcileCollectedFailure(proof.taskID, proof.phase, proof.results) {
		return errResultCollectionStale
	}
	c.saveCheckpoint()
	return nil
}

type collectionWorkflowProof struct {
	taskID   string
	phase    Phase
	results  map[Phase]PhaseResult
	deferred []string
}

func (c *Coordinator) completedCollectionWorkflowProof(ctx context.Context) (*collectionWorkflowProof, error) {
	current := c.collectionWorkflowSnapshot()
	if current.WorkflowState != PhaseFailed {
		return nil, nil
	}
	// An unresolved task that ran still needs its own repair. A deferral
	// may be cleared only by the later journal proof of the original failure.
	for _, item := range current.Tasks {
		if item != nil && isUnresolvedTaskStatus(item.Status) && !collectionDispatchDeferred(item, item) && !collectionDispatchRequeued(item) {
			return nil, nil
		}
	}
	items := current.Tasks
	branchID := c.activeBranchID()
	if !slices.ContainsFunc(items, func(item *TodoItem) bool {
		return hasCommittedWorkflowCollectionRepair(item, branchID)
	}) {
		return nil, nil
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return nil, err
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	replayed, err := ReplayTodoList(events)
	if err != nil {
		return nil, err
	}
	failureHeads, err := collectionFailureJournalHeads(events, branchID)
	if err != nil {
		return nil, err
	}
	var proof *collectionWorkflowProof
	var completed []*TodoItem
	required := make(map[string]bool)
	deferred := make(map[string]bool)
	for _, item := range items {
		if !hasCommittedWorkflowCollectionRepair(item, branchID) {
			continue
		}
		cp := currentResultCollection(item)
		for _, grant := range collectionRepairGrants(item.ExecutionReceipt) {
			if !isWorkflowCollectionRepairGrant(grant, cp) {
				continue
			}
			if err := c.validateWorkflowCollectionCompletion(ctx, item, todoItemByID(replayed, item.ID), cp); err != nil {
				return nil, err
			}
			i := slices.IndexFunc(events, func(event RunEvent) bool { return event.ID == grant.Recovery.JournalHeadID })
			if i < 0 {
				return nil, errResultCollectionStale
			}
			if !failureHeads[grant.Recovery.JournalHeadID] {
				continue
			}
			source := ReduceToSessionData(events[:i+1])
			var phase Phase
			if grant.Recovery.ValidatorRevision < 5 {
				phase, err = blockedCollectionWorkflowPhase(source, item.ID, validateCollectionRecoveryTarget)
			} else {
				phase, err = resultRepairWorkflowPhaseWithDeferrals(source, item.ID, grant.Recovery.ValidatorRevision >= 15)
			}
			if err != nil || phase == "" {
				continue
			}
			// Earlier recovered phases keep their accepted work and grants.
			// Their old blocked snapshot cannot reopen a later failed phase.
			if current.PhaseResults[phase].Status == PhaseStatusSuccess {
				continue
			}
			if !sameCanonicalEvidence(current.PhaseResults, source.PhaseResults) {
				return nil, fmt.Errorf("workflow changed after the authorized collection repair: %w", errResultCollectionStale)
			}
			if err := c.validateAcceptedDependencyPayloadWithContext(ctx, item); err != nil {
				return nil, err
			}
			for _, peer := range source.Tasks {
				if peer == nil || peer.Phase != phase {
					continue
				}
				if grant.Recovery.ValidatorRevision >= 15 && collectionDispatchDeferred(peer, item) {
					deferred[peer.ID] = true
					continue
				}
				if isUnresolvedTaskStatus(peer.Status) || collectionRepairBelongsToFailure(peer, branchID, failureHeads) {
					required[peer.ID] = true
				}
			}
			if proof != nil && proof.phase != phase {
				return nil, errResultCollectionStale
			}
			proof = &collectionWorkflowProof{taskID: item.ID, phase: phase, results: source.PhaseResults}
			completed = append(completed, item)
			break
		}
	}
	if proof == nil || !collectionFailuresMatchTasks(current.PhaseResults[proof.phase], proof.phase, completed) {
		return nil, nil
	}
	for _, item := range completed {
		delete(required, item.ID)
	}
	if len(required) != 0 {
		return nil, nil
	}
	released, err := c.verifyDeferredCollectionDispatches(events, items, replayed, deferred)
	if err != nil || len(released) != len(deferred) {
		return nil, err
	}
	proof.deferred = released
	return proof, nil
}

func (c *Coordinator) validateWorkflowCollectionCompletion(ctx context.Context, item, canonical *TodoItem, cp *ResultCollectionCheckpoint) error {
	if canonical == nil || canonical.Status != TaskDone ||
		!sameCanonicalEvidence(currentResultCollection(canonical), cp) ||
		!sameCanonicalEvidence(canonical.TypedResult, item.TypedResult) ||
		!sameCanonicalEvidence(collectionRepairGrants(canonical.ExecutionReceipt), collectionRepairGrants(item.ExecutionReceipt)) {
		return errResultCollectionStale
	}
	accepted, err := c.acceptedCollectionResult(ctx, item, cp)
	if err != nil {
		return err
	}
	if !sameCanonicalEvidence(accepted, item.TypedResult) {
		return errResultCollectionStale
	}
	return nil
}

func (c *Coordinator) verifyDeferredCollectionDispatches(events []RunEvent, items, replayed []*TodoItem, deferred map[string]bool) ([]string, error) {
	for _, event := range events {
		if deferred[event.TaskID] {
			switch EventType(event.Type) {
			case EventTaskStarted, EventTaskResultCollectionCheckpointed, EventStructuredStepReceiptRecorded:
				return nil, fmt.Errorf("deferred task %q already has execution events: %w", event.TaskID, errResultCollectionStale)
			}
		}
	}
	var reliability AntiThrashingState
	reliability.rebuild(items, c.reliabilityConfig())
	var released []string
	for _, item := range items {
		if item == nil || !isUnresolvedTaskStatus(item.Status) {
			continue
		}
		canonical := todoItemByID(replayed, item.ID)
		if !deferred[item.ID] || !sameResumeTaskEvidence(canonical, item) ||
			!sameCanonicalEvidence(canonical.FailureEvent, item.FailureEvent) || canonical.Detail != item.Detail {
			return nil, nil
		}
		if blocked, _ := reliability.dispatchBlock(taskDefFromTodoItem(item), item); blocked {
			return nil, fmt.Errorf("deferred task %q is still blocked by a reliability policy", item.ID)
		}
		released = append(released, item.ID)
	}
	return released, nil
}

// A phase can fail again after it has reopened. Its earlier repair grants
// remain valid provenance for accepted work, but cannot reopen this failure.
func collectionFailureJournalHeads(events []RunEvent, branchID string) (map[string]bool, error) {
	heads := make(map[string]bool)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if effectiveEventBranchID(event) != branchID {
			continue
		}
		if event.Type == string(EventWorkflowStateChanged) {
			var state WorkflowStateChangedPayload
			if err := json.Unmarshal(event.Payload, &state); err != nil {
				return nil, fmt.Errorf("decode workflow recovery boundary: %w", err)
			}
			if state.State != PhaseFailed {
				break
			}
		}
		heads[event.ID] = true
	}
	return heads, nil
}

func collectionRepairBelongsToFailure(item *TodoItem, branchID string, heads map[string]bool) bool {
	if !hasCommittedWorkflowCollectionRepair(item, branchID) {
		return false
	}
	cp := currentResultCollection(item)
	return slices.ContainsFunc(collectionRepairGrants(item.ExecutionReceipt), func(grant ResultRepairGrant) bool {
		return isWorkflowCollectionRepairGrant(grant, cp) && heads[grant.Recovery.JournalHeadID]
	})
}
