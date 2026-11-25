package team

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

// A correction consumes its allowance before opening another provider turn.
// The parent dispatch owns the reservation even for a private model slot.
type externalResultCorrectionCheckpoint struct {
	BranchID     string           `json:"branch_id"`
	StartEventID string           `json:"start_event_id"`
	DispatchID   string           `json:"dispatch_id"`
	Revision     int              `json:"revision"`
	Slot         int              `json:"slot"`
	Receipt      ExecutionReceipt `json:"receipt"`
	// Accept legacy reservations without carrying their unused path list into
	// new receipts.
	LegacyWorkspaceDelta jsontext.Value `json:"workspace_delta,omitempty"`
	Output               string         `json:"output"`
}

type externalResultCorrectionResponse struct {
	TurnID       string      `json:"turn_id"`
	Result       *TaskResult `json:"result"`
	ResultSHA256 string      `json:"result_sha256"`
}

type externalResultCorrectionReservation struct {
	start RunEvent
	slot  int
	key   string
	body  []byte
}

func externalResultCorrectionKey(branch string, item *TodoItem, startID string, slot int) string {
	return fmt.Sprintf("external-result-correction/%s/%s/%d/%s/%d", branch, item.ID, item.OccurrenceRevision, startID, slot)
}

// The durable start precedes installation of the live result-submission
// lease. A crash can therefore replay the start without the live dispatch ID.
// Use the journal event as the discoverable identity of the original work.
func (c *Coordinator) externalCorrectionStart(ctx context.Context, item *TodoItem) (*RunEvent, error) {
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	for _, event := range slices.Backward(events) {
		if event.TaskID != item.ID || eventDispatchAttempt(event) == 0 {
			continue
		}
		var start struct {
			Revision int `json:"occurrence_revision"`
		}
		if err := json.Unmarshal(event.Payload, &start); err != nil {
			return nil, err
		}
		if start.Revision != item.OccurrenceRevision {
			return nil, nil
		}
		if event.BranchID == "" {
			event.BranchID = "main"
		}
		return &event, nil
	}
	return nil, nil
}

