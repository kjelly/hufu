package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// PreparedDerivedFork is an observation under the native workspace/project
// locks. Only its creating operation may use it; publication still rechecks
// its source and conditionally commits at the observed event head.
type PreparedDerivedFork struct {
	owner       *WorkspaceSessionOps
	headID      string
	headHash    string
	active      string
	intent      DerivedForkIntent
	snapshot    versionstore.Snapshot
	subjectRoot string
	projectID   string
	stateDir    string
}

// PrepareDerivedFork performs no recovery, capture, branch creation, or
// materialization. In particular it must precede workspace startup recovery
// on this entry, so rejected uncertain effects are never reconciled implicitly.
func (o *WorkspaceSessionOps) PrepareDerivedFork(ctx context.Context, target string) (*PreparedDerivedFork, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := o.validateDerivedForkScope(); err != nil {
		return nil, err
	}
	if err := o.validateDerivedVersionState(ctx); err != nil {
		return nil, err
	}
	// Reuse the already verified store under the exclusive native locks.
	// Avoid a second full runtime.sqlite snapshot merely for this preflight.
	if err := preflightEventRead(ctx, o.Events.store.DB(), EventReadLimits{MaxEvents: 1_000_000, MaxPayloadBytes: 256 << 20}); err != nil {
		return nil, err
	}
	events, err := o.Events.ReadEvents()
	if err != nil {
		return nil, err
	}
	if err := o.validateDerivedLiveCheckpoint(ctx); err != nil {
		return nil, err
	}
	if err := validateDerivedForkWorldSafety(events); err != nil {
		return nil, err
	}
	active, err := ProjectValidatedEventsForBranch(events, o.Tree, o.Tree.ActiveBranch)
	if err != nil {
		return nil, err
	}
	if _, err := o.Events.validateHistoricalResultGrantProjection(ctx, active); err != nil {
		return nil, err
	}
	if err := validateDerivedForkSourceSafety(active, o.Version.TeamName); err != nil {
		return nil, err
	}
	branchID, cutoff := o.Tree.ActiveBranch, ""
	if target != "" {
		branchID, cutoff = o.Tree.ResolveTarget(target, o.Events)
	}
	lineage, err := ProjectValidatedEventsForBranch(events, o.Tree, branchID)
	if err != nil {
		return nil, err
	}
	if cutoff != "" {
		index := -1
		for i, event := range lineage {
			if event.ID == cutoff {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, fmt.Errorf("derived fork cutoff is outside the source branch lineage")
		}
		lineage = lineage[:index+1]
	}
	if err := validateDerivedForkLineage(lineage, o.Tree, branchID); err != nil {
		return nil, err
	}
	if _, err := o.Events.validateHistoricalResultGrantProjection(ctx, lineage); err != nil {
		return nil, err
	}
	if err := validateDerivedForkSourceSafety(lineage, o.Version.TeamName); err != nil {
		return nil, err
	}
	if len(lineage) == 0 || len(events) == 0 {
		return nil, fmt.Errorf("derived fork has no canonical source event")
	}
	activeHead, _, exists, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, o.Tree.ActiveBranch)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, versionstore.ErrWorkspaceSnapshotUnavailable
	}
	base, _, err := o.resolveForkBase(ctx, target, activeHead)
	if err != nil {
		return nil, err
	}
	if base.WorkspaceID != o.Version.WorkspaceID || base.State != versionstore.StatePublished || !base.Materializable {
		return nil, fmt.Errorf("%w: derived source must be a published materializable snapshot", versionstore.ErrWorkspaceSnapshotUnavailable)
	}
	if err := o.Store.Verify(ctx, base.ID); err != nil {
		return nil, err
	}
	provenance, err := o.SnapshotProvenance(ctx, base)
	if err != nil {
		return nil, err
	}
	source := lineage[len(lineage)-1]
	prepared := &PreparedDerivedFork{owner: o, active: o.Tree.ActiveBranch,
		headID: events[len(events)-1].ID, headHash: events[len(events)-1].Hash, snapshot: base,
		subjectRoot: o.Version.SubjectRoot, projectID: o.Version.ProjectID, stateDir: o.Version.StateDir,
		intent: DerivedForkIntent{SchemaVersion: derivedForkIntentVersion, Workspace: o.Workspace,
			WorkspaceID: o.Version.WorkspaceID, Team: o.Version.TeamName, SourceBranchID: branchID,
			SourceEventID: source.ID, SourceEventHash: source.Hash, SourceSnapshotID: string(base.ID),
			SourceSnapshotEventID: base.CommitEventID, SourceRootTreeHash: base.RootTreeHash,
			CaptureOrigin: provenance.CaptureState, CaptureSnapshotID: provenance.CaptureSnapshot, CaptureEventID: provenance.CaptureEventID,
			TaskIDFloor: workflowTaskIDHighWater(lineage)}}
	if err := prepared.bindSnapshotSources(lineage); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (o *WorkspaceSessionOps) validateDerivedForkScope() error {
	if o == nil || !o.Version.Required() || !o.Version.HoldsProjectLock || o.Version.ProjectID == "" ||
		o.Workspace != o.Version.ControlRoot || !historicalGrantCanonicalName(o.Version.TeamName) ||
		o.Events == nil || o.Events.store == nil || o.Tree == nil || o.Store == nil {
		return fmt.Errorf("new-task fork requires a locked Linux managed workspace in required versioning mode")
	}
	if filepath.Dir(o.Events.path) != o.Workspace {
		return fmt.Errorf("derived fork event store belongs to another control workspace")
	}
	_, err := o.Version.ExcludeSubtrees()
	return err
}

