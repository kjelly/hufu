package team

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/utils"
)

// MediaTurnRequest is trusted host ingress. File paths are consumed only here;
// the durable input and every later attempt contain CAS references instead.
type MediaTurnRequest struct {
	TurnID string
	Images []string
}

func (c *Coordinator) prepareSingleTaskMedia(ctx context.Context, task TaskDef, request MediaTurnRequest) (TaskDef, error) {
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return TaskDef{}, err
	}
	if taskHasMedia(task) || len(request.Images) == 0 || len(request.Images) > policy.Policy.Image.MaxCount || len(task.Goal) > 256<<10 {
		return TaskDef{}, media.Reject(media.BindingConflict, "invalid single-task media input")
	}
	if c.currentInvocationID() == "" {
		c.SetInvocationID(NewInvocationID())
	}
	turnID := request.TurnID
	if turnID == "" {
		turnID = newRuntimeIdentity("turn")
	}
	task.Goal = utils.RedactSecrets(task.Goal)
	input := media.InputSnapshot{RunID: c.executionRunID, BranchID: c.activeBranchID(), InvocationID: c.currentInvocationID(), TurnID: turnID, PolicyHash: policy.Hash,
		Input: media.TurnInput{Parts: []media.Part{{Type: "text", Text: task.Goal}}}}
	seen := make(map[string]bool)
	var selections []media.Selection
	var bundleBytes int64
	for _, path := range request.Images {
		if err := ctx.Err(); err != nil {
			return TaskDef{}, err
		}
		absolute, err := filepath.Abs(path)
		if err != nil || path == "" {
			return TaskDef{}, media.Reject(media.InputMissing, "image file is unavailable")
		}
		// The caller explicitly selected this file. The store's existing source
		// boundary is narrowed to that parent; ReadFile still rejects symlinks
		// in every component, including the original parent path.
		store, err := NewFileArtifactStore(c.artifactStoreRootPath(), filepath.Dir(absolute))
		if err != nil {
			return TaskDef{}, media.Reject(media.DurabilityRequired, "private artifact store unavailable")
		}
		remaining := policy.Policy.Image.MaxBytesPerBundle - bundleBytes
		if remaining <= 0 {
			return TaskDef{}, media.Reject(media.LimitExceeded, "image bundle exceeds byte budget")
		}
		limits := policy.Policy.Image
		if remaining < limits.MaxBytesPerImage {
			limits.MaxBytesPerImage = remaining
		}
		put, err := store.Put(ctx, PutArtifactRequest{Kind: "image", SourcePath: absolute, MediaLimits: limits})
		if err != nil {
			return TaskDef{}, err
		}
		bundleBytes += put.ByteSize
		metadata, err := store.mediaMetadata(ctx, put.ID)
		if err != nil {
			return TaskDef{}, err
		}
		index := len(input.Input.Parts)
		input.Input.Parts = append(input.Input.Parts, media.Part{Type: "image", MediaID: put.ID})
		input.Occurrences = append(input.Occurrences, media.Occurrence{RunID: input.RunID, BranchID: input.BranchID, InvocationID: input.InvocationID, TurnID: input.TurnID, PartIndex: index, MediaID: put.ID, Origin: "user_file"})
		if !seen[put.ID] {
			input.Refs = append(input.Refs, *metadata.Image)
			seen[put.ID] = true
		}
	}
	input, err = media.SealSnapshot(input, policy.Policy.Image)
	if err != nil {
		return TaskDef{}, err
	}
	input, err = c.admitMediaInput(ctx, input)
	if err != nil {
		return TaskDef{}, err
	}
	var refs []media.Ref
	for index, p := range input.Input.Parts {
		if p.Type != "image" {
			continue
		}
		selections = append(selections, media.Selection{SnapshotID: input.ID, SnapshotHash: input.Hash, TurnID: input.TurnID, PartIndex: index})
		for _, r := range input.Refs {
			if r.ID == p.MediaID {
				refs = append(refs, r)
				break
			}
		}
	}
	binding, err := (media.TaskBinding{Version: media.Version, RunID: input.RunID, BranchID: input.BranchID, Agent: strings.ToLower(strings.TrimSpace(task.Agent)), Purpose: "image_analysis",
		PolicyHash: input.PolicyHash, Selections: selections, Refs: refs}).Seal()
	if err != nil {
		return TaskDef{}, err
	}
	if err := c.persistMediaInputHistory(ctx, input); err != nil {
		return TaskDef{}, err
	}
	task.MediaBinding = &binding
	task.MediaInputDigest = binding.InputDigest
	return task, nil
}

func (c *Coordinator) rejectMediaTaskExecution(task TaskDef, taskID string) error {
	err := media.Reject(media.Unsupported, "native image execution is not enabled")
	if c == nil || c.taskTracker == nil {
		return err
	}
	if item := c.todoItemByID(taskID); item != nil && !isTerminalTaskStatus(item.Status) {
		if persistErr := c.PersistFailureWithClassAndStatusError(task.Agent, task.Goal, taskID, err.Error(), RetryNone, FailurePolicy, TaskBlocked); persistErr != nil {
			return persistErr
		}
	}
	return err
}
