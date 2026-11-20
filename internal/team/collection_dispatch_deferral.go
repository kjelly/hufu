package team

import "slices"

const (
	dispatchDeferredDetail         = "anti-thrashing limit reached for this task's scope; strategy change or human review required"
	collectionDispatchResumeDetail = "released for first dispatch after verified result recovery"
	failureTypeDispatchDeferred    = "dispatch_deferred"
)

func collectionUndispatched(item *TodoItem) bool {
	return item != nil && item.Retries == 0 && item.StartedAt.IsZero() &&
		item.ExecutionReceipt == nil && len(item.ExecutionReceipts) == 0 &&
		len(item.ContextManifests) == 0 && len(item.DecisionArtifacts) == 0 &&
		item.TypedResult == nil && item.ResultCandidate == nil && item.VerifyResult == nil
}

func collectionReadOnlyPendingTask(item *TodoItem) bool {
	return collectionUndispatched(item) && !IsPrimaryOccurrence(item) && item.Action == nil && !item.WorkerWorkspace.isolated() &&
		!item.CheckpointPause && (item.RecoveryState == "" || item.RecoveryState == RecoveryStateNotStarted) &&
		metadataReadOnlyTask(item.SideEffect, item.Execution)
}

func collectionDispatchDeferred(item, source *TodoItem) bool {
	return collectionReadOnlyPendingTask(item) && schedulerDispatchDeferred(item) &&
		item.Phase == source.Phase && item.Agent == source.Agent && stableOperation(item) == stableOperation(source)
}

func schedulerDispatchDeferred(item *TodoItem) bool {
	if !collectionUndispatched(item) || item.Status != TaskBlocked {
		return false
	}
	failure := item.FailureEvent
	if failure == nil || failure.TaskID != item.ID || failure.Phase != "coordination" ||
		failure.FailureClass != FailurePolicy || failure.RetryDisposition != NeedsHuman {
		return false
	}
	// Older runs recorded the synthetic policy denial as a task failure.
	// Match the scheduler's original fingerprint, not its formatted summary.
	return failure.FailureType == failureTypeDispatchDeferred ||
		slices.ContainsFunc(item.FailureFingerprints, func(fp FailureFingerprint) bool {
			return fp.Digest == failure.Fingerprint && isDispatchDeferralFingerprint(fp) &&
				fp.Component == item.Agent && fp.Operation == stableOperation(item)
		})
}

func isDispatchDeferralFingerprint(fp FailureFingerprint) bool {
	return fp.Class == FailurePolicy && fp.NormalizedError == dispatchDeferredDetail
}

func discardDispatchDeferralFingerprints(item *TodoItem) {
	item.FailureFingerprints = slices.DeleteFunc(slices.Clone(item.FailureFingerprints), isDispatchDeferralFingerprint)
}

func collectionDispatchRequeued(item *TodoItem) bool {
	return collectionReadOnlyPendingTask(item) && item.Status == TaskPending &&
		item.RecoveryState == RecoveryStateNotStarted && item.FailureEvent == nil && item.Detail == collectionDispatchResumeDetail
}
