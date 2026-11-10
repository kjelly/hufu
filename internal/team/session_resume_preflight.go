package team

import (
	"context"
	"fmt"
)

// ResumePreflightAssessment describes source readiness, not an execution or
// acceptance outcome. Provider reachability and future model output are not
// evaluated by this read-only check.
type ResumePreflightAssessment struct {
	Branch             string `json:"branch"`
	PolicySHA256       string `json:"policy_sha256"`
	Accepted           int    `json:"accepted"`
	Resumable          int    `json:"resumable"`
	TerminalUnresolved int    `json:"terminal_unresolved"`
}

// AssessResumeSource runs ordinary policy, frozen-input, retained-result and
// artifact-scope admission without activating a branch or invoking providers.
// Like result-only recovery preview, it requires a fresh dry-run coordinator.
func (c *Coordinator) AssessResumeSource(ctx context.Context, source *ResumeSource) (*ResumePreflightAssessment, error) {
	if c == nil || c.session == nil || c.eventStore != nil || c.executionRunID != "" || source == nil || source.Session == nil || source.workspace != c.session.Workspace {
		return nil, fmt.Errorf("resume preflight requires a fresh read-only coordinator and canonical source")
	}
	if digest, err := collectionDigest(source.Session); err != nil || digest != source.sessionSHA256 {
		return nil, fmt.Errorf("resume preflight source projection changed")
	}
	if source.Session.RecoveryRequired {
		return nil, fmt.Errorf("resume preflight requires session recovery: %s", source.Session.RecoveryReason)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.prepareRecoveryValidation(); err != nil {
		return nil, err
	}
	matched, err := c.executionPolicySnapshotMatchesCurrent(source.Session.ExecutionPolicySnapshot)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, c.executionPolicySnapshotDriftError(source.Session.ExecutionPolicySnapshot)
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	restore := c.installResumeValidationSource(source, store)
	defer restore()
	if err := c.validateResumeSources(WithSessionResume(ctx)); err != nil {
		return nil, err
	}
	if source.Session.WorkflowState == PhaseDone {
		return nil, fmt.Errorf("workflow is already complete; start a follow-up request instead of resume")
	}
	if source.Session.WorkflowState == PhaseFailed {
		proof, err := c.completedCollectionWorkflowProof(ctx)
		if err != nil {
			return nil, err
		}
		if proof == nil {
			return nil, fmt.Errorf("workflow is failed; use an eligible explicit result-only repair or targeted reconciliation")
		}
	}
	assessment := &ResumePreflightAssessment{Branch: source.BranchID, PolicySHA256: source.Session.ExecutionPolicySnapshot.ConfigurationHash}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || IsPrimaryOccurrence(item) || item.Status == TaskSkipped {
			continue
		}
		switch {
		case item.Status == TaskDone:
			assessment.Accepted++
		case isInterruptedStatus(item.Status):
			assessment.Resumable++
		default:
			assessment.TerminalUnresolved++
		}
	}
	return assessment, nil
}

// Both ordinary and result-only previews need the same frozen-input selection
// and exact branch scope as execution. Installing only tasks loses native
// output provenance and can silently leave the read-only store on main.
func (c *Coordinator) installResumeValidationSource(source *ResumeSource, store *EventStore) func() {
	previousData, previousTracker := c.sessionData, c.taskTracker
	previousStore, previousJournal := c.eventStore, c.eventJournal
	previousBranch, previousRun, previousSelection := c.recoveryBranch, c.executionRunID, c.runInputSelectionRunID
	projection := *source.Session
	projection.RunInputSnapshots = CloneRunInputSnapshots(source.Session.RunInputSnapshots)
	projection.MediaInputSnapshots = cloneMediaInputSnapshots(source.Session.MediaInputSnapshots)
	projection.Tasks = make([]*TodoItem, len(source.Session.Tasks))
	for i, item := range source.Session.Tasks {
		projection.Tasks[i] = cloneTodoItem(item)
	}
	restoreLegacyBoundInvariantVerification(projection.Tasks, c.session.ContractTasks)
	c.sessionData = &projection
	c.taskTracker = NewTaskTracker()
	c.taskTracker.TodoList().AddReserved(projection.Tasks)
	store.SetBranchID(source.BranchID)
	c.eventStore, c.eventJournal = store, eventStoreJournal{store: store}
	c.recoveryBranch, c.executionRunID = source.BranchID, "resume-preview-"+source.eventHead
	c.runInputSelectionRunID = c.executionRunID
	return func() {
		c.sessionData, c.taskTracker = previousData, previousTracker
		c.eventStore, c.eventJournal = previousStore, previousJournal
		c.recoveryBranch, c.executionRunID, c.runInputSelectionRunID = previousBranch, previousRun, previousSelection
	}
}
