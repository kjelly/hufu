package team

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ResumeSource is a read-only projection of the exact selected event lineage.
// Selection never creates a branch or substitutes the active checkpoint.
type ResumeSource struct {
	BranchID string
	Session  *SessionData
}

func ReadResumeSource(workspace, target, runID string) (*ResumeSource, error) {
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return nil, err
	}
	if tree == nil {
		return nil, fmt.Errorf("resume requires a canonical session tree")
	}
	if target == "" {
		target = tree.ActiveBranch
	}
	store, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	branch := tree.GetBranch(target)
	if branch == nil {
		return nil, ResumeSourceError(workspace, fmt.Errorf("resume branch %q does not exist", target))
	}
	_, lineage, tasks, err := branchReplayInputs(tree, store, branch.ID)
	if err != nil {
		return nil, ResumeSourceError(workspace, err)
	}
	if len(lineage) == 0 || !slices.ContainsFunc(tasks, func(item *TodoItem) bool { return item != nil && !IsPrimaryOccurrence(item) }) {
		return nil, ResumeSourceError(workspace, fmt.Errorf("branch %q has no canonical tasks to resume", branch.ID))
	}
	if runID != "" && !slices.ContainsFunc(lineage, func(event RunEvent) bool { return event.RunID == runID }) {
		return nil, ResumeSourceError(workspace, fmt.Errorf("run %q is not in branch %q", runID, branch.ID))
	}
	session := ReduceToSessionData(lineage)
	session.Tasks = tasks
	return &ResumeSource{BranchID: branch.ID, Session: session}, nil
}

// ResumeSourceError retains the refusal and names inspectable alternatives.
func ResumeSourceError(workspace string, cause error) error {
	tree, err := LoadSessionTree(workspace)
	if err != nil || tree == nil {
		return fmt.Errorf("resume refused: %w (recovery sources unavailable)", cause)
	}
	var sources []string
	for _, branch := range tree.Branches {
		if branch != nil {
			sources = append(sources, branch.ID)
		}
	}
	slices.Sort(sources)
	return fmt.Errorf("resume refused: %w; available recovery branches: %s (inspect with session list / inspect task --branch)", cause, strings.Join(sources, ", "))
}

// ActivateResumeSource is called under the workspace lease. Checkout commits
// the outgoing snapshot and the selected replay atomically, retaining both.
func ActivateResumeSource(ctx context.Context, workspace string, source *ResumeSource) error {
	if source == nil || source.Session == nil {
		return fmt.Errorf("resume source is missing")
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return err
	}
	store, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	_, _, err = CheckoutSessionBranch(ctx, workspace, tree, store, source.BranchID)
	return err
}
