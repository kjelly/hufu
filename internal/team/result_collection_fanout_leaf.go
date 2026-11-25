package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

type fanoutLeafCompletion struct {
	SlotSHA256    string           `json:"slot_sha256"`
	Output        string           `json:"output"`
	OutputSHA256  string           `json:"output_sha256"`
	Receipt       ExecutionReceipt `json:"receipt"`
	ReceiptSHA256 string           `json:"receipt_sha256"`
	pendingBody   []byte
}

func fanoutSlotName(index int) string {
	if index == 0 {
		return "main"
	}
	return fmt.Sprintf("extra-%d", index)
}

func (c *Coordinator) fanoutOccurrenceDigest(item *TodoItem) (string, error) {
	if item == nil || item.Status != TaskInProgress {
		return "", errResultCollectionStale
	}
	return collectionDigest(struct {
		Branch, Task      string
		Revision, Retries int
	}{c.activeBranchID(), item.ID, item.OccurrenceRevision, item.Retries})
}

func (c *Coordinator) fanoutLeafDocument(item *TodoItem, index int) (string, string, error) {
	occurrence, err := c.fanoutOccurrenceDigest(item)
	if err != nil || index < 0 || index >= min(len(item.ExecutionTopology), maxConcurrentModels) {
		return "", "", errResultCollectionStale
	}
	digest, err := collectionDigest(struct {
		Occurrence string
		Index      int
		Target     execution.ExecutionTarget
	}{occurrence, index, item.ExecutionTopology[index]})
	return "result-collection-fanout-leaf:" + digest, digest, err
}

// An external leaf keeps its legacy result adapter. In mixed fanout, seal
// that adapter's terminal output before the parent can depend on the leaf.
func (c *Coordinator) fanoutLeafNeedsCompletion(def *agent.AgentDef, task TaskDef, target execution.ExecutionTarget) (bool, error) {
	limit := min(len(task.ExecutionTopology), maxConcurrentModels)
	if limit < 2 || !c.hasDurableEventJournal() {
		return false, nil
	}
	registry := c.ExecutionRegistry()
	backend, err := registry.ResolveBackend(target.Backend)
	if err != nil || backend.Kind() != execution.BackendKindAgent {
		return false, err
	}
	for _, candidate := range task.ExecutionTopology[:limit] {
		probe := task
		probe.ResolvedExecutionTarget = candidate
		transport, err := c.resultTransportForTask(def, probe)
		if err != nil {
			return false, err
		}
		if taskUsesResultCollection(probe, transport) {
			return true, nil
		}
	}
	return false, nil
}

// Local collection acceptance and external result admission both precede
// deliverable checks. The terminal handoff records the result after those
// checks so an interrupted sibling or parent cannot cause them to run again.
func (c *Coordinator) sealFanoutLeafCompletion(ctx context.Context, item *TodoItem, index int, output string, receipt ExecutionReceipt) error {
	name, slot, err := c.fanoutLeafDocument(item, index)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	if err := c.validateFanoutLeafReceipt(context.WithoutCancel(ctx), item, index, &receipt, true); err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	digest, err := collectionDigest(&receipt)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	completion := fanoutLeafCompletion{
		SlotSHA256: slot, Output: output, OutputSHA256: sha256HexBytes([]byte(output)),
		Receipt: receipt, ReceiptSHA256: digest,
	}
	body, err := json.Marshal(completion, json.Deterministic(true), collectionDurationFormat)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	if err := c.stageFanoutDocument(context.WithoutCancel(ctx), name, body); err != nil {
		return err
	}
	completion.pendingBody = body
	return c.publishFanoutLeafCompletion(ctx, item, index, &completion)
}

