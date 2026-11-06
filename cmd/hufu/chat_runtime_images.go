package main

import (
	"context"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/media"
)

var _ conversation.InputRuntimePort = (*chatRuntimeAdapter)(nil)
var _ conversation.ImageRuntimePort = (*chatRuntimeAdapter)(nil)

func (a *chatRuntimeAdapter) PendingImageCount() int {
	return len(a.seq.images.status().Images)
}

func (a *chatRuntimeAdapter) ManageImageInputs(ctx context.Context, command conversation.ImageCommand) (conversation.ImageStatus, error) {
	stage := a.stageImage
	if stage == nil {
		stage = func(ctx context.Context, path string, refs []media.Ref) (*media.StagedImage, error) {
			tc, _ := a.Current()
			if tc == nil || tc.coordinator == nil {
				return nil, media.Reject(media.Unsupported, "image staging is unavailable")
			}
			return tc.coordinator.StageImageInput(ctx, path, refs)
		}
	}
	return a.seq.images.manage(ctx, command, stage)
}
