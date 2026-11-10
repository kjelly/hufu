package team

import (
	"context"
	"slices"

	"github.com/kjelly/hufu/internal/media"
)

// StageImageInput validates an explicit host-selected image without creating
// CAS or history. Existing refs account for this process-local pending bundle;
// they are quota inputs and confer no source or provider authority.
func (c *Coordinator) StageImageInput(ctx context.Context, path string, existing []media.Ref) (*media.StagedImage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return nil, err
	}
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		return nil, media.Reject(media.Unsupported, "image staging does not yet support a phase workflow")
	}
	if _, err := c.coordinatorImageTarget(); err != nil {
		return nil, err
	}
	limits := policy.Policy.Image
	if len(existing) >= limits.MaxCount {
		return nil, media.Reject(media.LimitExceeded, "pending image count exceeds policy")
	}
	if len(existing) > 0 {
		if err := media.CheckBundle(existing, limits); err != nil {
			return nil, err
		}
	}
	remaining := limits.MaxBytesPerBundle
	for _, ref := range existing {
		remaining -= ref.ByteSize
	}
	if remaining <= 0 {
		return nil, media.Reject(media.LimitExceeded, "pending image bundle exceeds policy")
	}
	if remaining < limits.MaxBytesPerImage {
		limits.MaxBytesPerImage = remaining
	}
	staged, err := media.StageImage(ctx, path, limits)
	if err != nil {
		return nil, err
	}
	refs := append(slices.Clone(existing), staged.Ref())
	if err := media.CheckBundle(refs, policy.Policy.Image); err != nil {
		staged.Close()
		return nil, err
	}
	return staged, nil
}
