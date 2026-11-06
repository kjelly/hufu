package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/kjelly/hufu/internal/media"
)

// Media ownership is a branch-local occurrence relationship, never metadata
// from the blob's first publisher. This store API is a trusted inspection API;
// model-facing opens still need the coordinator's current attempt grant.
func (s *FileArtifactStore) listTaskMedia(ctx context.Context, taskID string) ([]ArtifactRef, error) {
	if taskID == "" {
		return nil, nil
	}
	if _, err := os.Lstat(filepath.Join(s.root, "media")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	workspace := filepath.Dir(filepath.Dir(s.root))
	events, err := readValidatedEventJournal(ctx, workspace)
	if err != nil {
		return nil, err
	}
	tree, err := LoadSessionTreeContext(ctx, workspace)
	if err != nil {
		return nil, err
	}
	branch, err := validateActiveSessionBranch(tree)
	if err != nil {
		return nil, err
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, branch)
	if err != nil {
		return nil, err
	}
	var binding *media.TaskBinding
	for _, event := range lineage {
		if event.Type != string(EventTaskMediaBound) || event.TaskID != taskID {
			continue
		}
		payload, err := decodeTaskMediaEvent(event)
		if err != nil {
			return nil, err
		}
		binding = payload.Binding.Clone()
	}

	items := ReduceToTodoList(lineage)
	item := todoItemByID(items, taskID)
	if item == nil {
		return nil, nil
	}
	if !reflect.DeepEqual(binding, item.MediaBinding) || binding != nil && binding.InputDigest != item.MediaInputDigest {
		return nil, media.Reject(media.BindingConflict, "task media index differs from occurrence")
	}
	view := mediaLineageView{snapshots: make(map[string]media.InputSnapshot)}
	for _, e := range lineage {
		if e.Type == string(EventMediaInputAdmitted) {
			snapshot, err := decodeMediaInputEvent(e)
			if err != nil {
				return nil, err
			}
			view.snapshots[snapshot.ID] = snapshot
		}
	}
	if binding != nil {
		if err := mediaBindingSources(binding, view, media.Limits{}); err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool)
	var refs []ArtifactRef
	var images []media.Ref
	if binding != nil {
		images = append(images, binding.Refs...)
	}
	for index, event := range lineage {
		if event.Type != string(EventToolMediaPublished) || event.TaskID != taskID {
			continue
		}
		publication, err := decodeToolMediaPublication(event)
		if err != nil {
			return nil, err
		}
		if err := verifyToolMediaPublication(event, publication, lineage[:index]); err != nil {
			return nil, err
		}
		images = append(images, publication.Snapshot.Refs...)
	}
	for _, image := range images {
		if seen[image.ID] {
			continue
		}
		seen[image.ID] = true
		ref, err := s.Resolve(ctx, imageArtifactRef(image))
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func compareMediaInputSnapshotProjection(live, replayed []MediaInputSnapshot) error {
	if len(live) != len(replayed) {
		return media.Reject(media.BindingConflict, "media snapshot projection count differs from journal")
	}
	for i, s := range live {
		if err := s.Validate(media.Limits{}); err != nil {
			return err
		}
		if !reflect.DeepEqual(s, replayed[i]) {
			return media.Reject(media.BindingConflict, "media snapshot projection differs from journal")
		}
	}
	return nil
}
