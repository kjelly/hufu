package team

// taskStartedDispatchAttemptKey marks the task_started events that begin a
// worker attempt. task_started is derived from the in_progress status, so a
// same-status re-commit (a parent resuming after a delegated child, a human
// terminal handoff returning) emits one too; only events carrying this key
// start an attempt. Its value is the in-dispatch attempt counter, the same
// number the attempt's ExecutionReceipt.Attempt records.
const taskStartedDispatchAttemptKey = "dispatch_attempt"

// attemptStartMetadata is the transition metadata for an in_progress commit
// that begins a new attempt.
func attemptStartMetadata(dispatchAttempt int) map[string]interface{} {
	return map[string]interface{}{taskStartedDispatchAttemptKey: dispatchAttempt}
}