func (o *WorkspaceSessionOps) validateDerivedVersionState(ctx context.Context) error {
	state, exists, err := o.Store.GetSubjectState(ctx, o.Version.SubjectRoot)
	if err != nil {
		return err
	}
	if !exists || state.RecoveryRequired || state.CheckpointDeferred {
		return fmt.Errorf("derived fork requires settled workspace recovery and checkpoint state")
	}
	if state.MaterializedWorkspaceID != "" && (state.MaterializedWorkspaceID != o.Version.WorkspaceID || state.MaterializedBranchID != o.Tree.ActiveBranch || state.MaterializedSnapshotID == "") {
		return fmt.Errorf("derived fork materialized ownership differs from the active workspace and branch")
	}
	operations, err := o.Store.IncompleteOperations(ctx)
	if err != nil {
		return err
	}
	if len(operations) != 0 {
		return fmt.Errorf("derived fork requires existing workspace operations to settle first")
	}
	return nil
}

func (o *WorkspaceSessionOps) validateDerivedLiveCheckpoint(ctx context.Context) error {
	data, exists, err := o.Events.store.GetDocument(ctx, runtimestore.DocumentSession)
	if err != nil || !exists {
		return err
	}
	var session SessionData
	if err := json.Unmarshal(data, &session, jsonv1.DefaultOptionsV1()); err != nil {
		return err
	}
	if session.RecoveryRequired || session.PendingTerminalCommit != nil {
		return fmt.Errorf("derived fork requires the live session's recovery and terminal commit to settle first")
	}
	for _, item := range session.Tasks {
		if item != nil && item.CheckpointPause {
			return fmt.Errorf("derived fork cannot discard a checkpoint pause")
		}
	}
	return nil
}

func validateDerivedForkSourceSafety(lineage []RunEvent, expectedTeam string) error {
	var teamName string
	for _, event := range lineage {
		if event.Type != string(EventRunStarted) {
			continue
		}
		var payload struct {
			Team string `json:"team"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		teamName = strings.ToLower(strings.TrimSpace(payload.Team))
	}
	if teamName != expectedTeam {
		return fmt.Errorf("derived fork source has no matching canonical team identity")
	}
	if err := validateWorkflowCycleLineage(lineage); err != nil {
		return err
	}
	if _, err := ReplayTodoList(lineage); err != nil {
		return err
	}
	return validateDerivedForkSessionSafety(ReduceToSessionData(lineage))
}

func (prepared *PreparedDerivedFork) bindSnapshotSources(lineage []RunEvent) error {
	var snapshotFound, captureFound bool
	for _, event := range lineage {
		if event.ID == prepared.intent.SourceSnapshotEventID {
			payload, ok := decodeWorkspaceSnapshotEvent(event)
			if !ok || payload.SnapshotID != prepared.intent.SourceSnapshotID || payload.RootTreeHash != prepared.intent.SourceRootTreeHash ||
				payload.ManifestDigest != prepared.snapshot.ManifestDigest || !payload.Materializable {
				return ErrDerivedForkIntent
			}
			prepared.intent.SourceSnapshotEventHash, snapshotFound = event.Hash, true
		}
		if prepared.intent.CaptureOrigin == "known" && event.ID == prepared.intent.CaptureEventID {
			payload, ok := decodeWorkspaceSnapshotEvent(event)
			if !ok || payload.SnapshotID != prepared.intent.CaptureSnapshotID || payload.RootTreeHash != prepared.intent.SourceRootTreeHash {
				return ErrDerivedForkIntent
			}
			prepared.intent.CaptureEventHash, captureFound = event.Hash, true
		}
	}
	if !snapshotFound || prepared.intent.CaptureOrigin == "known" && !captureFound {
		return fmt.Errorf("derived fork snapshot publication or capture is outside the selected lineage")
	}
	return nil
}
