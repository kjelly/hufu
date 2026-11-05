package team

import (
	"context"
	"reflect"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

type historyReferenceLocation struct {
	Message   int
	Part      int
	Reference media.HistoryReference
}

func historyReferences(messages []fantasy.Message) []historyReferenceLocation {
	var references []historyReferenceLocation
	for index, message := range messages {
		for partIndex, part := range message.Content {
			switch p := part.(type) {
			case agent.MediaReferencePart:
				references = append(references, historyReferenceLocation{index, partIndex, p.Reference})
			case *agent.MediaReferencePart:
				if p != nil {
					references = append(references, historyReferenceLocation{index, partIndex, p.Reference})
				}
			}
		}
	}
	return references
}

// This verifies references, not a new read grant. A provider compiler still
// needs its current occurrence lease and resolved destination authorization.
func validateMediaHistorySources(ctx context.Context, workspace, branch string, messages []fantasy.Message) error {
	return validateMediaHistorySourceSet(ctx, workspace, branch, messages, true)
}

func validateMediaHistoryMetadata(ctx context.Context, workspace, branch string, messages []fantasy.Message) error {
	return validateMediaHistorySourceSet(ctx, workspace, branch, messages, false)
}

func validateMediaHistorySourceSet(ctx context.Context, workspace, branch string, messages []fantasy.Message, verifyContent bool) error {
	hasReferences, err := agent.ValidatePersistentMessages(messages)
	if err != nil || !hasReferences {
		return err
	}
	events, err := readValidatedEventJournal(ctx, workspace)
	if err != nil {
		return err
	}
	tree, err := LoadSessionTreeContext(ctx, workspace)
	if err != nil {
		return err
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, branch)
	if err != nil {
		return err
	}
	return validateMediaHistoryLineage(ctx, workspace, lineage, messages, verifyContent)
}

func validateMediaHistoryLineage(ctx context.Context, workspace string, lineage []RunEvent, messages []fantasy.Message, verifyContent bool) error {
	hasReferences, err := agent.ValidatePersistentMessages(messages)
	if err != nil || !hasReferences {
		return err
	}
	snapshots := make(map[string]media.InputSnapshot)
	bindings := make(map[string]media.TaskBinding)
	for _, event := range lineage {
		switch EventType(event.Type) {
		case EventMediaInputAdmitted:
			snapshot, err := decodeMediaInputEvent(event)
			if err != nil {
				return err
			}
			snapshots[snapshot.ID] = snapshot
		case EventTaskMediaBound:
			payload, err := decodeTaskMediaEvent(event)
			if err != nil {
				return err
			}
			bindings[event.TaskID] = payload.Binding
		}
	}
	tasks, err := ReplayTodoList(lineage)
	if err != nil {
		return err
	}
	store, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		return err
	}
	verified := make(map[string]media.Ref)
	for _, location := range historyReferences(messages) {
		reference := location.Reference
		snapshot, found := snapshots[reference.Source.SnapshotID]
		if !found || snapshot.Hash != reference.Source.SnapshotHash || snapshot.TurnID != reference.Source.TurnID || snapshot.PolicyHash != reference.PolicyHash || reference.Source.PartIndex >= len(snapshot.Input.Parts) {
			return media.Reject(media.AccessDenied, "history image source is not admitted in this lineage")
		}
		part := snapshot.Input.Parts[reference.Source.PartIndex]
		if part.Type != "image" || part.MediaID != reference.Ref.ID || !snapshotHasImage(snapshot, reference.Ref) {
			return media.Reject(media.IntegrityMismatch, "history image differs from its canonical source")
		}
		if err := validateHistoryReferenceOwner(reference, tasks, bindings); err != nil {
			return err
		}
		if old, ok := verified[reference.Ref.ID]; ok {
			if old != reference.Ref {
				return media.Reject(media.IntegrityMismatch, "history has conflicting image metadata")
			}
			continue
		}
		if err := verifyHistoryImage(ctx, store, reference.Ref, verifyContent); err != nil {
			return err
		}
		verified[reference.Ref.ID] = reference.Ref
	}
	return nil
}

func verifyHistoryImage(ctx context.Context, store *FileArtifactStore, ref media.Ref, verifyContent bool) error {
	metadata, err := store.mediaMetadata(ctx, ref.ID)
	if err != nil {
		return err
	}
	if metadata.Image == nil || *metadata.Image != ref {
		return media.Reject(media.IntegrityMismatch, "history image metadata differs from canonical CAS metadata")
	}
	if verifyContent {
		return store.Verify(ctx, imageArtifactRef(ref))
	}
	return nil
}

func snapshotHasImage(snapshot media.InputSnapshot, ref media.Ref) bool {
	for _, existing := range snapshot.Refs {
		if existing == ref {
			return true
		}
	}
	return false
}

func validateHistoryReferenceOwner(reference media.HistoryReference, tasks []*TodoItem, bindings map[string]media.TaskBinding) error {
	if reference.TaskID == "" {
		return nil
	}
	item := todoItemByID(tasks, reference.TaskID)
	if item == nil || item.MediaBinding == nil || item.MediaBinding.Hash != reference.BindingHash || item.MediaBinding.PolicyHash != reference.PolicyHash {
		return media.Reject(media.AccessDenied, "history image task binding is missing or stale")
	}
	bound, ok := bindings[reference.TaskID]
	if !ok || !reflect.DeepEqual(&bound, item.MediaBinding) {
		return media.Reject(media.AccessDenied, "history image has no canonical task media event")
	}
	for index, selection := range item.MediaBinding.Selections {
		if selection == reference.Source && index < len(item.MediaBinding.Refs) && item.MediaBinding.Refs[index] == reference.Ref {
			return nil
		}
	}
	return media.Reject(media.AccessDenied, "history image is outside its canonical task binding")
}

func canonicalMediaHistoryProjection(workspace string, messages []fantasy.Message) (string, error) {
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil {
		return "", err
	}
	if !exists || state.Version != mediaCompactionStateVersion {
		return "", media.Reject(media.DurabilityRequired, "media history has no canonical checkpoint")
	}
	tree, err := LoadSessionTreeContext(context.Background(), workspace)
	if err != nil {
		return "", err
	}
	branch, err := validateActiveSessionBranch(tree)
	if err != nil {
		return "", err
	}
	checkpoint, ok := state.Branches[branch]
	if !ok || !reflect.DeepEqual(historyReferences(messages), historyReferences(checkpoint.History)) {
		return "", media.Reject(media.BindingConflict, "media history projection differs from the canonical checkpoint")
	}
	if err := validateMediaHistoryMetadata(context.Background(), workspace, branch, messages); err != nil {
		return "", err
	}
	return checkpoint.EventID, nil
}
