package team

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

type fanoutResumeLeaf struct {
	checkpoint *ResultCollectionCheckpoint
	work       *resultCollectionWork
	completion *fanoutLeafCompletion
}

// Authenticate every leaf's recovery input before starting the provider
// boundary. A sibling must not spend a collector turn on an incomplete fanout.
func (c *Coordinator) prepareFanoutRecovery(ctx context.Context, item *TodoItem, task TaskDef, def *agent.AgentDef) ([]fanoutResumeLeaf, error) {
	leaves := make([]fanoutResumeLeaf, min(len(item.ExecutionTopology), maxConcurrentModels))
	for index := range leaves {
		target := item.ExecutionTopology[index]
		external, err := c.fanoutLeafNeedsCompletion(def, task, target)
		if err != nil {
			return nil, err
		}
		if external {
			leaves[index].completion, _, err = c.readFanoutLeafCompletion(ctx, item, index)
		} else {
			leaves[index], err = c.prepareFanoutCollectionLeaf(ctx, item, index, target)
			if err == nil && leaves[index].checkpoint.State == ResultCollectionCommitted {
				leaves[index].completion, _, err = c.loadFanoutLeafCompletion(ctx, item, index)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("recover fanout slot %d: %w", index, err)
		}
	}
	return leaves, nil
}

// Completed parent evidence lives in CAS. Keep private leaf workspaces through
// collection and verification recovery, and release only those exact workspaces
// after the parent's canonical terminal transition succeeds.
func (c *Coordinator) releaseFanoutCollectionWorkspaces(ctx context.Context, taskID string) {
	item := c.todoItemByID(taskID)
	if item == nil || item.Status != TaskDone {
		return
	}
	root := filepath.Join(c.artifactStoreRootPath(), "extra-models")
	for _, receipt := range item.ExecutionReceipts {
		checkpoint := receipt.ResultCollection
		if checkpoint == nil || !checkpoint.FanoutLeaf || checkpoint.State != ResultCollectionCommitted {
			continue
		}
		work, err := c.loadCollectionWork(ctx, item, checkpoint)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(root, work.Workspace)
		if err != nil || relative == "." || !filepath.IsLocal(relative) {
			continue
		}
		if err := os.RemoveAll(work.Workspace); err != nil {
			log.Printf("warning: release completed fanout collection workspace: %v", err)
		}
	}
}

// Select by the frozen topology slot, including repeated model names. A
// missing work checkpoint after an interrupted fanout is unknown-effect work,
// never permission to silently dispatch that leaf again.
func (c *Coordinator) prepareFanoutCollectionLeaf(ctx context.Context, item *TodoItem, index int, target execution.ExecutionTarget) (fanoutResumeLeaf, error) {
	selected := currentFanoutCollection(item, index)
	if selected == nil || selected.ExecutionTarget != target {
		return fanoutResumeLeaf{}, fmt.Errorf("fanout leaf has no sealed work checkpoint; execution effects require reconciliation: %w", errResultCollectionStale)
	}
	work, err := c.loadCollectionWork(ctx, item, selected)
	if err != nil {
		return fanoutResumeLeaf{}, err
	}
	if err := c.verifyFanoutCollectionProgress(ctx, item, selected, work); err != nil {
		return fanoutResumeLeaf{}, err
	}
	return fanoutResumeLeaf{checkpoint: selected, work: work}, nil
}

func (c *Coordinator) verifyFanoutCollectionProgress(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint, work *resultCollectionWork) error {
	switch cp.State {
	case ResultCollectionBlocked:
		if !collectionGrantedForRun(collectionReceipt(item, cp), c.executionRunID) {
			return errResultCollectionBlocked
		}
	case ResultCollectionCommitted:
		return c.verifyAcceptedFanoutCollection(ctx, item, cp)
	}
	ctx, err := c.collectionReadContext(ctx, item, cp, work)
	if err != nil {
		return err
	}
	_, compiled, err := c.boundResultContract(cp.TaskID)
	if err != nil {
		return err
	}
	// A collector that has not returned may still report an unsuccessful
	// result. Once returned, use the same omission rules as admission.
	omissions := resultSourceOmissions{runtimeCompletion: hasRuntimeCompletion(compiled), unsettledDecisions: true}
	if cp.State == ResultCollectionInFlight {
		returned, err := c.readReturnedCollectionResponse(cp)
		if err != nil {
			return err
		}
		if returned != nil {
			blob, err := decodeCollectionBlob(cp, "response", returned.Body, resultCollectionResponseMaxBytes)
			if err != nil {
				return err
			}
			omissions, err = c.validateFanoutReturnedClaims(ctx, cp, work, compiled, blob, false)
			if err != nil {
				return err
			}
		}
	}
	if cp.Response != nil {
		omissions, err = c.verifySavedFanoutClaims(ctx, cp, work, compiled)
		if err != nil {
			return err
		}
	}
	if cp.Request != nil {
		if _, err := c.readCollectionBlobRecord(ctx, cp, "request", cp.Request); err != nil {
			return err
		}
	}
	return c.verifyFanoutCollectionSources(ctx, cp, work, compiled, omissions)
}

func (c *Coordinator) verifySavedFanoutClaims(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract) (resultSourceOmissions, error) {
	response, err := c.readCollectionBlobRecord(ctx, cp, "response", cp.Response)
	if err != nil {
		return resultSourceOmissions{}, err
	}
	if cp.Prepared == nil {
		return c.validateFanoutReturnedClaims(ctx, cp, work, compiled, response, false)
	}
	prepared, err := c.readCollectionBlobRecord(ctx, cp, "claims", cp.Prepared)
	if err != nil {
		return resultSourceOmissions{}, err
	}
	raw, _ := collectionResponseJSON(response.Data)
	if response.ResponseIssue != nil || !bytes.Equal(prepared.Data, raw) {
		return resultSourceOmissions{}, fmt.Errorf("prepared fanout claims differ from the original response: %w", errResultCollectionStale)
	}
	return c.validateFanoutReturnedClaims(ctx, cp, work, compiled, prepared, true)
}

func (c *Coordinator) validateFanoutReturnedClaims(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract, blob *resultCollectionBlob, prepared bool) (resultSourceOmissions, error) {
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, cp)
	raw, _ := collectionResponseJSON(blob.Data)
	inspection := string(raw)
	var omissions resultSourceOmissions
	if blob.ResponseIssue == nil {
		omissions = submissionSourceOmissions(compiled, inspection, false)
	}
	if grantedSourceFieldRecollection(collectionReceipt(c.todoItemByID(cp.TaskID), cp), cp) != nil {
		// Recheck the source fields that the grant allowed the next turn to
		// recollect. The projection is for inspection only.
		var err error
		inspection, _, err = c.collectionSourceFieldRepair(ctx, cp, compiled, inspection)
		if err != nil {
			return omissions, err
		}
	}
	if err := c.validateSubmissionSourceEnvelope(ctx, cp.TaskID, work.Workspace, inspection, cp, compiled); err != nil && localResultRejectionReason(err) != RepairFailureInvalidSchema {
		return omissions, err
	}
	if err := c.validateRejectedCollectionResponseSources(ctx, cp, work, compiled, blob, false); err != nil {
		return omissions, err
	}
	return omissions, c.validateFanoutPayloadSources(ctx, cp, work, compiled, inspection, blob, prepared)
}