func (c *Coordinator) reserveExternalResultCorrection(ctx context.Context, request AttemptRequest, result AttemptResult, transcriptRef string, world *PreparedExecutionWorld, rejection error) (string, *RepairProvenance, error) {
	owner, slot := c, 0
	if c.isModelFanoutLeaf(request.TaskID) {
		owner, slot = c.modelFanout.parent, c.modelFanout.index+1
	}
	item := owner.todoItemByID(request.TaskID)
	if item == nil || (request.BranchID != "" && (item.Status != TaskInProgress || item.DispatchID == "" || request.BranchID != owner.activeBranchID())) {
		return "", nil, rejectLocalResult(RepairFailureSource, errTaskResultStale)
	}
	branch, startID := owner.activeBranchID(), item.DispatchID
	if request.BranchID != "" {
		start, err := owner.externalCorrectionStart(ctx, item)
		if err != nil {
			return "", nil, rejectResultSourceRead(err)
		}
		if start == nil || start.ID == "" || start.RunID != request.RunID || start.BranchID != branch ||
			(slot == 0 && eventDispatchAttempt(*start) != request.Attempt) {
			return "", nil, rejectResultSourceRead(errTaskResultStale)
		}
		startID = start.ID
	}
	proof := &RepairProvenance{
		Attempted: true, RepairAttempts: 1,
		ProviderWorkTurnID: result.ProviderTurnID, ProviderWorkTranscriptRef: transcriptRef,
		Error: utils.TruncateRunes(utils.RedactSecrets(rejection.Error()), resultPayloadMaxErrors*resultPayloadErrorMaxRunes),
	}
	key := externalResultCorrectionKey(branch, item, startID, slot)
	checkpoint := externalResultCorrectionCheckpoint{
		BranchID: branch, StartEventID: startID, DispatchID: item.DispatchID, Revision: item.OccurrenceRevision, Slot: slot,
		Output: result.Output,
		Receipt: ExecutionReceipt{
			RunID: request.RunID, TaskID: request.TaskID, Attempt: request.Attempt, OccurrenceAttempt: item.Retries + 1,
			ProducerID: canonicalAttemptAgentName(request), ExecutionTarget: receiptExecutionTarget(request.ExecutionTarget),
			Backend: request.Provider, Model: request.ModelID, ModelExecutionID: request.modelExecutionID,
			ProviderSessionID: result.ProviderSessionID, ProviderTurnID: result.ProviderTurnID,
			ExecutionWorldID: world.ID, ProviderTranscriptRef: transcriptRef,
			WorkspaceBefore: world.Baseline.ID, WorkspaceAfter: request.resultWorkspaceSnapshot.ID,
			ArtifactScope: cloneArtifactAccessScope(request.ArtifactScope), StartedAt: item.StartedAt, FinishedAt: time.Now(),
			RunInputSnapshotID: item.RunInputSnapshotID, RunInputSnapshotHash: item.RunInputSnapshotHash,
			WorkSubmissionFailure: RepairFailureInvalidSchema, RepairProvenance: proof,
		},
	}
	body, err := json.Marshal(checkpoint)
	if err != nil {
		return "", nil, rejectLocalResultPersistence(err)
	}
	err = withRuntimeStore(owner.artifactStoreRootPath(), func(store *runtimestore.Store) error {
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			_, exists, err := runtimestore.GetDocument(ctx, tx, key)
			if err != nil {
				return err
			}
			if exists {
				return rejectLocalResult(RepairFailureSource, errors.New("result correction already reserved for this dispatch"))
			}
			return runtimestore.PutDocument(ctx, tx, key, body)
		})
	})
	if err != nil {
		return "", nil, rejectLocalResultPersistence(err)
	}
	return key, proof, nil
}

func (c *Coordinator) recordExternalCorrectionTurn(ctx context.Context, key string, proof *RepairProvenance, turnID string) error {
	if turnID == "" || turnID == proof.ProviderWorkTurnID {
		return rejectResultSourceRead(errors.New("result correction did not identify a distinct provider turn"))
	}
	if err := c.sealCollectionDocument(ctx, key+"/turn", []byte(turnID)); err != nil {
		return rejectLocalResultPersistence(err)
	}
	proof.ProviderCorrectionTurnID = turnID
	return nil
}

func (c *Coordinator) checkpointExternalCorrectionResponse(ctx context.Context, key, turnID string, candidate *TaskResult) (*externalResultCorrectionResponse, error) {
	if turnID == "" || candidate == nil {
		return nil, rejectResultSourceRead(errTaskResultStale)
	}
	digest, err := collectionDigest(candidate)
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	response := &externalResultCorrectionResponse{TurnID: turnID, Result: candidate, ResultSHA256: digest}
	body, err := json.Marshal(response, jsontext.PreserveRawStrings(true))
	if err == nil {
		err = c.sealCollectionDocument(ctx, key+"/response", body)
	}
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	return response, nil
}

// Seal the public result before either live admission or a correction checkpoint.
// The verified source projection and its hashes must survive redaction intact.
func redactExternalResultCandidate(result *TaskResult, compiled *CompiledResultContract, secrets *utils.SecretRedactionScope) (*TaskResult, error) {
	raw, err := json.Marshal(result, jsontext.PreserveRawStrings(true))
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	redacted, err := secrets.RedactJSONCompact(raw)
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	var safe TaskResult
	if err := json.Unmarshal(redacted, &safe, json.RejectUnknownMembers(true)); err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	originalEvidence, err := collectionDigest(externalResultEvidence(result))
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	safeEvidence, err := collectionDigest(externalResultEvidence(&safe))
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	if originalEvidence != safeEvidence ||
		(safe.StructuredPayload != nil && historicalResultDigest(safe.StructuredPayload.Value) != safe.StructuredPayload.SHA256) ||
		(hasRuntimeCompletion(compiled) && taskResultStatusIsSuccessful(result.Status) && safe.Summary != result.Summary) {
		return nil, rejectResultSourceRead(errors.New("redaction would alter verified external result evidence"))
	}
	if err := validateResultEnvelope(compiled, &safe); err != nil || validateFindingSeverities(safe.Findings) != nil {
		return nil, rejectResultSourceRead(errors.New("redaction would invalidate the external result envelope"))
	}
	return &safe, nil
}

