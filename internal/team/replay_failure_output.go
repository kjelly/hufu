package team

import "github.com/kjelly/hufu/internal/utils"

// Old checkpoint re-emission shortened failure output even when no task state
// changed. Recover only an exact redundant snapshot from an earlier canonical
// event. A checkpoint, prefix similarity, new work or changed receipt supplies
// no authority. New events explicitly distinguish complete short output.
func legacyFailureOutputSource(kind string, previous *TodoItem, output string, complete bool) *TodoItem {
	if complete || previous == nil || output == "" || output == previous.Output {
		return nil
	}
	switch kind {
	case "task_failed", "task_cancelled", "task_blocked", "task_protocol_incomplete":
	default:
		return nil
	}
	redacted := utils.RedactSecrets(previous.Output)
	if redacted != previous.Output || output != utils.TruncateString(redacted, 2000) {
		return nil
	}
	return cloneTodoItem(previous)
}

func restoreRedundantFailureOutput(previous, current *TodoItem) {
	if previous == nil {
		return
	}
	candidate := cloneTodoItem(current)
	candidate.Output = previous.Output
	if compareSingleTaskProjection(previous, candidate) == nil {
		current.Output = previous.Output
	}
}
