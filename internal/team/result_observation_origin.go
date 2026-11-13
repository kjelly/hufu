package team

import (
	"context"
)

type resultObservationOrigin struct {
	branch       string
	schema       string
	events       []RunEvent
	workRevision int
	workDispatch string
}

// A collection upgrade changes the submission contract, never the identity of
// successful reads in its sealed work. Only the canonical recovery proof can
// select that original observation namespace; the current artifact scope and
// content integrity checks still apply when each key is resolved.
func (c *Coordinator) resultObservationScope(ctx context.Context, taskID string, compiled *CompiledResultContract, metadata InvocationMetadata, events []RunEvent) (resultObservationOrigin, error) {
	origin := resultObservationOrigin{branch: c.activeBranchID(), events: events}
	if compiled != nil {
		origin.schema = compiled.SchemaSHA256
	}
	evidence, collecting := ctx.Value(resultCollectionEvidenceKey{}).(*taskTranscript)
	item := c.todoItemByID(taskID)
	cp := currentResultCollection(item)
	if !collecting || evidence == nil || cp == nil {
		return origin, nil
	}
	if err := validateResultCollectionCheckpoint(cp); err != nil {
		return origin, err
	}
	if cp.BranchID != origin.branch || cp.SourceRunID != metadata.RunID || cp.TaskID != metadata.TaskID || cp.Agent != metadata.AgentName ||
		cp.Attempt != metadata.Attempt || cp.ModelExecutionID != metadata.ModelExecutionID {
		return origin, errResultCollectionStale
	}
	if _, err := c.loadCollectionWork(ctx, item, cp); err != nil {
		return origin, err
	}
	origin.workRevision, origin.workDispatch = cp.WorkOccurrenceRevision, cp.WorkDispatchID
	if cp.ContractUpgrade == nil {
		return origin, nil
	}
	proof := cp.ContractUpgrade
	if item.ResultContract == nil || *item.ResultContract != proof.AcceptedContract || compiled == nil || compiled.SchemaSHA256 != proof.AcceptedContract.SchemaSHA256 {
		return origin, errResultCollectionStale
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return origin, err
	}
	for index, event := range events {
		if event.Type != string(EventContractRecovery) || event.BranchID != cp.BranchID {
			continue
		}
		admitted, err := executionPolicySnapshotFromEvents(events[:index])
		if err != nil {
			return origin, err
		}
		plan, err := validateContractRecoveryLineage(event, events[:index], admitted)
		if err != nil {
			return origin, err
		}
		for _, patch := range plan.Tasks {
			if patch.ID == taskID && patch.Action == "repair_result" && sameCanonicalEvidence(patch.Collection, proof) {
				origin.branch, origin.schema, origin.events = proof.Original.BranchID, proof.OriginalContract.SchemaSHA256, events[:index]
				return origin, nil
			}
		}
	}
	return origin, errResultCollectionStale
}