func externalResultEvidence(result *TaskResult) TaskResult {
	evidence := *result
	evidence.Summary, evidence.Details = "", ""
	evidence.Findings, evidence.Risks, evidence.OpenQuestions, evidence.Facts = nil, nil, nil, nil
	evidence.Artifacts = slices.Clone(result.Artifacts)
	for i := range evidence.Artifacts {
		evidence.Artifacts[i].Description = ""
	}
	return evidence
}

// Resolve interrupted result-only calls before ordinary work-replay policy.
// A verified completion for a fanout slot already owns its final handoff.
func (c *Coordinator) stopInterruptedExternalCorrection(ctx context.Context, item *TodoItem) (bool, error) {
	if item.Status != TaskInProgress && item.Status != TaskProtocolIncomplete && item.Status != TaskVerifying {
		return false, nil
	}
	reservation, err := c.pendingExternalCorrection(ctx, item)
	if err != nil || reservation == nil {
		return err != nil, err
	}
	return true, c.restoreExternalCorrectionForReconciliation(ctx, item, reservation)
}

// Pending reservations own result settlement in both ordinary resume and
// targeted recovery. A completed fanout slot has its own accepted handoff.
func (c *Coordinator) pendingExternalCorrection(ctx context.Context, item *TodoItem) (*externalResultCorrectionReservation, error) {
	if c.session == nil || len(c.session.Config.SubagentProviders) == 0 || !c.hasDurableEventJournal() {
		return nil, nil
	}
	start, err := c.externalCorrectionStart(ctx, item)
	if err != nil || start == nil {
		return nil, rejectLocalResultPersistence(err)
	}
	docs := workspaceDocs(c.artifactStoreRootPath())
	for slot := 0; slot <= min(len(item.ExecutionTopology), maxConcurrentModels); slot++ {
		key := externalResultCorrectionKey(start.BranchID, item, start.ID, slot)
		body, found, err := docs.getDocument(key)
		if err != nil {
			return nil, rejectLocalResultPersistence(err)
		}
		if !found {
			continue
		}
		if slot != 0 {
			completed, _, err := c.loadFanoutLeafCompletion(ctx, item, slot-1)
			if err != nil {
				return nil, err
			}
			if completed != nil {
				continue
			}
		}
		return &externalResultCorrectionReservation{start: *start, slot: slot, key: key, body: body}, nil
	}
	return nil, nil
}

