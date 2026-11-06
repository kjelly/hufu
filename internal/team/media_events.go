package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/media"
)

type MediaInputSnapshot = media.InputSnapshot

func addTaskMediaPayload(payload map[string]any, item *TodoItem) {
	if item.MediaBinding != nil {
		payload["media_binding"] = item.MediaBinding.Clone()
		payload["media_input_digest"] = item.MediaInputDigest
	}
}

func (c *Coordinator) mediaInputSemaphore() chan struct{} {
	if c.modelFanout != nil && c.modelFanout.parent != nil {
		return c.modelFanout.parent.mediaInputSemaphore()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mediaAdmissionGate == nil {
		c.mediaAdmissionGate = make(chan struct{}, 1)
	}
	return c.mediaAdmissionGate
}

type TaskMediaBoundPayload struct {
	Version            int               `json:"version"`
	OccurrenceRevision int               `json:"occurrence_revision"`
	Binding            media.TaskBinding `json:"binding"`
}

func validateTaskMediaEventFields(e RunEvent) error {
	if !strings.HasPrefix(e.Type, "task_") {
		return nil
	}
	// Preserve legacy/forward-compatible task payloads. Strict media parsing
	// starts only when the new contract fields are actually present.
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(e.Payload, &fields, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil
	}
	_, hasBinding := fields["media_binding"]
	_, hasDigest := fields["media_input_digest"]
	if !hasBinding && !hasDigest {
		return nil
	}
	var payload struct {
		Agent    string             `json:"agent"`
		Binding  *media.TaskBinding `json:"media_binding"`
		Digest   string             `json:"media_input_digest"`
		Receipt  *ExecutionReceipt  `json:"execution_receipt"`
		Receipts []ExecutionReceipt `json:"execution_receipts"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		return err
	}
	if err := validateTaskMediaShape(&TodoItem{ID: e.TaskID, Agent: payload.Agent, MediaBinding: payload.Binding, MediaInputDigest: payload.Digest}); err != nil {
		return err
	}
	_, reset, err := decodeMediaTaskReset(e)
	if err != nil {
		return err
	}
	if payload.Binding != nil && e.Type == string(EventTaskCreated) && !reset && (payload.Binding.RunID != e.RunID || payload.Binding.BranchID != e.BranchID) {
		return media.Reject(media.BindingConflict, "created media owner differs from event identity")
	}
	if payload.Receipt != nil && payload.Receipt.MediaInputDigest != payload.Digest {
		return media.Reject(media.BindingConflict, "media receipt digest differs from its task")
	}
	for _, receipt := range payload.Receipts {
		if receipt.MediaInputDigest != payload.Digest {
			return media.Reject(media.BindingConflict, "media receipt history differs from its task")
		}
	}
	return nil
}

func decodeMediaInputEvent(e RunEvent) (media.InputSnapshot, error) {
	var snapshot media.InputSnapshot
	if e.SchemaVersion < eventStoreSchemaVersion || len(e.Payload) > 512<<10 {
		return snapshot, media.Reject(media.BindingConflict, "invalid media input event envelope")
	}
	if err := json.Unmarshal(e.Payload, &snapshot, json.RejectUnknownMembers(true)); err != nil {
		return snapshot, media.Reject(media.BindingConflict, "invalid media input payload")
	}
	if err := snapshot.Validate(media.Limits{}); err != nil {
		return snapshot, err
	}
	if snapshot.RunID != e.RunID || snapshot.BranchID != e.BranchID || snapshot.InvocationID != e.InvocationID {
		return snapshot, media.Reject(media.BindingConflict, "media input envelope identity mismatch")
	}
	return snapshot, nil
}

func decodeTaskMediaEvent(e RunEvent) (TaskMediaBoundPayload, error) {
	var payload TaskMediaBoundPayload
	if e.SchemaVersion < eventStoreSchemaVersion || len(e.Payload) > 64<<10 {
		return payload, media.Reject(media.BindingConflict, "invalid task media event envelope")
	}
	if err := json.Unmarshal(e.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
		return payload, media.Reject(media.BindingConflict, "invalid task media event payload")
	}
	if payload.Version != media.Version || payload.OccurrenceRevision < 0 || payload.Binding.TaskID != e.TaskID || payload.Binding.RunID != e.RunID || payload.Binding.BranchID != e.BranchID || payload.Binding.Agent != e.Actor {
		return payload, media.Reject(media.BindingConflict, "task media event identity mismatch")
	}
	return payload, payload.Binding.Validate(media.Limits{})
}

func cloneMediaInputSnapshots(snapshots []MediaInputSnapshot) []MediaInputSnapshot {
	if snapshots == nil {
		return nil
	}
	clones := make([]MediaInputSnapshot, len(snapshots))
	for i, s := range snapshots {
		clones[i] = s.Clone()
	}
	return clones
}

func appendMediaInputSnapshot(snapshots []MediaInputSnapshot, snapshot MediaInputSnapshot) []MediaInputSnapshot {
	for _, old := range snapshots {
		if old.ID == snapshot.ID {
			return snapshots
		}
	}
	return append(snapshots, snapshot.Clone())
}

type mediaLineageView struct {
	branch     string
	head       string
	events     []RunEvent
	snapshots  map[string]media.InputSnapshot
	policyHash string
	inputs     []MediaInputSnapshot
}

func (c *Coordinator) mediaLineage(ctx context.Context) (mediaLineageView, error) {
	var view mediaLineageView
	if c == nil || c.session == nil || !c.hasDurableEventJournal() {
		return view, media.Reject(media.DurabilityRequired, "media requires a durable event journal")
	}
	journal := c.EventJournal()
	// Verification refreshes the validated cache from all writers. The global
	// head is retained separately from the filtered branch view for append CAS.
	if err := journal.VerifyHashChain(ctx); err != nil {
		return view, err
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return view, err
	}
	if len(events) > 0 {
		view.head = events[len(events)-1].ID
	}
	owner := c.canonicalJournalOwner()
	tree, err := LoadSessionTreeContext(ctx, owner.session.Workspace)
	if err != nil {
		return view, err
	}
	branch, err := validateActiveSessionBranch(tree)
	if err != nil {
		return view, err
	}
	if c.eventStore != nil && c.eventStore.BranchID() != branch {
		return view, media.Reject(media.AccessDenied, "media journal branch differs from active branch")
	}
	if branch != owner.activeBranchID() {
		return view, media.Reject(media.AccessDenied, "active media branch changed")
	}
	view.branch = branch
	view.events, err = ProjectValidatedEventsForBranch(events, tree, branch)
	if err != nil {
		return view, err
	}
	policy, err := executionPolicySnapshotFromEvents(view.events)
	if err != nil {
		return view, err
	}
	if policy != nil && policy.Media != nil {
		view.policyHash = policy.Media.Hash
	}
	view.snapshots = make(map[string]media.InputSnapshot)
	for _, event := range view.events {
		if event.Type != string(EventMediaInputAdmitted) {
			continue
		}
		snapshot, err := decodeMediaInputEvent(event)
		if err != nil {
			return view, err
		}
		view.snapshots[snapshot.ID] = snapshot
		view.inputs = appendMediaInputSnapshot(view.inputs, snapshot)
	}
	return view, nil
}

// admitMediaInput binds already-published, verified CAS content to one runtime
// turn. No source path, raw bytes, or first-writer authority enters the event.
// Head fencing makes quota validation and the admitted reference projection one
// canonical commit, including with concurrent EventStore handles.
func (c *Coordinator) admitMediaInput(ctx context.Context, snapshot media.InputSnapshot) (media.InputSnapshot, error) {
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return media.InputSnapshot{}, err
	}
	if snapshot.RunID != c.executionRunID || snapshot.InvocationID != c.currentInvocationID() || snapshot.PolicyHash != policy.Hash {
		return media.InputSnapshot{}, media.Reject(media.BindingConflict, "media input does not belong to this invocation")
	}
	if err := snapshot.Validate(policy.Policy.Image); err != nil {
		return media.InputSnapshot{}, err
	}
	for _, occurrence := range snapshot.Occurrences {
		if occurrence.Origin == "mcp_tool" {
			return media.InputSnapshot{}, media.Reject(media.AccessDenied, "tool image sources require atomic producer publication")
		}
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return media.InputSnapshot{}, media.Reject(media.DurabilityRequired, "private artifact store unavailable")
	}
	for _, r := range snapshot.Refs {
		metadata, err := store.mediaMetadata(ctx, r.ID)
		if err != nil {
			return media.InputSnapshot{}, err
		}
		if metadata.Image == nil || *metadata.Image != r {
			return media.InputSnapshot{}, media.Reject(media.IntegrityMismatch, "input metadata differs from private CAS")
		}
		if err := store.Verify(ctx, metadata.ArtifactRef); err != nil {
			return media.InputSnapshot{}, err
		}
	}
	slot, err := acquireSem(ctx, c.mediaInputSemaphore())
	if err != nil {
		return media.InputSnapshot{}, err
	}
	defer slot.release()
	journal, ok := c.EventJournal().(ConditionalEventJournal)
	if !ok {
		return media.InputSnapshot{}, media.Reject(media.DurabilityRequired, "media requires conditional durable append")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return media.InputSnapshot{}, err
	}
	for retry := 0; retry < 16; retry++ {
		if err := ctx.Err(); err != nil {
			return media.InputSnapshot{}, err
		}
		view, err := c.mediaLineage(ctx)
		if err != nil {
			return media.InputSnapshot{}, err
		}
		if snapshot.BranchID != view.branch {
			return media.InputSnapshot{}, media.Reject(media.AccessDenied, "media input branch mismatch")
		}
		if view.policyHash != policy.Hash {
			return media.InputSnapshot{}, media.Reject(media.DurabilityRequired, "media policy is not committed in this lineage")
		}
		for _, old := range view.snapshots {
			if old.RunID == snapshot.RunID && old.InvocationID == snapshot.InvocationID && old.TurnID == snapshot.TurnID {
				if old.Hash != snapshot.Hash {
					return media.InputSnapshot{}, media.Reject(media.BindingConflict, "turn already admitted with different input")
				}
				if err := c.mutateSessionData(func(sd *SessionData) error {
					sd.MediaInputSnapshots = cloneMediaInputSnapshots(view.inputs)
					return nil
				}); err != nil {
					return media.InputSnapshot{}, err
				}
				return old.Clone(), nil
			}
		}
		if err := checkMediaBranchQuota(view.snapshots, snapshot, policy.Policy.Image); err != nil {
			return media.InputSnapshot{}, err
		}
		_, err = journal.AppendAtHead(ctx, RunEvent{Type: string(EventMediaInputAdmitted), Actor: "runtime", RunID: snapshot.RunID, InvocationID: snapshot.InvocationID, BranchID: snapshot.BranchID,
			IdempotencyKey: "media-input:" + snapshot.BranchID + ":" + snapshot.RunID + ":" + snapshot.InvocationID + ":" + snapshot.TurnID, Payload: encoded}, view.head)
		if errors.Is(err, ErrEventHeadChanged) {
			continue
		}
		if err != nil {
			return media.InputSnapshot{}, err
		}
		if err := c.mutateSessionData(func(sd *SessionData) error {
			sd.MediaInputSnapshots = appendMediaInputSnapshot(cloneMediaInputSnapshots(view.inputs), snapshot)
			return nil
		}); err != nil {
			return media.InputSnapshot{}, err
		}
		return snapshot.Clone(), nil
	}
	return media.InputSnapshot{}, fmt.Errorf("media admission: %w", ErrEventHeadChanged)
}

func checkMediaBranchQuota(existing map[string]media.InputSnapshot, addition media.InputSnapshot, limits media.Limits) error {
	l, err := limits.Resolve()
	if err != nil {
		return err
	}
	unique := make(map[string]media.Ref)
	for _, s := range existing {
		for _, r := range s.Refs {
			if old, ok := unique[r.ID]; ok && old != r {
				return media.Reject(media.IntegrityMismatch, "branch has conflicting image metadata")
			}
			unique[r.ID] = r
		}
	}
	for _, r := range addition.Refs {
		if old, ok := unique[r.ID]; ok && old != r {
			return media.Reject(media.IntegrityMismatch, "input conflicts with admitted image metadata")
		}
		unique[r.ID] = r
	}
	var total int64
	for _, r := range unique {
		if err := r.Validate(); err != nil {
			return err
		}
		if r.ByteSize > l.MaxBranchBytes-total {
			return media.Reject(media.LimitExceeded, "branch media byte quota exceeded")
		}
		total += r.ByteSize
	}
	if len(unique) > l.MaxBranchBlobs || total > l.MaxBranchBytes {
		return media.Reject(media.LimitExceeded, "branch media quota exceeded")
	}
	return nil
}

func mediaBindingSources(binding *media.TaskBinding, view mediaLineageView, limits media.Limits) error {
	if err := binding.Validate(limits); err != nil {
		return err
	}
	for index, selection := range binding.Selections {
		s, ok := view.snapshots[selection.SnapshotID]
		if !ok || s.Hash != selection.SnapshotHash || s.TurnID != selection.TurnID || s.PolicyHash != binding.PolicyHash ||
			binding.InvocationMode != media.InvocationDelegatedTask && s.RunID != binding.RunID || selection.PartIndex >= len(s.Input.Parts) {
			return media.Reject(media.AccessDenied, "media source is not admitted for this occurrence")
		}
		part := s.Input.Parts[selection.PartIndex]
		if part.Type != "image" || part.MediaID != binding.Refs[index].ID || !slices.Contains(s.Refs, binding.Refs[index]) {
			return media.Reject(media.BindingConflict, "media selection differs from admitted source")
		}
	}
	return nil
}
