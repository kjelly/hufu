package team

import "context"

// Context overflow observation is kept outside the task runner so the runner
// only reports the provider event at its existing failure branch.

func (c *Coordinator) observeContextOverflow(ctx context.Context, modelID string, window int) error {
	if c == nil || window <= 0 {
		return nil
	}
	if c.modelProfileRuntime == nil {
		// Direct old unit constructors have no provider-bound runtime service;
		// retain their compatibility behavior through the legacy registry.
		GlobalModelSpecRegistry().RegisterObservedContextWindow(modelID, window)
		return nil
	}
	return c.modelProfileRuntime.ObserveOverflow(ctx, modelID, window)
}

func (c *Coordinator) invalidateContextProfile(modelID string) error {
	if c == nil || c.modelProfileRuntime == nil || c.modelProfileRuntime.manager == nil {
		return nil
	}
	ref, err := c.modelProfileRuntime.manager.ResolveProviderRef(modelID)
	if err != nil {
		return err
	}
	ref.NoNet = c.modelProfileRuntime.noNet
	return c.modelProfileRuntime.resolver.InvalidateProfile(ref, modelID)
}