func (c *Coordinator) publishFanoutLeafCompletion(ctx context.Context, item *TodoItem, index int, completion *fanoutLeafCompletion) error {
	if completion.pendingBody == nil {
		return nil
	}
	name, _, err := c.fanoutLeafDocument(item, index)
	if err != nil {
		return err
	}
	receipt := &completion.Receipt
	workspace := c.artifactStoreRootPath()
	var refs []ArtifactRef
	if receipt.ResultCollection == nil {
		refs = externalFanoutArtifacts(receipt.AcceptedResult)
	}
	withCodexTranscript := isCodexFanoutTranscript(receipt.ProviderTranscriptRef)
	if len(refs) > 0 || withCodexTranscript {
		store, err := NewFileArtifactStore(workspace, workspace)
		if err != nil {
			return rejectLocalResultPersistence(err)
		}
		for _, ref := range refs {
			body, err := readExternalFanoutArtifact(ctx, workspace, receipt, ref)
			if err != nil {
				return rejectNormalizationRecoveryRead(err)
			}
			if err := publishExternalFanoutArtifact(store, ref, body); err != nil {
				return rejectLocalResultPersistence(err)
			}
		}
		if withCodexTranscript {
			body, err := readCodexFanoutTranscript(ctx, workspace, receipt)
			if err != nil {
				return rejectNormalizationRecoveryRead(err)
			}
			if err := putCodexTranscript(ctx, store, receipt.ProviderTranscriptRef, receipt.ExecutionTarget.Backend, receipt.RunID, receipt.TaskID, receipt.Attempt, body, receipt.FinishedAt); err != nil {
				return rejectLocalResultPersistence(err)
			}
		}
	}
	if err := c.validateFanoutLeafReceipt(ctx, item, index, receipt, false); err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	if err := c.publishFanoutDocument(ctx, name, completion.pendingBody); err != nil {
		return err
	}
	completion.pendingBody = nil
	for _, ref := range refs {
		removeFanoutStage(workspace, externalFanoutArtifactKey(receipt.ModelExecutionID, ref.ID))
	}
	if withCodexTranscript {
		removeFanoutStage(workspace, codexFanoutTranscriptKey(receipt.ModelExecutionID, receipt.Attempt))
	}
	return nil
}

func (c *Coordinator) readFanoutLeafCompletion(ctx context.Context, item *TodoItem, index int) (*fanoutLeafCompletion, string, error) {
	completion, digest, err := c.loadFanoutLeafCompletion(ctx, item, index)
	if err == nil && completion == nil {
		err = rejectLocalResult(RepairFailureSource, fmt.Errorf("fanout slot %d has no sealed terminal result: %w", index, errResultCollectionStale))
	}
	return completion, digest, err
}

