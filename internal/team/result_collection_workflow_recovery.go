package team

import (
	"context"
	"fmt"
	"slices"
)

// Failed workflows remain closed to delegation. An explicit collection repair
// can only settle the failed phase's sole remaining protocol task. Match typed
// failure provenance rather than message text, which can change during repair.
func resultRepairWorkflowPhase(session *SessionData, taskID string) (Phase, error) {
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
	if item == nil || len(unresolved) != 1 || unresolved[0].ID != taskID || validateCollectionRecoveryTarget(item) != nil {
		return "", fmt.Errorf("failed workflow recovery requires its sole blocked collection task")
	}
	result, exists := session.PhaseResults[item.Phase]
	if !exists || result.Status != PhaseStatusBlocked || len(result.Errors) != 1 {
		return "", fmt.Errorf("workflow failure is not attributable to the selected collection")
	}
	failure := result.Errors[0]
	if failure.Category != "TASK_BLOCKED" || failure.Phase != item.Phase || failure.Component != item.Agent || failure.Source != item.Agent {
		return "", fmt.Errorf("workflow failure does not match the selected task's canonical failure")
	}
	return item.Phase, nil
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
	items := current.Tasks
	for _, item := range items {
		if item == nil {
			continue
		}
		cp := currentResultCollection(item)
		if item.Status != TaskDone || cp == nil || cp.State != ResultCollectionCommitted {
			continue
		}
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
			phase, err := resultRepairWorkflowPhase(source, item.ID)
			if err != nil || phase == "" {
				continue
			}
			if !sameCanonicalEvidence(current.PhaseResults, source.PhaseResults) {
				return nil, fmt.Errorf("workflow changed after the authorized collection repair: %w", errResultCollectionStale)
			}
			if err := c.validateAcceptedDependencyPayload(item); err != nil {
				return nil, err
			}
			return &collectionWorkflowProof{taskID: item.ID, phase: phase, results: source.PhaseResults}, nil
		}
	}
	return nil, nil
}
