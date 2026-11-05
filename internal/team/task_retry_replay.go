package team

// A late status checkpoint cannot reopen terminal work. A retry must carry the
// runtime's explicit attempt marker and a newer, closed occurrence lease, and
// the ordinary lifecycle must permit reopening this status.
func isReplayRetryStart(item *TodoItem, event RunEvent, status string, revision int, dispatchID string) bool {
	return item != nil && isReducerTerminalTaskStatus(item.Status) &&
		CanTransition(item.Status, TaskInProgress) && status == string(TaskInProgress) &&
		eventDispatchAttempt(event) > 1 && revision > item.OccurrenceRevision && dispatchID == ""
}
