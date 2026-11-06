package team

import (
	"context"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func (c *Coordinator) persistMediaConversationReset(ctx context.Context) error {
	workspace := c.sessionWorkspace()
	if workspace == "" {
		return nil
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil {
		return err
	}
	if !exists || !isMediaCompactionVersion(state.Version) {
		return nil
	}
	branch := c.compactionBranch()
	nextSource := state.Branches[branch].NextSourceIndex
	if err := c.commitNativeConversationCheckpoint(ctx, state, branch, nil, nil, nextSource, nativeResetGenerationKind); err != nil {
		return err
	}
	return workspaceDocs(workspace).deleteDocument(runtimestore.DocumentSessionHistory)
}

func (c *Coordinator) resetConversationSourceIndex() int {
	c.compactionMu.Lock()
	defer c.compactionMu.Unlock()
	if c.compactionState != nil && isMediaCompactionVersion(c.compactionState.Version) {
		return c.compactionState.Branches[c.compactionBranchID].NextSourceIndex
	}
	return 0
}
