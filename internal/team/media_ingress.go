package team

import (
	"context"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/utils"
)

// MediaTurnRequest is trusted host ingress. File paths are consumed only here;
// the durable input and every later attempt contain CAS references instead.
type MediaTurnRequest struct {
	TurnID string
	Images []string
	// StagedImages are borrowed from trusted host staging for this admission.
	// The caller keeps them alive until OnAdmission or the invocation returns.
	StagedImages []*media.StagedImage     `json:"-"`
	OnAdmission  func(MediaTurnAdmission) `json:"-"`
}

func (r MediaTurnRequest) clone() MediaTurnRequest {
	r.Images = slices.Clone(r.Images)
	r.StagedImages = slices.Clone(r.StagedImages)
	return r
}

func (r MediaTurnRequest) imageCount() (int, error) {
	if len(r.Images) > 0 && len(r.StagedImages) > 0 {
		return 0, media.Reject(media.BindingConflict, "image input has multiple source types")
	}
	return len(r.Images) + len(r.StagedImages), nil
}

func (c *Coordinator) prepareSingleTaskMedia(ctx context.Context, task TaskDef, request MediaTurnRequest) (TaskDef, error) {
	return c.prepareTaskMedia(ctx, task, request, media.InvocationSingleTask)
}

func (c *Coordinator) prepareTaskMedia(ctx context.Context, task TaskDef, request MediaTurnRequest, mode media.InvocationMode) (TaskDef, error) {
	if err := ctx.Err(); err != nil {
		return TaskDef{}, err
	}
	if mode != "" {
		if _, err := c.nativeImageWorkerDefinition(task.Agent); err != nil {
			return TaskDef{}, err
		}
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return TaskDef{}, err
	}
	count, err := request.imageCount()
	if err != nil {
		return TaskDef{}, err
	}
	if taskHasMedia(task) || count == 0 || count > policy.Policy.Image.MaxCount || len(task.Goal) > 256<<10 {
		return TaskDef{}, media.Reject(media.BindingConflict, "invalid single-task media input")
	}
	input, err := c.admitMediaTurnInput(ctx, task.Goal, request)
	if err != nil {
		return TaskDef{}, err
	}
	task.Goal = input.Input.Parts[0].Text
	var selections []media.Selection
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
		InvocationMode: mode, PolicyHash: input.PolicyHash, Selections: selections, Refs: refs}).Seal()
	if err != nil {
		return TaskDef{}, err
	}
	if err := c.persistMediaInputHistoryWithAdmission(ctx, input, request.OnAdmission); err != nil {
		return TaskDef{}, err
	}
	task.MediaBinding = &binding
	task.MediaInputDigest = binding.InputDigest
	return task, nil
}

// admitMediaTurnInput is shared trusted ingress for direct work and
// coordinator turns. It publishes a canonical snapshot; callers commit its
// history before exposing a turn to a provider.
func (c *Coordinator) admitMediaTurnInput(ctx context.Context, goal string, request MediaTurnRequest) (media.InputSnapshot, error) {
	request = request.clone()
	if err := ctx.Err(); err != nil {
		return media.InputSnapshot{}, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return media.InputSnapshot{}, err
	}
	count, err := request.imageCount()
	if err != nil {
		return media.InputSnapshot{}, err
	}
	if count == 0 || count > policy.Policy.Image.MaxCount || len(goal) > 256<<10 {
		return media.InputSnapshot{}, media.Reject(media.BindingConflict, "invalid media turn input")
	}
	if c.currentInvocationID() == "" {
		c.SetInvocationID(NewInvocationID())
	}
	turnID := request.TurnID
	if turnID == "" {
		turnID = newRuntimeIdentity("turn")
	}
	goal = utils.RedactSecrets(goal)
	input := media.InputSnapshot{RunID: c.executionRunID, BranchID: c.activeBranchID(), InvocationID: c.currentInvocationID(), TurnID: turnID, PolicyHash: policy.Hash,
		Input: media.TurnInput{Parts: []media.Part{{Type: "text", Text: goal}}}}
	seen := make(map[string]bool)
	var bundleBytes int64
	for imageIndex := range count {
		if err := ctx.Err(); err != nil {
			return media.InputSnapshot{}, err
		}
		remaining := policy.Policy.Image.MaxBytesPerBundle - bundleBytes
		if remaining <= 0 {
			return media.InputSnapshot{}, media.Reject(media.LimitExceeded, "image bundle exceeds byte budget")
		}
		limits := policy.Policy.Image
		if remaining < limits.MaxBytesPerImage {
			limits.MaxBytesPerImage = remaining
		}
		ref, err := c.admitMediaTurnImage(ctx, request, imageIndex, limits)
		if err != nil {
			return media.InputSnapshot{}, err
		}
		bundleBytes += ref.ByteSize
		index := len(input.Input.Parts)
		input.Input.Parts = append(input.Input.Parts, media.Part{Type: "image", MediaID: ref.ID})
		input.Occurrences = append(input.Occurrences, media.Occurrence{RunID: input.RunID, BranchID: input.BranchID, InvocationID: input.InvocationID, TurnID: input.TurnID, PartIndex: index, MediaID: ref.ID, Origin: "user_file"})
		if !seen[ref.ID] {
			input.Refs = append(input.Refs, ref)
			seen[ref.ID] = true
		}
	}
	input, err = media.SealSnapshot(input, policy.Policy.Image)
	if err != nil {
		return media.InputSnapshot{}, err
	}
	input, err = c.admitMediaInput(ctx, input)
	if err != nil {
		return media.InputSnapshot{}, err
	}
	return input, nil
}

func (c *Coordinator) rejectMediaTaskExecution(taskID string) error {
	err := media.Reject(media.Unsupported, "native image execution is not enabled")
	return c.blockMediaTaskExecution(taskID, err)
}

func (c *Coordinator) blockMediaTaskExecution(taskID string, err error) error {
	if c == nil || c.taskTracker == nil {
		return err
	}
	if item := c.todoItemByID(taskID); item != nil && !isTerminalTaskStatus(item.Status) {
		class := ClassifyTaskFailureStructured(FailureClassificationInput{Err: err})
		status := TaskBlocked
		if class == FailureCancelled || class == FailureTimeout {
			status = TaskError
		}
		if persistErr := c.PersistFailureWithClassAndStatusError(item.Agent, item.Goal, taskID, err.Error(), RetryNone, class, status); persistErr != nil {
			return persistErr
		}
	}
	return err
}