func (c *Coordinator) verifyFanoutCollectionSources(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract, omissions resultSourceOmissions) error {
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, cp)
	if err := c.validateIndependentSubmissionSources(ctx, cp.TaskID, compiled, omissions); err != nil {
		return err
	}
	if err := c.verifyFanoutArtifactObservations(ctx, cp.TaskID, compiled); err != nil {
		return err
	}
	if _, err := c.collectionMemoryManifest(cp); err != nil {
		return err
	}
	if _, err := c.historicalResultCollectionInput(ctx, cp.TaskID, work.Input); err != nil {
		return err
	}
	if acceptedRepairProof(cp) != nil {
		original := cp.ContractUpgrade.Original
		_, err := c.readCollectionBlob(ctx, original, "response", original.Response)
		return err
	}
	return nil
}

func currentFanoutCollection(item *TodoItem, index int) *ResultCollectionCheckpoint {
	if item == nil || index < 0 || index >= len(item.ExecutionTopology) {
		return nil
	}
	var selected *ResultCollectionCheckpoint
	for _, receipt := range item.ExecutionReceipts {
		cp := receipt.ResultCollection
		legacyFirstOccurrence := receipt.OccurrenceAttempt == 0 && item.Retries == 0
		if cp == nil || !cp.FanoutLeaf || cp.FanoutIndex != index || cp.ExecutionTarget != item.ExecutionTopology[index] ||
			cp.Attempt != receipt.Attempt || (receipt.OccurrenceAttempt != item.Retries+1 && !legacyFirstOccurrence) {
			continue
		}
		if selected == nil || cp.Attempt > selected.Attempt {
			selected = cp
		}
	}
	return cloneResultCollectionCheckpoint(selected)
}

func hasCurrentFanoutCollection(item *TodoItem) bool {
	if item == nil || len(item.ExecutionTopology) < 2 {
		return false
	}
	for index := range min(len(item.ExecutionTopology), maxConcurrentModels) {
		if currentFanoutCollection(item, index) != nil {
			return true
		}
	}
	return false
}
