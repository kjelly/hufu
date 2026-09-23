package team

import (
	"context"
	"fmt"
	"sort"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceDoctorReport is the result of DoctorWorkspace.
type WorkspaceDoctorReport struct {
	Issues   []versionstore.DoctorIssue `json:"issues"`
	Repaired []string                   `json:"repaired,omitempty"`
}

// HasErrors reports whether any issue is an error.
func (r WorkspaceDoctorReport) HasErrors() bool {
	for _, issue := range r.Issues {
		if issue.Severity == "error" {
			return true
		}
	}
	return false
}

// lastOwnWorkspaceSnapshot returns the last snapshot committed on branchID
// itself (not inherited from a parent before the fork).
func lastOwnWorkspaceSnapshot(events []RunEvent, branchID string) (versionstore.SnapshotID, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if effectiveEventBranchID(events[index]) != branchID {
			continue
		}
		if payload, ok := decodeWorkspaceSnapshotEvent(events[index]); ok {
			return versionstore.SnapshotID(payload.SnapshotID), true
		}
	}
	return "", false
}

// DoctorWorkspace runs the store checks plus the ones that need this
// workspace's event log and session tree (§29): every published snapshot has
// a durable commit event, every head equals its branch's latest own commit
// in the event lineage, branches without heads are reported as legacy, and
// the active branch's live files are compared with its head. With repair it
// performs only deterministic repairs: settling pending snapshots and
// incomplete operations, resetting heads to the lineage, and removing stale
// temp files. It never invents content or picks a newer snapshot.
func DoctorWorkspace(ctx context.Context, o *WorkspaceSessionOps, repair bool) (WorkspaceDoctorReport, error) {
	var report WorkspaceDoctorReport
	if repair {
		recovered, err := o.recoverAll(ctx)
		if err != nil {
			return report, fmt.Errorf("repair: %w", err)
		}
		for _, action := range recovered.Snapshots {
			report.Repaired = append(report.Repaired, fmt.Sprintf("snapshot %s %s", action.SnapshotID, action.Kind))
		}
		for _, id := range append(recovered.Completed, recovered.Failed...) {
			report.Repaired = append(report.Repaired, "operation "+string(id)+" settled")
		}
		removed, err := o.Store.RemoveStaleTempFiles()
		if err != nil {
			return report, err
		}
		if removed > 0 {
			report.Repaired = append(report.Repaired, fmt.Sprintf("%d stale temp file(s) removed", removed))
		}
	}
	events, err := o.Events.ReadEvents()
	if err != nil {
		return report, fmt.Errorf("read events: %w", err)
	}
	if repair {
		repaired, repairErr := o.repairHeadsFromLineage(ctx, events)
		if repairErr != nil {
			return report, repairErr
		}
		report.Repaired = append(report.Repaired, repaired...)
	}
	if report.Issues, err = o.Store.Doctor(ctx); err != nil {
		return report, err
	}
	workspaceIssues, err := o.workspaceIssues(ctx, events)
	if err != nil {
		return report, err
	}
	report.Issues = append(report.Issues, workspaceIssues...)
	return report, nil
}

func (o *WorkspaceSessionOps) workspaceIssues(ctx context.Context, events []RunEvent) ([]versionstore.DoctorIssue, error) {
	var issues []versionstore.DoctorIssue
	ws := o.Version.WorkspaceID
	lookup := workspaceCommitLookup(events)
	published, err := o.Store.ListSnapshots(ctx, versionstore.SnapshotFilter{WorkspaceID: ws, State: versionstore.StatePublished})
	if err != nil {
		return nil, err
	}
	for _, snapshot := range published {
		eventID, found, _ := lookup(ctx, snapshot.ID)
		if !found || eventID != snapshot.CommitEventID {
			issues = append(issues, versionstore.DoctorIssue{Code: "commit_event_missing", Severity: "error", WorkspaceID: ws, BranchID: snapshot.BranchID, SnapshotID: snapshot.ID,
				Detail: fmt.Sprintf("published snapshot's commit event %s is not in the event log", snapshot.CommitEventID)})
		}
	}
	branches := make([]string, 0, len(o.Tree.Branches))
	for id := range o.Tree.Branches {
		branches = append(branches, id)
	}
	sort.Strings(branches)
	for _, branchID := range branches {
		want, committed := lastOwnWorkspaceSnapshot(events, branchID)
		head, _, hasHead, headErr := o.Store.GetBranchHead(ctx, ws, branchID)
		if headErr != nil {
			return nil, headErr
		}
		switch {
		case committed && (!hasHead || head.ID != want):
			issues = append(issues, versionstore.DoctorIssue{Code: "head_lineage_mismatch", Severity: "error", WorkspaceID: ws, BranchID: branchID, SnapshotID: want,
				Detail: fmt.Sprintf("head is %q but the event lineage last committed %s", head.ID, want), Repairable: true})
		case !committed && !hasHead:
			issues = append(issues, versionstore.DoctorIssue{Code: "legacy_branch", Severity: "warning", WorkspaceID: ws, BranchID: branchID,
				Detail: "branch has no workspace snapshot (metadata-only)"})
		}
	}
	driftIssue, err := o.liveDriftIssue(ctx)
	if err != nil {
		return nil, err
	}
	if driftIssue != nil {
		issues = append(issues, *driftIssue)
	}
	if _, bound, bindErr := o.Store.GetSubjectState(ctx, o.Version.SubjectRoot); bindErr != nil {
		issues = append(issues, versionstore.DoctorIssue{Code: "subject_root_binding", Severity: "error", Detail: bindErr.Error()})
	} else if !bound && len(published) > 0 {
		issues = append(issues, versionstore.DoctorIssue{Code: "subject_root_rebound", Severity: "error",
			Detail: fmt.Sprintf("snapshots exist but %s is not the recorded subject root (was the project rebound?)", o.Version.SubjectRoot)})
	}
	return issues, nil
}

