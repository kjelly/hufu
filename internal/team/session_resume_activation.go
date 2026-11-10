package team

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// ErrStaleResumeSource refuses activation after the selected canonical source
// changed. The caller must select again rather than use a preview as authority.
var ErrStaleResumeSource = errors.New("stale_activation_plan")

// ActivateVersionedResumeSource runs under the team's workspace lease and, in
// required mode, the project lock. It shares checkout's durable materialization
// and recovery instead of changing only the session's active branch.
func ActivateVersionedResumeSource(ctx context.Context, workspace string, source *ResumeSource, version WorkspaceVersionContext) error {
	plan, err := PlanResumeActivation(ctx, workspace, source, version)
	if err != nil {
		return err
	}
	return ActivateResumePlan(ctx, workspace, source, version, plan)
}

// ActivateResumePlan revalidates the complete observation before starting a
// switch. A plan supplied by a caller cannot replace current native checks.
func ActivateResumePlan(ctx context.Context, workspace string, source *ResumeSource, version WorkspaceVersionContext, plan *ResumeActivationPlan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == nil || source.Session == nil || source.workspace != workspace {
		return fmt.Errorf("resume source is missing or belongs to another workspace")
	}
	if version.Required() && (!version.HoldsProjectLock || version.ControlRoot != workspace) {
		return fmt.Errorf("resume activation requires the bound workspace's project lock")
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return err
	}
	events, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		return err
	}
	if err = validateResumeActivation(source, tree, events); err != nil {
		return errors.Join(err, events.Close())
	}
	if err = events.Close(); err != nil {
		return err
	}
	if !version.Required() {
		if err := plan.validate(ctx, workspace, source, version, nil); err != nil {
			return err
		}
		if tree.ActiveBranch == source.BranchID {
			return nil
		}
		return activateMetadataResumeSource(ctx, workspace, source)
	}
	return activateRequiredResume(ctx, workspace, source, version, tree, plan)
}

func validateResumeActivation(source *ResumeSource, tree *SessionTree, events *EventStore) error {
	if tree == nil || tree.ActiveBranch != source.activeBranch {
		return ErrStaleResumeSource
	}
	rows, err := events.ReadEvents()
	if err != nil {
		return err
	}
	head, hash := "", ""
	if len(rows) > 0 {
		head, hash = rows[len(rows)-1].ID, rows[len(rows)-1].Hash
	}
	if head != source.eventHead || hash != source.eventHash {
		return ErrStaleResumeSource
	}
	return nil
}

func activateRequiredResume(ctx context.Context, workspace string, source *ResumeSource, version WorkspaceVersionContext, tree *SessionTree, plan *ResumeActivationPlan) error {
	store, err := version.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	if err := plan.validate(ctx, workspace, source, version, store); err != nil {
		return err
	}
	events, err := OpenEventStore(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = events.Close() }()
	ops := &WorkspaceSessionOps{Version: version, Workspace: workspace, Tree: tree, Events: events, Store: store}
	if _, err := RecoverWorkspaceOperations(ctx, ops); err != nil {
		return err
	}
	// Forward recovery can settle a prior switch. Do not silently use a plan
	// made before it: selection on the next invocation sees its durable state.
	if err := validateResumeActivation(source, tree, events); err != nil {
		return err
	}
	if err := plan.validate(ctx, workspace, source, version, store); err != nil {
		return err
	}
	if err := plan.validateMaterializedOwner(version); err != nil {
		return err
	}
	if tree.ActiveBranch == source.BranchID {
		// Admission owns same-branch external drift and checkpoints. In
		// particular, do not rebuild away result-collection recovery state.
		return nil
	}
	head, _, ok, err := store.GetBranchHead(ctx, version.WorkspaceID, source.BranchID)
	if err != nil {
		return err
	}
	if !ok || head.State != versionstore.StatePublished {
		return fmt.Errorf("%w: branch %q has no published workspace snapshot", versionstore.ErrWorkspaceSnapshotUnavailable, source.BranchID)
	}
	_, err = ops.Checkout(ctx, source.BranchID)
	return err
}
