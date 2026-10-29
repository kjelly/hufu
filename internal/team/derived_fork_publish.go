package team

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func (o *WorkspaceSessionOps) commitDerivedForkSnapshot(ctx context.Context, child versionstore.Snapshot, prepared *PreparedDerivedFork) (versionstore.Snapshot, error) {
	if err := o.validatePreparedDerivedFork(ctx, child, prepared); err != nil {
		return versionstore.Snapshot{}, err
	}
	branch := o.Tree.Branches[child.BranchID]
	if branch == nil || branch.Derivation == nil {
		return versionstore.Snapshot{}, ErrDerivedForkIntent
	}
	commit, err := workspaceSnapshotCommittedEvent(child, versionstore.CaptureStats{})
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	data, err := json.Marshal(DerivedForkCreatedPayload{SchemaVersion: derivedForkIntentVersion,
		Intent: *branch.Derivation, SnapshotID: string(child.ID)}, json.Deterministic(true))
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	created := RunEvent{Type: string(EventDerivedForkCreated), BranchID: branch.ID, Actor: "workspace-versioning",
		IdempotencyKey: derivedForkIdempotencyKey(string(child.ID)), Payload: data}
	if err := workspaceStage("fork:before_events"); err != nil {
		return versionstore.Snapshot{}, err
	}
	durable, err := o.Events.AppendPersistedBatchAtHead(ctx, []RunEvent{commit, created}, prepared.headID)
	if err != nil {
		return versionstore.Snapshot{}, err
	}
	if len(durable) != 2 {
		return versionstore.Snapshot{}, fmt.Errorf("derived fork publication returned an incomplete batch")
	}
	if _, err := o.Store.PublishSnapshot(ctx, child.ID, durable[0].ID, 0); err != nil {
		return versionstore.Snapshot{}, err
	}
	return o.Store.GetSnapshot(ctx, child.ID)
}

func (o *WorkspaceSessionOps) validatePreparedDerivedFork(ctx context.Context, child versionstore.Snapshot, prepared *PreparedDerivedFork) error {
	if err := o.validateDerivedForkScope(); err != nil {
		return err
	}
	if prepared == nil || prepared.owner != o || prepared.active != o.Tree.ActiveBranch ||
		o.Version.WorkspaceID != prepared.intent.WorkspaceID || o.Workspace != prepared.intent.Workspace ||
		o.Version.TeamName != prepared.intent.Team || o.Version.SubjectRoot != prepared.subjectRoot ||
		o.Version.ProjectID != prepared.projectID || o.Version.StateDir != prepared.stateDir {
		return ErrDerivedForkIntent
	}
	branch := o.Tree.Branches[child.BranchID]
	if branch == nil || branch.Derivation == nil || child.Parent != prepared.snapshot.ID ||
		child.RootTreeHash != prepared.intent.SourceRootTreeHash || child.WorkspaceID != prepared.intent.WorkspaceID ||
		child.Reason != versionstore.SnapshotFork || !child.Materializable {
		return ErrDerivedForkIntent
	}
	expected := prepared.intent
	expected.BranchID = branch.ID
	if expected != *branch.Derivation || validateDerivedForkIntent(branch.Derivation, branch) != nil {
		return ErrDerivedForkIntent
	}
	if err := o.Events.VerifyHashChain(); err != nil {
		return err
	}
	events, err := o.Events.ReadEvents()
	if err != nil {
		return err
	}
	if len(events) == 0 || events[len(events)-1].ID != prepared.headID || events[len(events)-1].Hash != prepared.headHash {
		return ErrEventHeadChanged
	}
	return ctx.Err()
}