func (o *WorkspaceSessionOps) liveDriftIssue(ctx context.Context) (*versionstore.DoctorIssue, error) {
	active := o.Tree.ActiveBranch
	head, _, ok, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, active)
	if err != nil || !ok {
		return nil, err
	}
	drift, err := o.LiveDrift(ctx, head)
	if err != nil {
		return &versionstore.DoctorIssue{Code: "live_drift_unknown", Severity: "warning", BranchID: active, Detail: err.Error()}, nil
	}
	if !drift {
		return nil, nil
	}
	return &versionstore.DoctorIssue{Code: "live_drift", Severity: "warning", WorkspaceID: o.Version.WorkspaceID, BranchID: active, SnapshotID: head.ID,
		Detail: "live files differ from the active branch head; the next run admission records them"}, nil
}

// LiveDrift reports whether the subject root's managed files differ from
// head, without writing anything.
func (o *WorkspaceSessionOps) LiveDrift(ctx context.Context, head versionstore.Snapshot) (bool, error) {
	req, err := o.Version.captureRequest(ctx, head.BranchID, "", versionstore.SnapshotManual)
	if err != nil {
		return false, err
	}
	req.WorkspaceID = o.Version.WorkspaceID
	live, err := o.Store.LiveRootTree(ctx, req)
	if err != nil {
		return false, err
	}
	return live != head.RootTreeHash, nil
}

func (o *WorkspaceSessionOps) repairHeadsFromLineage(ctx context.Context, events []RunEvent) ([]string, error) {
	var repaired []string
	for branchID := range o.Tree.Branches {
		want, committed := lastOwnWorkspaceSnapshot(events, branchID)
		if !committed {
			continue
		}
		head, _, hasHead, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, branchID)
		if err != nil {
			return repaired, err
		}
		if hasHead && head.ID == want {
			continue
		}
		snapshot, err := o.Store.GetSnapshot(ctx, want)
		if err != nil || snapshot.State != versionstore.StatePublished {
			continue // not deterministic: leave it reported
		}
		if err = o.Store.RepairBranchHead(ctx, o.Version.WorkspaceID, branchID, want); err != nil {
			return repaired, err
		}
		repaired = append(repaired, fmt.Sprintf("head of %s reset to %s from the event lineage", branchID, want))
	}
	sort.Strings(repaired)
	return repaired, nil
}

// WorkspaceVersionStatus is `hufu workspace version status` (§32.2).
type WorkspaceVersionStatus struct {
	SubjectRoot             string         `json:"subject_root"`
	EffectiveMode           string         `json:"effective_mode"`
	ModeFloor               string         `json:"mode_floor"`
	PlatformSupported       bool           `json:"platform_supported"`
	ActiveWorkspaceID       string         `json:"active_workspace_id"`
	ActiveBranchID          string         `json:"active_branch_id"`
	ActiveHeadSnapshotID    string         `json:"active_head_snapshot_id,omitempty"`
	MaterializedWorkspaceID string         `json:"materialized_workspace_id,omitempty"`
	MaterializedBranchID    string         `json:"materialized_branch_id,omitempty"`
	MaterializedSnapshotID  string         `json:"materialized_snapshot_id,omitempty"`
	LiveDrift               *bool          `json:"live_drift,omitempty"`
	RecoveryRequired        bool           `json:"recovery_required"`
	RecoveryCode            string         `json:"recovery_code,omitempty"`
	RecoveryDetail          string         `json:"recovery_detail,omitempty"`
	CheckpointDeferred      bool           `json:"checkpoint_deferred"`
	IncompleteOperations    []string       `json:"incomplete_operations"`
	SnapshotCounts          map[string]int `json:"snapshot_counts"`
	CASObjects              int            `json:"cas_objects"`
	CASBytes                int64          `json:"cas_bytes"`
	// Commit metrics projected from this workspace's commit events (§35).
	Commits        map[string]int `json:"commits_by_reason"`
	NewCASBytes    int64          `json:"new_cas_bytes"`
	ReusedCASBytes int64          `json:"reused_cas_bytes"`
}

