package team

// DerivedForkRef is a disposable reference to the native initialization event.
// Its pending state is cleared only by the first derived_fork workflow cycle.
// The immutable branch intent and verified event lineage establish authority.
type DerivedForkRef struct {
	BranchID           string `json:"branch_id"`
	EventID            string `json:"event_id"`
	EventHash          string `json:"event_hash"`
	SnapshotID         string `json:"snapshot_id"`
	Team               string `json:"team"`
	AwaitingNewRequest bool   `json:"awaiting_new_request"`
	FirstCycleID       string `json:"first_cycle_id,omitempty"`
}

func cloneDerivedForkRef(ref *DerivedForkRef) *DerivedForkRef {
	if ref == nil {
		return nil
	}
	return new(*ref)
}

func resetDerivedForkProjection(sd *SessionData, event RunEvent, payload DerivedForkCreatedPayload) {
	// The parent remains in the journal. None of its executable state or
	// conversational instructions become the child's new request.
	workspace, gaps, rounds := sd.RuntimeWorkspace, sd.LearningGaps, sd.Rounds
	*sd = *NewSession()
	sd.CreatedAt, sd.UpdatedAt = event.Timestamp, event.Timestamp
	sd.RuntimeWorkspace, sd.LearningGaps, sd.Rounds = workspace, gaps, rounds
	resetWorkflowCycleProjection(sd, "", payload.Intent.TaskIDFloor)
	sd.DerivedFork = &DerivedForkRef{
		BranchID: event.BranchID, EventID: event.ID, EventHash: event.Hash,
		SnapshotID: payload.SnapshotID, Team: payload.Intent.Team, AwaitingNewRequest: true,
	}
}

func sameDerivedForkLineage(left, right *DerivedForkRef) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.BranchID == right.BranchID && left.EventID == right.EventID && left.EventHash == right.EventHash
}

func (c *Coordinator) initialDerivedCycle() bool {
	var initial bool
	c.viewSessionData(func(sd *SessionData) {
		initial = sd.DerivedFork != nil && (sd.DerivedFork.AwaitingNewRequest || sd.DerivedFork.FirstCycleID == sd.WorkflowCycleID)
	})
	return initial
}

func (o *WorkspaceSessionOps) materializeForkCompaction(docs runtimeDocs, branch *SessionBranch) error {
	if branch.Derivation == nil {
		return materializeCompactionBranch(docs, o.Workspace, o.Events, branch.ParentID, branch.ID, branch.ForkEventID)
	}
	state, exists, err := loadCompactionStateFrom(docs)
	if err != nil || !exists {
		return err
	}
	state = cloneCompactionState(state)
	delete(state.Branches, branch.ID)
	delete(state.Checkpoints, branch.ID)
	return saveCompactionStateTo(docs, o.Workspace, state, o.Events)
}
