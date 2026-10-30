package team

// applyCollectionRepairGrant runs inside the task transition projection lock,
// after authority is validated and before the single durable transition.
func applyCollectionRepairGrant(projected, current *TodoItem, grant *ResultRepairGrant) {
	if projected.ExecutionReceipt.ResultCollection == nil {
		return
	}
	receipt := projected.ExecutionReceipt
	checkpoint := cloneResultCollectionCheckpoint(receipt.ResultCollection)
	checkpoint.Revision++
	checkpoint.SubmissionRevision = projected.OccurrenceRevision
	checkpoint.SubmissionDispatchID = newTaskDispatchID()
	checkpoint.TurnLimit = max(checkpoint.TurnLimit, grant.AfterAttempts+grant.AdditionalAttempts)
	if recovery := grant.Recovery; recovery != nil && checkpoint.State != ResultCollectionCommitted {
		// The grant event preserves recovery.Checkpoint with the old prepared
		// reference. Only the live progress is reopened; work is immutable.
		switch recovery.Disposition {
		case RecoveryRepairClaims:
			checkpoint.State, checkpoint.Prepared, checkpoint.FailureReason = ResultCollectionReceived, nil, RepairFailureInvalidSchema
		case RecoveryCommitLocal:
			if checkpoint.Prepared != nil {
				checkpoint.State = ResultCollectionPrepared
			} else {
				checkpoint.State = ResultCollectionReceived
			}
			checkpoint.FailureReason = ""
		}
	} else if checkpoint.Prepared != nil && checkpoint.State != ResultCollectionCommitted {
		checkpoint.State, checkpoint.FailureReason = ResultCollectionPrepared, ""
	}
	receipt.ResultCollection = checkpoint
	projected.DispatchID = checkpoint.SubmissionDispatchID
	projected.ExecutionReceipts = appendExecutionReceipt(append([]ExecutionReceipt(nil), current.ExecutionReceipts...), cloneExecutionReceipt(receipt))
}