// Status assembles the status view from the store, events, and session tree.
func (o *WorkspaceSessionOps) Status(ctx context.Context) (WorkspaceVersionStatus, error) {
	status := WorkspaceVersionStatus{
		SubjectRoot: o.Version.SubjectRoot, EffectiveMode: string(o.Version.Mode), PlatformSupported: versionstore.PlatformSupported(),
		ActiveWorkspaceID: o.Version.WorkspaceID, ActiveBranchID: o.Tree.ActiveBranch,
		SnapshotCounts: map[string]int{}, Commits: map[string]int{}, IncompleteOperations: []string{},
	}
	if status.EffectiveMode == "" {
		status.EffectiveMode = string(versionstore.ModeOff)
	}
	state, ok, err := o.Store.GetSubjectState(ctx, o.Version.SubjectRoot)
	if err != nil {
		return status, err
	}
	if ok {
		status.ModeFloor = string(state.ModeFloor)
		status.MaterializedWorkspaceID, status.MaterializedBranchID = state.MaterializedWorkspaceID, state.MaterializedBranchID
		status.MaterializedSnapshotID = string(state.MaterializedSnapshotID)
		status.RecoveryRequired, status.RecoveryCode, status.RecoveryDetail = state.RecoveryRequired, state.RecoveryCode, state.RecoveryDetail
		status.CheckpointDeferred = state.CheckpointDeferred
	}
	head, _, hasHead, err := o.Store.GetBranchHead(ctx, o.Version.WorkspaceID, o.Tree.ActiveBranch)
	if err != nil {
		return status, err
	}
	if hasHead {
		status.ActiveHeadSnapshotID = string(head.ID)
		if drift, driftErr := o.LiveDrift(ctx, head); driftErr == nil {
			status.LiveDrift = &drift
		}
	}
	ops, err := o.Store.IncompleteOperations(ctx)
	if err != nil {
		return status, err
	}
	for _, op := range ops {
		status.IncompleteOperations = append(status.IncompleteOperations, fmt.Sprintf("%s %s %s", op.ID, op.Kind, op.State))
	}
	snapshots, err := o.Store.ListSnapshots(ctx, versionstore.SnapshotFilter{WorkspaceID: o.Version.WorkspaceID})
	if err != nil {
		return status, err
	}
	for _, snapshot := range snapshots {
		status.SnapshotCounts[string(snapshot.State)]++
	}
	if status.CASObjects, status.CASBytes, err = o.Store.StoreUsage(); err != nil {
		return status, err
	}
	events, err := o.Events.ReadEvents()
	if err != nil {
		return status, err
	}
	for _, event := range events {
		if payload, isCommit := decodeWorkspaceSnapshotEvent(event); isCommit {
			status.Commits[payload.Reason]++
			status.NewCASBytes += payload.NewCASBytes
			status.ReusedCASBytes += payload.ReusedCASBytes
		}
	}
	return status, nil
}

// PinnedWorkspaceSnapshots returns the workspace snapshots that session
// labels pin (GC roots, §28.1): a label on an event pins the snapshot at that
// event; labels on branches need nothing extra because heads are roots.
func PinnedWorkspaceSnapshots(o *WorkspaceSessionOps) ([]versionstore.SnapshotID, error) {
	events, err := o.Events.ReadEvents()
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	var pinned []versionstore.SnapshotID
	for _, target := range o.Tree.Labels {
		branchID, eventID := o.Tree.ResolveTarget(target, o.Events)
		if eventID == "" {
			continue
		}
		id, found, resolveErr := workspaceSnapshotAtEvent(events, o.Tree, branchID, eventID)
		if resolveErr == nil && found {
			pinned = append(pinned, id)
		}
	}
	return pinned, nil
}
