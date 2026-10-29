package team

import (
	"context"
	"fmt"
	"strings"
)

// ReadDerivedForkState verifies the selected branch without opening a writer,
// migrating storage, or performing workspace recovery.
func ReadDerivedForkState(ctx context.Context, workspace string) (*DerivedForkRef, error) {
	tree, err := LoadSessionTreeContext(ctx, workspace)
	if err != nil {
		return nil, err
	}
	// Ordinary workspaces need no extra full event scan or snapshot copy.
	seen, derived := make(map[string]bool), false
	for branch := tree.GetBranch(tree.ActiveBranch); branch != nil; branch = tree.GetBranch(branch.ParentID) {
		if seen[branch.ID] {
			return nil, ErrDerivedForkIntent
		}
		seen[branch.ID] = true
		if branch.Derivation != nil {
			derived = true
			break
		}
		if branch.ParentID == "" {
			break
		}
	}
	if !derived {
		return nil, nil
	}
	store, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return nil, err
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, tree.ActiveBranch)
	if err != nil {
		return nil, err
	}
	return cloneDerivedForkRef(ReduceToSessionData(lineage).DerivedFork), nil
}

// AwaitingDerivedForkRequest reports the checkpoint projection. Admission
// always verifies the original native event before using this state.
func (c *Coordinator) AwaitingDerivedForkRequest() bool {
	var pending bool
	c.viewSessionData(func(sd *SessionData) { pending = sd.DerivedFork != nil && sd.DerivedFork.AwaitingNewRequest })
	return pending
}

func (c *Coordinator) rejectPendingDerivedFork() error {
	if c.AwaitingDerivedForkRequest() {
		return fmt.Errorf("derived branch is awaiting_new_request; use run with a new nonempty request for the same team")
	}
	return nil
}

// ValidateDerivedForkRequest is read-only startup admission. Call it before
// provider probes, semantic warmup, or interpreting the request with a model.
func (c *Coordinator) ValidateDerivedForkRequest(ctx context.Context, prompt string) error {
	if !c.AwaitingDerivedForkRequest() {
		return nil
	}
	if resume, _ := ctx.Value(sessionResumeKey{}).(bool); resume || strings.TrimSpace(prompt) == "" || c.IsWrapUp() {
		return fmt.Errorf("derived branch is awaiting_new_request; resume cannot execute parent work")
	}
	if c.session == nil || !c.session.WorkspaceVersion.Required() || !c.session.WorkspaceVersion.HoldsProjectLock ||
		!c.session.Scope.Managed || c.session.Scope.ControlRoot != c.session.Workspace {
		return fmt.Errorf("derived request requires its locked managed workspace in required versioning mode")
	}
	if c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return fmt.Errorf("derived request requires an enabled runtime workflow")
	}
	lineage, err := c.derivedForkRequestLineage(ctx)
	if err != nil {
		return err
	}
	canonical := ReduceToSessionData(lineage)
	live := c.SessionData()
	if canonical.DerivedFork == nil || !canonical.DerivedFork.AwaitingNewRequest || live == nil || live.DerivedFork == nil ||
		*canonical.DerivedFork != *live.DerivedFork || canonical.DerivedFork.Team != strings.ToLower(strings.TrimSpace(c.session.Config.Name)) ||
		canonical.RecoveryRequired || live.RecoveryRequired || live.PendingTerminalCommit != nil || canonical.WorkflowState != PhaseInit || len(canonical.Tasks) != 0 {
		return ErrDerivedForkIntent
	}
	return nil
}

func (c *Coordinator) derivedForkRequestLineage(ctx context.Context) ([]RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store := c.EventStore()
	if store == nil {
		var err error
		store, err = OpenEventStoreReadOnly(c.session.Workspace)
		if err != nil {
			return nil, err
		}
		defer func() { _ = store.Close() }()
	} else if err := store.VerifyHashChain(); err != nil {
		return nil, err
	}
	events, err := store.ReadEvents()
	if err != nil {
		return nil, err
	}
	tree, err := LoadSessionTreeContext(ctx, c.session.Workspace)
	if err != nil {
		return nil, err
	}
	if c.EventStore() != nil && c.EventStore().BranchID() != tree.ActiveBranch {
		return nil, ErrDerivedForkIntent
	}
	return ProjectValidatedEventsForBranch(events, tree, tree.ActiveBranch)
}
