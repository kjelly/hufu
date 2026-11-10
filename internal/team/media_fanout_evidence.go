package team

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// nativeMediaAcceptedWork reads canonical completion evidence. A merged model
// summary is never a substitute for the accepted result of every launched leaf.
func (c *Coordinator) nativeMediaAcceptedWork(item *TodoItem) bool {
	if !nativeMediaHasModelFanout(item) {
		return c.GetTaskResult(item.ID) != nil
	}
	for index := range min(len(item.ExecutionTopology), maxConcurrentModels) {
		accepted := false
		for _, receipt := range item.ExecutionReceipts {
			expected, err := nativeMediaFanoutExecutionID(item, receipt.RunID, index)
			result := receipt.AcceptedResult
			if err != nil || receipt.RunID == "" || receipt.ModelExecutionID != expected ||
				!receipt.Succeeded() || receipt.FinishedAt.IsZero() || receipt.HandoffState != ResultHandoffSubmitted ||
				receipt.TaskID != item.ID || receipt.ExecutionTarget != item.ExecutionTopology[index] ||
				receipt.MediaInputDigest != item.MediaInputDigest || receipt.OccurrenceAttempt != item.Retries+1 ||
				result == nil || result.TaskID != item.ID || result.Attempt != receipt.Attempt ||
				!strings.EqualFold(result.Agent, item.Agent) || !isSubmittedResultSource(result.Source) ||
				validateCompletedTaskResult(result) != nil || !nativeMediaAcceptedPayload(item, receipt) {
				continue
			}
			accepted = true
			break
		}
		if !accepted {
			return false
		}
	}
	return len(item.ExecutionTopology) > 1
}

func nativeMediaAcceptedPayload(item *TodoItem, receipt ExecutionReceipt) bool {
	payload := receipt.AcceptedResult.StructuredPayload
	if payload == nil {
		return item.ResultContract == nil || !item.ResultContract.RequireStructured
	}
	if item.ResultContract == nil || payload.Contract != *item.ResultContract ||
		receipt.ResultValidation != ResultValidationValid || receipt.ResultPayloadSHA256 != payload.SHA256 ||
		receipt.ResultContractID != payload.Contract.ID || receipt.ResultContractSHA256 != payload.Contract.SchemaSHA256 ||
		len(payload.Value) > resultPayloadMaxBytes {
		return false
	}
	digest := sha256.Sum256(payload.Value)
	return payload.SHA256 == hex.EncodeToString(digest[:])
}
