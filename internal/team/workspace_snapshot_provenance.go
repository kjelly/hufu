package team

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceSnapshotProvenance distinguishes publication of a derived node
// from the capture of its file tree. Restore v1 does not retain its source
// node, so its original capture is explicitly unavailable.
type WorkspaceSnapshotProvenance struct {
	SnapshotEventID string `json:"workspace_snapshot_event_id,omitempty"`
	SnapshotReason  string `json:"workspace_snapshot_reason,omitempty"`
	CaptureSnapshot string `json:"workspace_capture_snapshot_id,omitempty"`
	CaptureEventID  string `json:"workspace_capture_event_id,omitempty"`
	CaptureState    string `json:"workspace_capture_origin,omitempty"`
}

// SnapshotProvenance reads metadata only. Fork nodes point to the source tree
// through their parent; restore nodes cannot make that same assertion.
func (o *WorkspaceSessionOps) SnapshotProvenance(ctx context.Context, snapshot versionstore.Snapshot) (WorkspaceSnapshotProvenance, error) {
	result := WorkspaceSnapshotProvenance{
		SnapshotEventID: snapshot.CommitEventID, SnapshotReason: string(snapshot.Reason), CaptureState: "unavailable",
	}
	seen := make(map[versionstore.SnapshotID]bool)
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if seen[snapshot.ID] || snapshot.WorkspaceID != o.Version.WorkspaceID {
			return result, fmt.Errorf("invalid snapshot provenance for %s", snapshot.ID)
		}
		seen[snapshot.ID] = true
		if snapshot.Reason == versionstore.SnapshotRestore {
			return result, nil
		}
		if snapshot.Reason != versionstore.SnapshotFork {
			if snapshot.CommitEventID != "" {
				result.CaptureSnapshot, result.CaptureEventID = string(snapshot.ID), snapshot.CommitEventID
				result.CaptureState = "known"
			}
			return result, nil
		}
		if snapshot.Parent == "" {
			return result, nil
		}
		var err error
		snapshot, err = o.Store.GetSnapshot(ctx, snapshot.Parent)
		if err != nil {
			return result, err
		}
	}
}
