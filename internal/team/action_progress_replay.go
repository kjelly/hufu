package team

// Attempts and sequences are ordered within an action invocation, not across
// recovery branches. A new action_started event binds the identity that may
// replace the previous invocation's observation, even if its counter restarts.
// Histories without that binding retain the legacy ordering rule.
func replayActionProgress(prior, observed *ActionProgressObservation, activeActionID string) *ActionProgressObservation {
	if observed == nil || (activeActionID != "" && observed.ActionID != activeActionID) {
		return prior
	}
	if prior == nil || (activeActionID != "" && prior.ActionID != activeActionID) ||
		observed.Attempt > prior.Attempt ||
		(observed.Attempt == prior.Attempt && observed.ActionID == prior.ActionID && observed.Sequence > prior.Sequence) {
		return cloneActionProgress(observed)
	}
	return prior
}
