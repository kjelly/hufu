package team

import (
	"context"
	"fmt"
	"slices"
)

// Failed workflows remain closed to delegation. An explicit collection repair
// settles only the explicitly selected protocol task. Match typed
// failure provenance rather than message text, which can change during repair.
func resultRepairWorkflowPhase(session *SessionData, taskID string) (Phase, error) {
	if session == nil || session.WorkflowState != PhaseFailed {
		return blockedCollectionWorkflowPhase(session, taskID, validateCollectionRecoveryTarget)
	}
	item := todoItemByID(session.Tasks, taskID)
	if err := validateCollectionRecoveryTarget(item); err != nil {
		return "", err
	}
	var failed []*TodoItem
	for _, candidate := range session.Tasks {
		if candidate == nil {
			continue
		}
		if isUnresolvedTaskStatus(candidate.Status) {
			if candidate.Phase != item.Phase || validateCollectionRecoveryTarget(candidate) != nil {
				return "", fmt.Errorf("failed workflow contains an unrelated unresolved task")
			}
			failed = append(failed, candidate)
		} else if candidate.Phase == item.Phase && hasCommittedWorkflowCollectionRepair(candidate) {
			failed = append(failed, candidate)
		}
	}
	if !collectionFailuresMatchTasks(session.PhaseResults[item.Phase], item.Phase, failed) {
		return "", fmt.Errorf("workflow failure is not attributable to its collection tasks")
	}
	return item.Phase, nil
}

func hasCommittedWorkflowCollectionRepair(item *TodoItem) bool {
	if item.Status != TaskDone {
		return false
	}
	cp := currentResultCollection(item)
	if cp == nil || cp.State != ResultCollectionCommitted {
		return false
	}
	return slices.ContainsFunc(collectionRepairGrants(item.ExecutionReceipt), func(grant ResultRepairGrant) bool {
		return grant.Recovery != nil && grant.Recovery.ValidatorRevision >= 4 && validateStoredRecoveryGrant(grant, cp) == nil
	})
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
	_, err := resultRepairWorkflowPhase(c.collectionWorkflowSnapshot(), id)
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
	if !w.reconcileCollectedFailure(proof.taskID, proof.phase, proof.results) {
		return errResultCollectionStale
	}
	c.saveCheckpoint()
	return nil
}

type collectionWorkflowProof struct {
	taskID  string
	phase   Phase
	results map[Phase]PhaseResult
}

func (c *Coordinator) completedCollectionWorkflowProof(ctx context.Context) (*collectionWorkflowProof, error) {
	current := c.collectionWorkflowSnapshot()
	if current.WorkflowState != PhaseFailed {
		return nil, nil
	}
	// A repair grant never authorizes the other blocked tasks or delegation.
	// Keep the original phase failure intact across partial repairs and crashes.
	if len(UnresolvedTaskReferences(current.Tasks)) != 0 {
		return nil, nil
	}
	items := current.Tasks
	var proof *collectionWorkflowProof
	var completed []*TodoItem
	required := make(map[string]bool)
	for _, item := range items {
		if item == nil || !hasCommittedWorkflowCollectionRepair(item) {
			continue
		}
		cp := currentResultCollection(item)
		for _, grant := range collectionRepairGrants(item.ExecutionReceipt) {
			if grant.Recovery == nil || grant.Recovery.ValidatorRevision < 4 || validateStoredRecoveryGrant(grant, cp) != nil {
				continue
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
			canonical := todoItemByID(replayed, item.ID)
			if canonical == nil || canonical.Status != TaskDone || !sameCanonicalEvidence(currentResultCollection(canonical), cp) || !sameCanonicalEvidence(canonical.TypedResult, item.TypedResult) || !sameCanonicalEvidence(collectionRepairGrants(canonical.ExecutionReceipt), collectionRepairGrants(item.ExecutionReceipt)) {
				return nil, errResultCollectionStale
			}
			accepted, err := c.acceptedCollectionResult(ctx, item, cp)
			if err != nil {
				return nil, err
			}
			if !sameCanonicalEvidence(accepted, item.TypedResult) {
				return nil, errResultCollectionStale
			}
			i := slices.IndexFunc(events, func(event RunEvent) bool { return event.ID == grant.Recovery.JournalHeadID })
			if i < 0 {
				return nil, errResultCollectionStale
			}
			source := ReduceToSessionData(events[:i+1])
			var phase Phase
			if grant.Recovery.ValidatorRevision < 5 {
				phase, err = blockedCollectionWorkflowPhase(source, item.ID, validateCollectionRecoveryTarget)
			} else {
				phase, err = resultRepairWorkflowPhase(source, item.ID)
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
			if err := c.validateAcceptedDependencyPayload(item); err != nil {
				return nil, err
			}
			for _, peer := range source.Tasks {
				if peer != nil && peer.Phase == phase && (isUnresolvedTaskStatus(peer.Status) || hasCommittedWorkflowCollectionRepair(peer)) {
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
	return proof, nil
}