func (c *Coordinator) restoreExternalCorrectionForReconciliation(ctx context.Context, item *TodoItem, reservation *externalResultCorrectionReservation) error {
	start, slot := reservation.start, reservation.slot
	docs := workspaceDocs(c.artifactStoreRootPath())
	var checkpoint externalResultCorrectionCheckpoint
	if err := json.Unmarshal(reservation.body, &checkpoint, json.RejectUnknownMembers(true)); err != nil {
		return rejectResultSourceRead(err)
	}
	receipt := &checkpoint.Receipt
	proof := receipt.RepairProvenance
	target := item.ExecutionTarget
	var binding *durableSessionBinding
	var err error
	if slot != 0 {
		target = item.ExecutionTopology[slot-1]
		binding, err = c.readFanoutBackendSession(item, slot-1)
	} else {
		binding, err = c.durableBackendSessionBinding(ctx, item.ID)
	}
	if err != nil {
		return rejectResultSourceRead(err)
	}
	if checkpoint.BranchID != start.BranchID || checkpoint.StartEventID != start.ID || checkpoint.DispatchID == "" ||
		checkpoint.Revision != item.OccurrenceRevision || checkpoint.Slot != slot || receipt.TaskID != item.ID ||
		receipt.RunID == "" || receipt.RunID != start.RunID || receipt.Attempt <= 0 ||
		(slot == 0 && eventDispatchAttempt(start) != receipt.Attempt) || receipt.OccurrenceAttempt != item.Retries+1 ||
		receipt.ExecutionTarget != receiptExecutionTarget(target) || binding == nil || receipt.ProviderSessionID == "" ||
		receipt.RunInputSnapshotID != item.RunInputSnapshotID || receipt.RunInputSnapshotHash != item.RunInputSnapshotHash ||
		receipt.WorkSubmissionFailure != RepairFailureInvalidSchema || proof == nil || !proof.Attempted ||
		proof.RepairAttempts != 1 || receipt.ProviderTurnID == "" || proof.ProviderWorkTurnID != receipt.ProviderTurnID ||
		proof.ProviderWorkTranscriptRef != receipt.ProviderTranscriptRef {
		return rejectResultSourceRead(errTaskResultStale)
	}
	if err := backendSessionConflict(item.ID, binding, receipt.Backend, receipt.ProviderSessionID); err != nil {
		return rejectResultSourceRead(err)
	}
	store, err := NewFileArtifactStore(string(docs), string(docs))
	if err != nil {
		return rejectResultSourceRead(err)
	}
	if _, err := verifyCodexTranscriptArtifact(ctx, store, receipt.ProviderTranscriptRef, receipt.Backend, receipt.RunID, item.ID, receipt.Attempt); err != nil {
		return rejectResultSourceRead(err)
	}
	if err := restoreExternalCorrectionResponse(docs, reservation.key, receipt); err != nil {
		return err
	}
	if err := c.setAttemptReceipt(item.ID, receipt); err != nil {
		return err
	}
	status := TaskBlocked
	detail := "interrupted external result correction retained the original work and consumed its single correction allowance"
	return c.persistFailureWithOutput(item.Agent, item.Desc, item.ID, detail, ReconcileOnly, FailureProtocol, &status, checkpoint.Output)
}

func restoreExternalCorrectionResponse(docs workspaceDocs, key string, receipt *ExecutionReceipt) error {
	proof := receipt.RepairProvenance
	turn, _, err := docs.getDocument(key + "/turn")
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	proof.ProviderCorrectionTurnID = string(turn)
	if proof.ProviderCorrectionTurnID == receipt.ProviderTurnID {
		return rejectResultSourceRead(errTaskResultStale)
	}
	candidate, returned, err := docs.getDocument(key + "/response")
	if err != nil || !returned {
		return rejectLocalResultPersistence(err)
	}
	var response externalResultCorrectionResponse
	if err := json.Unmarshal(candidate, &response, json.RejectUnknownMembers(true)); err != nil {
		return rejectResultSourceRead(err)
	}
	if response.TurnID == "" || response.TurnID != proof.ProviderCorrectionTurnID || response.Result == nil ||
		response.Result.TaskID != receipt.TaskID || response.Result.Attempt != receipt.Attempt ||
		response.Result.Agent != receipt.ProducerID || response.Result.Source != ExternalProviderProposalSource {
		return rejectResultSourceRead(errTaskResultStale)
	}
	digest, err := collectionDigest(response.Result)
	if err != nil || digest != response.ResultSHA256 {
		return rejectResultSourceRead(errTaskResultStale)
	}
	proof.PreparedCandidateSHA256 = digest
	proof.SubmittedResult = response.Result
	return nil
}
