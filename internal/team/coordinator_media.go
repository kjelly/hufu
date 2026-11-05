package team

import (
	"context"
	"slices"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

type coordinatorMediaInputKey struct{}
type coordinatorCommittedMediaKey struct{}

// RunWithMedia uses the ordinary coordinator lifecycle. The coordinator sees
// metadata; workers need explicit selections and native provider admission.
func (c *Coordinator) RunWithMedia(ctx context.Context, prompt string, input MediaTurnRequest) (string, error) {
	input.Images = slices.Clone(input.Images)
	return c.Run(context.WithValue(ctx, coordinatorMediaInputKey{}, input), prompt)
}

// ContinueWithMedia retains the existing conversation and commits only this
// turn's new attachments. Prior image turns remain canonical references.
func (c *Coordinator) ContinueWithMedia(ctx context.Context, prompt string, input MediaTurnRequest) (string, error) {
	input.Images = slices.Clone(input.Images)
	return c.ContinueWithPrompt(context.WithValue(ctx, coordinatorMediaInputKey{}, input), prompt)
}

func (c *Coordinator) admitCoordinatorMediaInput(ctx context.Context, prompt string) (context.Context, string, error) {
	input, present := ctx.Value(coordinatorMediaInputKey{}).(MediaTurnRequest)
	if !present {
		return ctx, prompt, nil
	}
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		return ctx, prompt, media.Reject(media.Unsupported, "image coordinator input does not yet support a phase workflow")
	}
	snapshot, err := c.admitMediaTurnInput(ctx, prompt, input)
	if err != nil {
		return ctx, prompt, err
	}
	if err := c.persistMediaInputHistory(ctx, snapshot); err != nil {
		return ctx, prompt, err
	}
	return context.WithValue(ctx, coordinatorCommittedMediaKey{}, snapshot), snapshot.Input.Parts[0].Text, nil
}

func coordinatorMediaPromptCommitted(ctx context.Context, prompt string, history []fantasy.Message) bool {
	snapshot, present := ctx.Value(coordinatorCommittedMediaKey{}).(media.InputSnapshot)
	if !present || len(snapshot.Input.Parts) == 0 || prompt != snapshot.Input.Parts[0].Text {
		return false
	}
	message, err := mediaSnapshotMessage(snapshot)
	if err != nil {
		return false
	}
	found, err := mediaTurnAlreadyRecorded(history, message, snapshot.ID)
	return err == nil && found
}

const coordinatorImageMetadataGuide = `
## Image delegation
Image input parts are metadata only: you have no image pixels and must not claim to inspect their contents. Delegate visual work with agent.tasks[].image_inputs, using only the listed turn_id and part_index pairs in the intended order. Repeated selections are preserved. A worker's image-capability and egress policy must permit the actual request. Never invent paths, CAS IDs, media bindings or image digests. Consume the worker's accepted result and preserve its limitations; metadata alone cannot complete a new image request.
`

const coordinatorMediaContinuationGuide = `
The current image-bearing user turn is an additional message. Continue coordinating and address both the original request and this new input. You may add, modify or cancel pending work under the existing task contracts. Call finish only when the complete response and required worker evidence are available.
`

func (c *Coordinator) requireCoordinatorImageWork(ctx context.Context) error {
	snapshot, present := ctx.Value(coordinatorCommittedMediaKey{}).(media.InputSnapshot)
	if !present {
		return nil
	}
	required := make(map[media.Selection]bool)
	for index, part := range snapshot.Input.Parts {
		if part.Type == "image" {
			required[media.Selection{SnapshotID: snapshot.ID, SnapshotHash: snapshot.Hash, TurnID: snapshot.TurnID, PartIndex: index}] = true
		}
	}
	for _, item := range c.TaskTracker().TodoList().Items() {
		if item.Status != TaskDone || item.MediaBinding == nil || item.ExecutionReceipt == nil ||
			item.ExecutionReceipt.MediaInputDigest != item.MediaInputDigest || c.GetTaskResult(item.ID) == nil {
			continue
		}
		for _, selection := range item.MediaBinding.Selections {
			delete(required, selection)
		}
	}
	if len(required) != 0 {
		return media.Reject(media.DurabilityRequired, "current image input requires completed native worker results; metadata alone cannot finish visual work")
	}
	return nil
}
