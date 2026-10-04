package team

import (
	"slices"

	"github.com/kjelly/hufu/internal/execution"
)

// maxToolSequenceEntries bounds the tool names kept per attempt.
const maxToolSequenceEntries = 256

// ToolSequenceRecord is the content-free order of an attempt's executed tool
// calls: tool names only, never arguments or results.
type ToolSequenceRecord struct {
	Tools []string `json:"tools,omitempty"`
	// Truncated counts calls past maxToolSequenceEntries that were not kept.
	Truncated int `json:"truncated,omitempty"`
}

func toolSequenceRecord(calls []executedToolCall) *ToolSequenceRecord {
	record := &ToolSequenceRecord{}
	for _, call := range calls {
		if len(record.Tools) == maxToolSequenceEntries {
			record.Truncated++
			continue
		}
		record.Tools = append(record.Tools, call.name)
	}
	return record
}

func cloneToolSequenceRecord(record *ToolSequenceRecord) *ToolSequenceRecord {
	if record == nil {
		return nil
	}
	return &ToolSequenceRecord{Tools: slices.Clone(record.Tools), Truncated: record.Truncated}
}

// toolSequenceObservable reports whether hufu itself executed an attempt's
// tool calls on target, so its tool gate saw every one. An external agent
// provider runs its own tools.
func (c *Coordinator) toolSequenceObservable(target execution.ExecutionTarget) bool {
	if c.workerAgentOverride != nil {
		return true
	}
	backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
	return err == nil && backend.Capabilities().DirectLanguageModel
}
