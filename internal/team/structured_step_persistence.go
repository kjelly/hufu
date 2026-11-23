package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/utils"
)

const EventStructuredStepReceiptRecorded EventType = "structured_step_receipt_recorded"

type structuredStepReceiptRecorded struct {
	Version            int                    `json:"version"`
	ReceiptID          string                 `json:"receipt_id"`
	OccurrenceRevision int                    `json:"occurrence_revision,omitzero"`
	Artifact           ArtifactRef            `json:"artifact"`
	Summary            *StructuredStepSummary `json:"summary,omitempty"`
}

func decodeStructuredStepReceiptEvent(event RunEvent) (structuredStepReceiptRecorded, error) {
	var payload structuredStepReceiptRecorded
	if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
		return payload, err
	}
	if event.Type != string(EventStructuredStepReceiptRecorded) || event.SchemaVersion < eventStoreStrictSchemaVersion ||
		event.RunID == "" || event.BranchID == "" || event.TaskID == "" || event.Attempt < 1 || event.Actor != "runtime" || payload.Version != 1 ||
		payload.ReceiptID == "" || payload.OccurrenceRevision < 0 || payload.Artifact.ID == "" ||
		!validStructuredStepDigest(payload.Artifact.SHA256) || payload.Artifact.Bytes <= 0 ||
		payload.Artifact.Bytes != payload.Artifact.ByteSize || payload.Artifact.Bytes > maxStructuredStepReceiptBytes ||
		payload.Artifact.Kind != "structured_step_receipt" || payload.Artifact.MediaType != "application/json" ||
		payload.Artifact.RunID != event.RunID || payload.Artifact.TaskID != event.TaskID || payload.Artifact.Attempt != event.Attempt ||
		payload.Artifact.Agent == "" || event.IdempotencyKey != structuredStepReceiptEventKey(event.RunID, event.BranchID, event.TaskID, event.Attempt, payload.OccurrenceRevision, payload.ReceiptID) {
		return payload, fmt.Errorf("invalid structured step receipt source")
	}
	if payload.Summary != nil {
		if err := validateStructuredStepSummary(*payload.Summary, payload.ReceiptID); err != nil {
			return payload, err
		}
	}
	return payload, nil
}

func (c *Coordinator) structuredStepReceiptPublisher(taskID string, attempt int) func(context.Context, ExecutionStepReceipt) (ExecutionStepReceipt, error) {
	if c.EventStore() == nil {
		return nil
	}
	identity, ok := c.activeTaskResultOccurrence(taskID)
	branchID := c.EventStore().BranchID()
	return func(ctx context.Context, receipt ExecutionStepReceipt) (ExecutionStepReceipt, error) {
		current, active := c.activeTaskResultOccurrence(taskID)
		if !ok || !active || !sameTaskResultOccurrence(identity, current) || identity.Attempt != attempt ||
			receipt.TaskID != taskID || receipt.Attempt != attempt || c.EventStore() == nil || c.EventStore().BranchID() != branchID {
			return receipt, fmt.Errorf("structured step receipt occurrence is stale")
		}
		receipt.RunID, receipt.BranchID, receipt.OccurrenceRevision = identity.RunID, branchID, identity.OccurrenceRevision
		canonical, raw, err := encodeStructuredStepReceipt(receipt)
		if err != nil {
			return receipt, err
		}
		store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
		if err != nil {
			return receipt, err
		}
		published, err := store.Put(context.WithoutCancel(ctx), PutArtifactRequest{
			Kind: "structured_step_receipt", Path: "structured-step-receipt", MediaType: "application/json", Content: raw,
			RunID: identity.RunID, TaskID: identity.TaskID, Attempt: identity.Attempt, Agent: identity.Agent,
		})
		if err != nil {
			return receipt, err
		}
		summary := summarizeStructuredStep(canonical)
		payload, err := json.Marshal(structuredStepReceiptRecorded{Version: 1, ReceiptID: receipt.ID, Summary: &summary,
			OccurrenceRevision: identity.OccurrenceRevision, Artifact: published.ArtifactRef})
		if err != nil {
			return receipt, err
		}
		durable, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
			Type: string(EventStructuredStepReceiptRecorded), RunID: identity.RunID, BranchID: branchID,
			TaskID: taskID, Attempt: attempt, Actor: "runtime", Payload: payload,
			IdempotencyKey: structuredStepReceiptEventKey(identity.RunID, branchID, taskID, attempt, identity.OccurrenceRevision, receipt.ID),
		})
		if err != nil {
			return canonical, err
		}
		recorded, err := decodeStructuredStepReceiptEvent(durable)
		if err != nil {
			return canonical, err
		}
		if recorded.Summary == nil || !sameStructuredStepSummary(summary, *recorded.Summary) {
			return canonical, fmt.Errorf("structured step summary changed during persistence")
		}
		return canonical, nil
	}
}

func (c *Coordinator) hydrateStructuredStepReceipts(events []RunEvent) {
	hasReceipts := false
	for _, event := range events {
		if event.Type == string(EventStructuredStepReceiptRecorded) {
			hasReceipts = true
			break
		}
	}
	if !hasReceipts {
		return
	}
	lineage, err := c.readActiveLineageEvents(context.Background())
	if err != nil {
		c.markSessionRecovery("structured step lineage replay failed: " + utils.RedactSecrets(err.Error()))
		return
	}
	rows, err := LoadStructuredStepReceipts(context.Background(), c.artifactStoreRootPath(), lineage)
	if err != nil {
		c.markSessionRecovery("structured step receipt replay failed: " + utils.RedactSecrets(err.Error()))
		return
	}
	for _, row := range rows {
		if err := c.executionStepReceiptRegistry().Record(row.Receipt); err != nil {
			c.markSessionRecovery("structured step receipt replay conflict: " + utils.RedactSecrets(err.Error()))
			return
		}
	}
}

func structuredStepReceiptEventKey(runID, branchID, taskID string, attempt, revision int, receiptID string) string {
	raw := fmt.Sprintf("%q/%q/%q/%d/%d/%q", runID, branchID, taskID, attempt, revision, receiptID)
	digest := sha256.Sum256([]byte(raw))
	return branchID + ":step-receipt:" + hex.EncodeToString(digest[:])
}