func (c *Coordinator) loadFanoutLeafCompletion(ctx context.Context, item *TodoItem, index int) (*fanoutLeafCompletion, string, error) {
	name, slot, err := c.fanoutLeafDocument(item, index)
	if err != nil {
		return nil, "", err
	}
	body, pending, err := c.readFanoutDocument(ctx, name)
	if err != nil || body == nil {
		return nil, "", err
	}
	var completion fanoutLeafCompletion
	if err := json.Unmarshal(body, &completion, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil ||
		completion.SlotSHA256 != slot || completion.OutputSHA256 != sha256HexBytes([]byte(completion.Output)) {
		return nil, "", errResultCollectionStale
	}
	if digest, err := collectionDigest(&completion.Receipt); err != nil || digest != completion.ReceiptSHA256 {
		return nil, "", errResultCollectionStale
	}
	if err := c.validateFanoutLeafReceipt(ctx, item, index, &completion.Receipt, pending); err != nil {
		return nil, "", err
	}
	if pending {
		completion.pendingBody = body
	}
	return &completion, sha256HexBytes(body), nil
}

func (c *Coordinator) validateFanoutLeafReceipt(ctx context.Context, item *TodoItem, index int, receipt *ExecutionReceipt, pending bool) error {
	if receipt.ResultCollection != nil {
		return c.validateCollectedFanoutLeafReceipt(ctx, item, index, receipt)
	}
	target := item.ExecutionTopology[index]
	modelExecutionID := modelFanoutExecutionID(receipt.RunID, item.ID, item.Agent, target.String(), fanoutSlotName(index), item.Retries)
	if receipt.RunID == "" || receipt.TaskID != item.ID || receipt.OccurrenceAttempt != item.Retries+1 ||
		receipt.Attempt < 1 || receipt.ExecutionTarget != target || receipt.ProducerID != item.Agent ||
		receipt.ModelExecutionID != modelExecutionID || receipt.FinishedAt.IsZero() ||
		receipt.ExitCode == nil || *receipt.ExitCode != 0 || !validFanoutLeafResult(item, receipt) {
		return errResultCollectionStale
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return err
	}
	if !collectionStartMatches(receipt.RunID, c.activeBranchID(), item, 0, events) {
		return errResultCollectionStale
	}
	binding, err := c.readFanoutBackendSession(item, index)
	if err != nil {
		return err
	}
	if binding == nil || receipt.ProviderSessionID != binding.Binding.SessionID {
		return errResultCollectionStale
	}
	if receipt.TranscriptRef != "" {
		if err := c.validateResumeTranscript(ctx, receipt); err != nil {
			return err
		}
	}
	if err := c.validateFanoutProviderTranscript(ctx, receipt, pending); err != nil {
		return err
	}
	return c.verifyFanoutLeafEvidence(ctx, item, receipt, events, pending)
}

func (c *Coordinator) validateCollectedFanoutLeafReceipt(ctx context.Context, item *TodoItem, index int, receipt *ExecutionReceipt) error {
	cp := currentFanoutCollection(item, index)
	if cp == nil || cp.State != ResultCollectionCommitted || !sameCanonicalEvidence(cp, receipt.ResultCollection) ||
		receipt.TaskID != item.ID || receipt.RunID != cp.SourceRunID || receipt.Attempt != cp.Attempt ||
		receipt.ModelExecutionID != cp.ModelExecutionID || receipt.ExecutionTarget != cp.ExecutionTarget ||
		!validFanoutLeafResult(item, receipt) {
		return errResultCollectionStale
	}
	result, err := c.readAcceptedCollectionResult(ctx, item, cp)
	if err != nil {
		return err
	}
	projection, err := c.verifyCollectedWorkEvidence(ctx, cp, receipt.AcceptedResult, receipt)
	if err != nil {
		return err
	}
	if receipt.AcceptedResult.RuntimeMedia != nil {
		attachTaskRuntimeMedia(result, projection)
	}
	if !sameCanonicalEvidence(result, receipt.AcceptedResult) {
		return errResultCollectionStale
	}
	return c.verifyCollectedFanoutVerification(ctx, item, cp, receipt, result, projection)
}

func (c *Coordinator) verifyCollectedFanoutVerification(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint, receipt *ExecutionReceipt, result *TaskResult, projection *RuntimeMediaProjection) error {
	objective := item.Verify != "" || item.VerifySpec != nil
	if !objective && item.AdversarialVerify == 0 {
		return nil
	}
	if objective && receipt.VerifyResult == nil {
		return fmt.Errorf("fanout verification receipt is missing: %w", errResultCollectionStale)
	}
	work, err := c.loadCollectionWork(ctx, item, cp)
	if err != nil {
		return err
	}
	checkpoint, err := c.readFanoutCheckProgress(item, cp, work, result, projection)
	if err != nil {
		return err
	}
	if checkpoint == nil || !checkpoint.verified(item, receipt.VerifyResult) {
		return fmt.Errorf("fanout receipt does not match completed verification: %w", errResultCollectionStale)
	}
	return nil
}

func (c *Coordinator) verifyFanoutLeafEvidence(ctx context.Context, item *TodoItem, receipt *ExecutionReceipt, events []RunEvent, pending bool) error {
	result := receipt.AcceptedResult
	if err := verifyRuntimeMediaResult(result, receipt, events); err != nil {
		return err
	}
	identity := submitResultRuntimeIdentity{RunID: receipt.RunID, TaskID: item.ID, Attempt: receipt.Attempt, Agent: item.Agent}
	proposed := externalFanoutArtifacts(result)
	if err := validateFanoutExternalArtifactSizes(proposed); err != nil {
		return err
	}
	root := c.artifactStoreRootPath()
	published := result.Artifacts
	if pending {
		published = slices.DeleteFunc(slices.Clone(published), func(ref ArtifactRef) bool { return !isMediaArtifactID(ref.ID) })
		for _, ref := range proposed {
			if _, err := readExternalFanoutArtifact(ctx, root, receipt, ref); err != nil {
				return err
			}
		}
	}
	if err := c.verifyNormalizedArtifactRefs(ctx, identity, published, result.RuntimeMedia.artifacts()); err != nil {
		return err
	}
	if err := verifyExternalResultArtifactMetadata(ctx, root, published); err != nil {
		return err
	}
	if err := verifyExternalResultScopedReads(ctx, root, receipt, result.FilesRead); err != nil {
		return err
	}
	return c.validateRequiredEvidenceDeliveryForResult(ctx, item.ID, receipt.Attempt, result, receipt.RunID)
}

func externalFanoutArtifacts(result *TaskResult) []ArtifactRef {
	var proposed []ArtifactRef
	for _, ref := range result.Artifacts {
		if !isMediaArtifactID(ref.ID) {
			proposed = append(proposed, ref)
		}
	}
	return proposed
}

func verifyExternalResultArtifactMetadata(ctx context.Context, root string, refs []ArtifactRef) error {
	if len(refs) == 0 {
		return nil
	}
	store, err := OpenFileArtifactStoreReadOnly(root, root)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		stored, err := store.Get(ctx, ref.ID)
		if err != nil {
			return err
		}
		if !sameArtifactContentMetadata(stored, ref) {
			return fmt.Errorf("external result artifact %q conflicts with immutable metadata", ref.ID)
		}
	}
	return nil
}

