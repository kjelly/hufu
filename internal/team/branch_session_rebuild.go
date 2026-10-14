package team

import "fmt"

// Branch switches rebuild session.json from the target branch's event
// lineage. Every branch-specific field comes from replay; only the state that
// belongs to the workspace rather than a branch is carried over from the live
// session (carryWorkspaceSessionState). A field added to SessionData must be
// classified in TestSessionDataFieldsAreClassifiedForBranchSwitch.

// RebuildSessionForBranch rewrites workspace/session.json as branchID's
// replayed session, so the next run resumes the branch's own state. When the
// lineage has no task events, tasks fall back to the branch's TaskPlan
// snapshot. A branch with no lineage yields a fresh session. A terminal
// commit awaiting confirmation is recorded only in session.json: it stays
// with a rebuild of its own branch and blocks a switch to any other.
func RebuildSessionForBranch(workspace string, st *SessionTree, es *EventStore, branchID string) error {
	return rebuildSessionForBranch(workspaceDocs(workspace), st, es, branchID)
}

// rebuildSessionForBranch is RebuildSessionForBranch reading and writing the
// session through docs.
func rebuildSessionForBranch(docs runtimeDocs, st *SessionTree, es *EventStore, branchID string) error {
	b, lineage, tasks, err := branchReplayInputs(st, es, branchID)
	if err != nil {
		return err
	}
	live, _, err := loadSessionFrom(docs)
	if err != nil {
		return fmt.Errorf("rebuild session for branch %q: %w", b.ID, err)
	}
	if err := refusePendingTerminalCommitSwitch(live, b.ID); err != nil {
		return err
	}
	if live == nil {
		live = NewSession()
	}
	next := ReduceToSessionData(lineage)
	next.Tasks = tasks
	carryWorkspaceSessionState(next, live)
	next.PendingTerminalCommit = live.PendingTerminalCommit
	if err := saveSessionTo(docs, next); err != nil {
		return fmt.Errorf("rebuild session for branch %q: %w", b.ID, err)
	}
	return nil
}

// carryWorkspaceSessionState copies into next the session state that belongs
// to the workspace, not to a branch: the runtime workspace root, the
// learning-gap repair queue (each repair event is pinned to its branch), the
// round counter that only names STM checkpoints, and the conversation-history
// source bookkeeping, which compaction state rewrites per branch.
func carryWorkspaceSessionState(next, live *SessionData) {
	next.RuntimeWorkspace = live.RuntimeWorkspace
	next.LearningGaps = append([]LearningGap(nil), live.LearningGaps...)
	next.Rounds = live.Rounds
	next.ConversationHistorySourceOffset = live.ConversationHistorySourceOffset
	next.ConversationHistorySourceCounts = append([]int(nil), live.ConversationHistorySourceCounts...)
	next.ConversationHistorySourceRanges = cloneSourceRanges(live.ConversationHistorySourceRanges)
	next.ConversationHistoryNextSourceIndex = live.ConversationHistoryNextSourceIndex
}

// RefreshActiveBranchProjection rewrites the replayed task, conversation,
// criterion and execution-policy projection of the active branch in place,
// after canonical events were appended to it. The rest of session.json is
// the same branch's live state and is kept, including recovery state that
// only session.json records. A lineage with nothing to project leaves
// session.json untouched.
func RefreshActiveBranchProjection(workspace string, st *SessionTree, es *EventStore) error {
	if st == nil {
		return fmt.Errorf("refresh active branch projection: session tree is nil")
	}
	b, lineage, tasks, err := branchReplayInputs(st, es, st.ActiveBranch)
	if err != nil {
		return err
	}
	replayed := ReduceToSessionData(lineage)
	if len(tasks) == 0 && len(replayed.Entries) == 0 && len(replayed.CriterionResults) == 0 && len(replayed.CriterionCheckpoints) == 0 && replayed.LastCriterionProgressAt == "" && replayed.ExecutionPolicySnapshot == nil {
		return nil
	}
	sd := LoadSession(workspace)
	if sd == nil {
		sd = NewSession()
	}
	sd.Tasks = tasks
	sd.Entries = replayed.Entries
	sd.CriterionResults = replayed.CriterionResults
	sd.CriterionCheckpoints = replayed.CriterionCheckpoints
	sd.LastCriterionProgressAt = replayed.LastCriterionProgressAt
	sd.ExecutionPolicySnapshot = cloneExecutionPolicySnapshot(replayed.ExecutionPolicySnapshot)
	if err := SaveSession(workspace, sd); err != nil {
		return fmt.Errorf("refresh projection for branch %q: %w", b.ID, err)
	}
	return nil
}

// RefuseBranchSwitchWithPendingTerminalCommit reports an error when making
// targetBranchID active would leave a branch whose run awaits confirmation
// that its run_finished event is durable. Only session.json records that
// commit, so a switch would lose it; the next run on its branch confirms it.
// An empty target names a branch being created, which never owns a commit.
func RefuseBranchSwitchWithPendingTerminalCommit(workspace, targetBranchID string) error {
	live, _, err := LoadSessionReadOnly(workspace)
	if err != nil {
		return fmt.Errorf("check pending terminal commit: %w", err)
	}
	return refusePendingTerminalCommitSwitch(live, targetBranchID)
}

func refusePendingTerminalCommitSwitch(live *SessionData, targetBranchID string) error {
	if live == nil || live.PendingTerminalCommit == nil {
		return nil
	}
	pending := live.PendingTerminalCommit
	if targetBranchID != "" && effectiveBranchID(targetBranchID) == effectiveBranchID(pending.BranchID) {
		return nil
	}
	return fmt.Errorf("run %q on branch %q has an unconfirmed terminal commit; run `hufu resume` on this branch to confirm it before switching branches", pending.RunID, pending.BranchID)
}

func branchReplayInputs(st *SessionTree, es *EventStore, branchID string) (*SessionBranch, []RunEvent, []*TodoItem, error) {
	if st == nil {
		return nil, nil, nil, fmt.Errorf("rebuild session: session tree is nil")
	}
	b := st.GetBranch(branchID)
	if b == nil {
		return nil, nil, nil, fmt.Errorf("rebuild session: branch %q not found", branchID)
	}
	var events []RunEvent
	if es != nil {
		events, _ = es.ReadEvents()
	}
	lineage := FilterEventsForBranch(events, st, b.ID)
	tasks, err := ReplayTodoList(lineage)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rebuild session: replay execution identity: %w", err)
	}
	if len(tasks) == 0 && len(b.State.TaskPlan) > 0 {
		tasks = make([]*TodoItem, len(b.State.TaskPlan))
		for i, t := range b.State.TaskPlan {
			tasks[i] = cloneTodoItem(t)
		}
	}
	return b, lineage, tasks, nil
}

// effectiveBranchID names the implicit main lineage explicitly.
func effectiveBranchID(branchID string) string {
	if branchID == "" {
		return "main"
	}
	return branchID
}
