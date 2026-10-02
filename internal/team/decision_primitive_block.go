package team

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/decisionrt"
)

// A decision primitive whose catalog entry lists the decided choice in
// block-on stops the calling worker's attempt. The prompt already tells the
// worker to stop; this makes the stop independent of the model: until the
// attempt ends, the worker can still read and report, but it cannot run a
// tool that changes state or submit anything but a blocked result.

type decisionBlockKey struct {
	runID, taskID, agent string
	attempt              int
}

func decisionBlockKeyFor(metadata InvocationMetadata) (decisionBlockKey, bool) {
	if metadata.RunID == "" || metadata.TaskID == "" || metadata.TaskID == CoordTodoID || metadata.AgentName == "" || metadata.Attempt < 1 {
		return decisionBlockKey{}, false
	}
	return decisionBlockKey{runID: metadata.RunID, taskID: metadata.TaskID, agent: metadata.AgentName, attempt: metadata.Attempt}, true
}

// recordDecisionBlock stops the worker attempt that received a decided
// choice its decision lists in block-on. Decisions owned by the run's root
// coordinator are shared with its extra-model clones.
func (c *Coordinator) recordDecisionBlock(metadata InvocationMetadata, payload decisionPrimitivePayload) {
	if c == nil || payload.ErrorCode != "" || payload.Result == nil || payload.Result.Status != decisionrt.StatusDecided {
		return
	}
	if !c.decisionPrimitives.Blocks(payload.Name, payload.Result.Value.Choice) {
		return
	}
	key, ok := decisionBlockKeyFor(metadata)
	if !ok {
		return
	}
	root := c.tokenBudgetRoot()
	root.decisionBlocksMu.Lock()
	defer root.decisionBlocksMu.Unlock()
	if root.decisionBlocks == nil {
		root.decisionBlocks = make(map[decisionBlockKey]string)
	}
	root.decisionBlocks[key] = payload.Name + "=" + payload.Result.Value.Choice
}

// decisionBlock returns the decision that stopped the current worker
// attempt, if any.
func (c *Coordinator) decisionBlock(ctx context.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok {
		return "", false
	}
	key, ok := decisionBlockKeyFor(metadata)
	if !ok {
		return "", false
	}
	root := c.tokenBudgetRoot()
	root.decisionBlocksMu.Lock()
	defer root.decisionBlocksMu.Unlock()
	decision, blocked := root.decisionBlocks[key]
	return decision, blocked
}

func decisionBlockToolDenial(decision, tool string) string {
	return fmt.Sprintf("decision_block: %s stops this step, so %q was not executed. You may still read, but do not change anything; call submit_result with status blocked and the reason.", decision, tool)
}

func decisionBlockResultDenial(decision, status string) string {
	return fmt.Sprintf("decision_block: %s stops this step, so status %q is not accepted. Call submit_result again with status blocked and the reason.", decision, status)
}
