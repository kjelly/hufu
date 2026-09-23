package team

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// WorkspaceSnapshotCommittedPayload is the payload of
// workspace_snapshot_committed. It carries only identities, hashes, and
// counts (never file content).
type WorkspaceSnapshotCommittedPayload struct {
	SnapshotID       string `json:"snapshot_id"`
	ParentSnapshotID string `json:"parent_snapshot_id,omitempty"`
	RootTreeHash     string `json:"root_tree_hash"`
	ManifestDigest   string `json:"manifest_digest"`

	FileCount    int   `json:"file_count"`
	LogicalBytes int64 `json:"logical_bytes"`

	NewCASBytes       int64 `json:"new_cas_bytes"`
	ReusedCASBytes    int64 `json:"reused_cas_bytes"`
	CaptureDurationMS int64 `json:"capture_duration_ms"`

	Reason string `json:"reason"`

	RunID   string `json:"run_id,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`

	Materializable bool `json:"materializable"`
}

// workspaceSnapshotIdempotencyKey is stable per snapshot, so a retried append
// of the same commit is deduplicated by the event store.
func workspaceSnapshotIdempotencyKey(id versionstore.SnapshotID) string {
	return "workspace-snapshot:" + string(id)
}

// appendWorkspaceSnapshotCommitted makes snapshot durable in the event log on
// its own branch. Only after this returns may the snapshot be published.
func appendWorkspaceSnapshotCommitted(ctx context.Context, es *EventStore, snapshot versionstore.Snapshot, stats versionstore.CaptureStats) (RunEvent, error) {
	if es == nil {
		return RunEvent{}, fmt.Errorf("commit workspace snapshot: event store is unavailable")
	}
	payload, err := json.Marshal(WorkspaceSnapshotCommittedPayload{
		SnapshotID: string(snapshot.ID), ParentSnapshotID: string(snapshot.Parent),
		RootTreeHash: snapshot.RootTreeHash, ManifestDigest: snapshot.ManifestDigest,
		FileCount: snapshot.FileCount, LogicalBytes: snapshot.LogicalBytes,
		NewCASBytes: stats.NewCASBytes, ReusedCASBytes: stats.ReusedCASBytes,
		CaptureDurationMS: stats.Duration.Milliseconds(), Reason: string(snapshot.Reason),
		RunID: snapshot.RunID, TaskID: snapshot.TaskID, Attempt: snapshot.Attempt,
		Materializable: snapshot.Materializable,
	})
	if err != nil {
		return RunEvent{}, fmt.Errorf("encode workspace snapshot event: %w", err)
	}
	event, err := es.AppendPersistedContext(ctx, RunEvent{
		RunID: snapshot.RunID, BranchID: snapshot.BranchID, Actor: "workspace-versioning",
		Type: string(EventWorkspaceSnapshotCommitted), IdempotencyKey: workspaceSnapshotIdempotencyKey(snapshot.ID),
		Payload: payload,
	})
	if err != nil {
		return RunEvent{}, fmt.Errorf("commit workspace snapshot %s: %w", snapshot.ID, err)
	}
	return event, nil
}

func decodeWorkspaceSnapshotEvent(event RunEvent) (WorkspaceSnapshotCommittedPayload, bool) {
	if event.Type != string(EventWorkspaceSnapshotCommitted) {
		return WorkspaceSnapshotCommittedPayload{}, false
	}
	var payload WorkspaceSnapshotCommittedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.SnapshotID == "" {
		return WorkspaceSnapshotCommittedPayload{}, false
	}
	return payload, true
}

// workspaceCommitLookup indexes the durable commit events of an event log for
// versionstore recovery.
func workspaceCommitLookup(events []RunEvent) versionstore.CommitEventLookup {
	index := make(map[versionstore.SnapshotID]string)
	for _, event := range events {
		if payload, ok := decodeWorkspaceSnapshotEvent(event); ok {
			index[versionstore.SnapshotID(payload.SnapshotID)] = event.ID
		}
	}
	return func(_ context.Context, id versionstore.SnapshotID) (string, bool, error) {
		eventID, ok := index[id]
		return eventID, ok, nil
	}
}

// workspaceSnapshotAtEvent implements §23: the workspace state of event E is
// the last workspace_snapshot_committed at or before E in E's branch lineage.
// An empty eventID means the end of the lineage. found is false when no such
// snapshot exists (a legacy lineage); callers must fail closed.
func workspaceSnapshotAtEvent(events []RunEvent, st *SessionTree, branchID, eventID string) (versionstore.SnapshotID, bool, error) {
	lineage := FilterEventsForBranch(events, st, branchID)
	end := len(lineage) - 1
	if eventID != "" {
		end = -1
		for index, event := range lineage {
			if event.ID == eventID {
				end = index
				break
			}
		}
		if end < 0 {
			return "", false, fmt.Errorf("event %q is not in the lineage of branch %q", eventID, branchID)
		}
	}
	for index := end; index >= 0; index-- {
		if payload, ok := decodeWorkspaceSnapshotEvent(lineage[index]); ok {
			return versionstore.SnapshotID(payload.SnapshotID), true, nil
		}
	}
	return "", false, nil
}
