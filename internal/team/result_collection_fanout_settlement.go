package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/utils"
)

type fanoutSettlement struct {
	OccurrenceSHA256 string                 `json:"occurrence_sha256"`
	Slots            []fanoutSettlementSlot `json:"slots"`
	Output           string                 `json:"output"`
	OutputSHA256     string                 `json:"output_sha256"`
	pendingBody      []byte
}

type fanoutSettlementSlot struct {
	Target           execution.ExecutionTarget `json:"target"`
	CollectionSHA256 string                    `json:"collection_sha256,omitempty"`
	CompletionSHA256 string                    `json:"completion_sha256,omitempty"`
}

func (c *Coordinator) fanoutSettlementDocument(item *TodoItem) (string, string, error) {
	if !hasCurrentFanoutCollection(item) {
		return "", "", errResultCollectionStale
	}
	digest, err := c.fanoutOccurrenceDigest(item)
	return "result-collection-fanout-output:" + digest, digest, err
}

func (c *Coordinator) fanoutSettlementSlots(ctx context.Context, item *TodoItem) ([]fanoutSettlementSlot, []*ExecutionReceipt, error) {
	slots := make([]fanoutSettlementSlot, min(len(item.ExecutionTopology), maxConcurrentModels))
	var receipts []*ExecutionReceipt
	for index := range slots {
		slots[index].Target = item.ExecutionTopology[index]
		cp := currentFanoutCollection(item, index)
		if cp == nil {
			leaf, digest, err := c.readFanoutLeafCompletion(ctx, item, index)
			if err != nil {
				return nil, nil, err
			}
			if leaf.pendingBody != nil {
				return nil, nil, errResultCollectionStale
			}
			slots[index].CompletionSHA256 = digest
			receipts = append(receipts, &leaf.Receipt)
			continue
		}
		if err := c.verifyAcceptedFanoutCollection(ctx, item, cp); err != nil {
			return nil, nil, err
		}
		digest, err := collectionDigest(cp)
		if err != nil {
			return nil, nil, err
		}
		slots[index].CollectionSHA256 = digest
		leaf, digest, err := c.loadFanoutLeafCompletion(ctx, item, index)
		if err != nil {
			return nil, nil, err
		}
		if leaf != nil {
			if leaf.pendingBody != nil {
				return nil, nil, errResultCollectionStale
			}
			slots[index].CompletionSHA256 = digest
			receipts = append(receipts, &leaf.Receipt)
		}
	}
	return slots, receipts, nil
}

func (c *Coordinator) verifyAcceptedFanoutCollection(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint) error {
	if cp.State != ResultCollectionCommitted {
		return errResultCollectionStale
	}
	work, err := c.loadCollectionWork(ctx, item, cp)
	if err != nil {
		return err
	}
	result, err := c.readAcceptedCollectionResult(ctx, item, cp)
	if err != nil {
		return err
	}
	receipt := collectionReceipt(item, cp)
	if receipt == nil {
		return errResultCollectionStale
	}
	projection, err := c.verifyCollectedWorkEvidence(ctx, cp, result, receipt)
	if err != nil {
		return err
	}
	if item.Verify != "" || item.VerifySpec != nil || item.AdversarialVerify > 0 {
		checkpoint, err := c.readFanoutCheckProgress(item, cp, work, result, projection)
		if err != nil {
			return err
		}
		if receipt.VerifyResult != nil && (checkpoint == nil || !checkpoint.verified(item, receipt.VerifyResult)) {
			return rejectResultSourceRead(errors.New("fanout verification outcome differs from the original receipt"))
		}
	}
	_, compiled, err := c.boundResultContract(item.ID)
	if err != nil {
		return err
	}
	requiredReads := item.WorksetBinding != nil && len(item.WorksetBinding.RequiredReads) > 0
	observations := compiled != nil && (compiled.evidenceKeys != nil || hasResultDecisions(compiled))
	if !requiredReads && !observations {
		return nil
	}
	readCtx, err := c.collectionReadContext(ctx, item, cp, work)
	if err != nil {
		return err
	}
	readCtx = context.WithValue(readCtx, resultCollectionAdmissionKey{}, cp)
	if err := c.validateRequiredArtifactReadSubmission(readCtx, item.ID); err != nil {
		return err
	}
	return c.verifyFanoutArtifactObservations(readCtx, item.ID, compiled)
}

func (c *Coordinator) sealFanoutSettlement(ctx context.Context, item *TodoItem, slots []fanoutSettlementSlot, output string) error {
	name, occurrence, err := c.fanoutSettlementDocument(item)
	if err != nil {
		return err
	}
	digest, err := collectionDigest(output)
	if err != nil {
		return err
	}
	body, err := json.Marshal(fanoutSettlement{OccurrenceSHA256: occurrence, Slots: slots, Output: output, OutputSHA256: digest}, json.Deterministic(true))
	if err != nil {
		return err
	}
	if err := c.stageFanoutDocument(context.WithoutCancel(ctx), name, body); err != nil {
		return err
	}
	return c.publishFanoutDocument(ctx, name, body)
}

func (c *Coordinator) readFanoutSettlement(ctx context.Context, item *TodoItem) (*fanoutSettlement, []*ExecutionReceipt, error) {
	name, occurrence, err := c.fanoutSettlementDocument(item)
	if err != nil {
		return nil, nil, err
	}
	body, pending, err := c.readFanoutDocument(ctx, name)
	if err != nil || body == nil {
		return nil, nil, err
	}
	var saved fanoutSettlement
	if err := json.Unmarshal(body, &saved, json.RejectUnknownMembers(true)); err != nil || saved.OccurrenceSHA256 != occurrence {
		return nil, nil, errResultCollectionStale
	}
	slots, receipts, err := c.fanoutSettlementSlots(ctx, item)
	if err != nil {
		return nil, nil, err
	}
	digest, err := collectionDigest(saved.Output)
	if err != nil || digest != saved.OutputSHA256 || !slices.Equal(saved.Slots, slots) {
		return nil, nil, errResultCollectionStale
	}
	if pending {
		saved.pendingBody = body
	}
	return &saved, receipts, nil
}

func (c *Coordinator) commitRecoveredFanoutOutput(ctx context.Context, item *TodoItem, saved *fanoutSettlement, receipts []*ExecutionReceipt) (string, error) {
	if saved.pendingBody != nil {
		name, _, err := c.fanoutSettlementDocument(item)
		if err != nil {
			return "", err
		}
		if err := c.publishFanoutDocument(ctx, name, saved.pendingBody); err != nil {
			return "", err
		}
	}
	for _, receipt := range receipts {
		if err := c.setAttemptReceipt(item.ID, receipt); err != nil {
			return "", err
		}
	}
	return c.commitFanoutOutput(ctx, item, saved.Output)
}

func (c *Coordinator) commitFanoutOutput(ctx context.Context, item *TodoItem, output string) (string, error) {
	if item == nil {
		return "", errResultCollectionStale
	}
	expected := TaskInProgress
	if err := c.commitTaskTransition(ctx, item.ID, &expected, item.OccurrenceRevision, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output, nil); err != nil {
		if _, appendFailed := errors.AsType[*taskTransitionAppendError](err); appendFailed && hasCurrentFanoutCollection(item) {
			err = rejectLocalResultPersistence(err)
		}
		return "", fmt.Errorf("mark fanout task done: %w", err)
	}
	c.reconcileTaskStatusProjection()
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	return output, nil
}
