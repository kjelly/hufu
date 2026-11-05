package team

import (
	"context"
	"slices"
)

// RunDirectAgent executes a named worker through the canonical direct lifecycle.
func (c *Coordinator) RunDirectAgent(ctx context.Context, agentName, task string) (*DirectAgentResult, error) {
	return c.runDirectAgent(ctx, agentName, task, nil, false)
}

// RunDirectAgentWithMedia admits one trusted input turn for the named worker.
// It shares RunDirectAgent's invocation, task, receipt and acceptance boundaries;
// later retries consume its canonical CAS references instead of source paths.
func (c *Coordinator) RunDirectAgentWithMedia(ctx context.Context, agentName, task string, input MediaTurnRequest) (*DirectAgentResult, error) {
	input.Images = slices.Clone(input.Images)
	return c.runDirectAgent(ctx, agentName, task, &input, false)
}

// RunDirectAgentWithMediaAndSynthesis presents the accepted worker result with
// the coordinator before the original direct invocation is finalized. The
// presentation call has a text-only input and no tools; it cannot grant image
// access, delegate more work, or replace the run's acceptance contract.
func (c *Coordinator) RunDirectAgentWithMediaAndSynthesis(ctx context.Context, agentName, task string, input MediaTurnRequest) (*DirectAgentResult, error) {
	input.Images = slices.Clone(input.Images)
	return c.runDirectAgent(ctx, agentName, task, &input, true)
}
