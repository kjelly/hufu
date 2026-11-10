package team

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// ResumeActivationPlan is a read-only observation, never an authorization.
// Activation checks it again under the workspace and project locks.
type ResumeActivationPlan struct {
	Workspace              string                  `json:"workspace"`
	WorkspaceID            string                  `json:"workspace_id,omitempty"`
	SourceBranchID         string                  `json:"source_branch_id"`
	ExpectedActiveBranchID string                  `json:"expected_active_branch_id"`
	ExpectedEventHead      string                  `json:"expected_event_head"`
	ExpectedEventHash      string                  `json:"expected_event_hash"`
	Mode                   versionstore.Mode       `json:"mode"`
	MetadataOnly           bool                    `json:"metadata_only"`
	TargetSnapshotID       versionstore.SnapshotID `json:"target_snapshot_id,omitempty"`
	TargetTreeHash         string                  `json:"target_tree_hash,omitempty"`
	TargetGeneration       int64                   `json:"target_generation,omitzero"`
	WorkspaceSnapshotProvenance
	MaterializedWorkspace string                  `json:"materialized_workspace,omitempty"`
	MaterializedBranch    string                  `json:"materialized_branch,omitempty"`
	MaterializedSnapshot  versionstore.SnapshotID `json:"materialized_snapshot,omitempty"`
}

// PlanResumeActivation neither creates a store nor enforces a mode floor. A
// missing target remains explicit; recovery may settle it during activation,
// but a changed plan must then be selected again before a new switch starts.
func PlanResumeActivation(ctx context.Context, workspace string, source *ResumeSource, version WorkspaceVersionContext) (*ResumeActivationPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || source.Session == nil || source.workspace != workspace {
		return nil, fmt.Errorf("resume source is missing or belongs to another workspace")
	}
	plan := &ResumeActivationPlan{
		Workspace: workspace, WorkspaceID: version.WorkspaceID, SourceBranchID: source.BranchID,
		ExpectedActiveBranchID: source.activeBranch, ExpectedEventHead: source.eventHead, ExpectedEventHash: source.eventHash,
		Mode: version.Mode, MetadataOnly: !version.Required(),
	}
	if plan.MetadataOnly {
		return plan, nil
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: version.StateDir, ReadOnly: true})
	if errors.Is(err, versionstore.ErrStoreNotFound) {
		return plan, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	if err := plan.readVersionState(ctx, version, store); err != nil {
		return nil, err
	}
	return plan, nil
}

func (p *ResumeActivationPlan) readVersionState(ctx context.Context, version WorkspaceVersionContext, store *versionstore.Store) error {
	state, _, err := store.GetSubjectState(ctx, version.SubjectRoot)
	if err != nil {
		return err
	}
	p.MaterializedWorkspace, p.MaterializedBranch, p.MaterializedSnapshot = state.MaterializedWorkspaceID, state.MaterializedBranchID, state.MaterializedSnapshotID
	head, meta, ok, err := store.GetBranchHead(ctx, version.WorkspaceID, p.SourceBranchID)
	if err != nil || !ok {
		return err
	}
	if head.State != versionstore.StatePublished {
		return fmt.Errorf("%w: target snapshot is not published", versionstore.ErrWorkspaceSnapshotUnavailable)
	}
	p.TargetSnapshotID, p.TargetTreeHash, p.TargetGeneration = head.ID, head.RootTreeHash, meta.Generation
	ops := &WorkspaceSessionOps{Version: version, Store: store}
	p.WorkspaceSnapshotProvenance, err = ops.SnapshotProvenance(ctx, head)
	return err
}

func (p *ResumeActivationPlan) validate(ctx context.Context, workspace string, source *ResumeSource, version WorkspaceVersionContext, store *versionstore.Store) error {
	if p == nil || p.Workspace != workspace || p.WorkspaceID != version.WorkspaceID || p.SourceBranchID != source.BranchID ||
		p.ExpectedActiveBranchID != source.activeBranch || p.ExpectedEventHead != source.eventHead || p.ExpectedEventHash != source.eventHash || p.Mode != version.Mode || p.MetadataOnly != !version.Required() {
		return ErrStaleResumeSource
	}
	if p.MetadataOnly {
		return nil
	}
	current := *p
	current.TargetSnapshotID, current.TargetTreeHash, current.TargetGeneration = "", "", 0
	current.WorkspaceSnapshotProvenance = WorkspaceSnapshotProvenance{}
	if err := current.readVersionState(ctx, version, store); err != nil {
		return err
	}
	if current.TargetSnapshotID != p.TargetSnapshotID || current.TargetTreeHash != p.TargetTreeHash || current.TargetGeneration != p.TargetGeneration || current.WorkspaceSnapshotProvenance != p.WorkspaceSnapshotProvenance ||
		current.MaterializedWorkspace != p.MaterializedWorkspace || current.MaterializedBranch != p.MaterializedBranch || current.MaterializedSnapshot != p.MaterializedSnapshot {
		return ErrStaleResumeSource
	}
	return nil
}

// Validate ownership only after pending operations have completed forward: a
// crash between session commit and materialization marking can temporarily
// leave these projections different without authorizing a new capture.
func (p *ResumeActivationPlan) validateMaterializedOwner(version WorkspaceVersionContext) error {
	if p.MaterializedWorkspace == "" && p.MaterializedBranch == "" && p.MaterializedSnapshot == "" {
		return nil
	}
	if p.MaterializedWorkspace != version.WorkspaceID || p.MaterializedBranch != p.ExpectedActiveBranchID || p.MaterializedSnapshot == "" {
		return fmt.Errorf("%w: materialized workspace/branch %s/%s does not match active %s/%s; inspect workspace version status before resuming", versionstore.ErrWorkspaceRecoveryRequired, p.MaterializedWorkspace, p.MaterializedBranch, version.WorkspaceID, p.ExpectedActiveBranchID)
	}
	return nil
}