func verifyExternalResultScopedReads(ctx context.Context, root string, receipt *ExecutionReceipt, refs []FileRef) error {
	request := AttemptRequest{RunID: receipt.RunID, TaskID: receipt.TaskID, Attempt: receipt.Attempt, ArtifactScope: receipt.ArtifactScope}
	if err := validateAttemptArtifactScope(request); err != nil {
		return err
	}
	if request.ArtifactScope.StoreRoot != root {
		return fmt.Errorf("external result artifact scope differs from the runtime store")
	}
	for _, ref := range refs {
		if ref.Purpose == "" && !looksLikeOpaqueArtifactID(ref.Path) {
			continue
		}
		resolved, handled, err := canonicalScopedFileRef(ctx, request, ref.Path, root)
		if err != nil {
			return err
		}
		if !handled || resolved != ref {
			return errResultCollectionStale
		}
	}
	return nil
}

func validFanoutLeafResult(item *TodoItem, receipt *ExecutionReceipt) bool {
	result := receipt.AcceptedResult
	if result == nil || result.TaskID != item.ID || result.Agent != item.Agent || result.Attempt != receipt.Attempt || !taskResultStatusIsSuccessful(result.Status) {
		return false
	}
	payload := result.StructuredPayload
	if payload == nil {
		return item.ResultContract == nil || !item.ResultContract.RequireStructured
	}
	return item.ResultContract != nil && payload.Contract == *item.ResultContract &&
		receipt.ResultContractID == payload.Contract.ID && receipt.ResultContractSHA256 == payload.Contract.SchemaSHA256 &&
		receipt.ResultValidation == ResultValidationValid && payload.SHA256 == sha256HexBytes(payload.Value) &&
		receiptMatchesResultPayload(receipt, payload)
}

// A completed external leaf also prevents a whole-fanout redispatch when all
// local siblings stopped before sealing their work.
func (c *Coordinator) incompleteMixedFanout(ctx context.Context, item *TodoItem, task TaskDef) error {
	limit := min(len(task.ExecutionTopology), maxConcurrentModels)
	if limit < 2 || item.Status != TaskInProgress {
		return nil
	}
	def, _, err := c.AgentPool().ResolveAgentName(item.Agent)
	if err != nil {
		return err
	}
	for index, target := range task.ExecutionTopology[:limit] {
		needsSeal, err := c.fanoutLeafNeedsCompletion(def, task, target)
		if err != nil {
			return err
		}
		if !needsSeal {
			continue
		}
		binding, err := c.readFanoutBackendSession(item, index)
		if err != nil {
			return err
		}
		if binding != nil || (item.BackendBinding != nil && item.BackendBinding.SessionID != "") {
			return fmt.Errorf("mixed fanout has a reserved external session and no sealed local work: %w", errResultCollectionStale)
		}
		if _, _, err := c.loadFanoutLeafCompletion(ctx, item, index); err != nil {
			return err
		}
	}
	return nil
}
