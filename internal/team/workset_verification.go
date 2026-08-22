package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (c *Coordinator) executeWorksetCompleteVerification(ctx context.Context, spec VerificationSpec) (*VerificationResult, error) {
	res := &VerificationResult{WorkDir: c.verificationWorkDir(), Spec: &spec, ExitCode: 1}
	if err := validateVerificationSpec(spec); err != nil {
		res.Stderr = "workset_complete malformed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, res.WorkDir)
		return res, err
	}
	receipt, err := c.findWorksetReceipt(spec.WorksetSourceTask)
	if err != nil {
		res.Stderr = "workset_complete failed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, res.WorkDir)
		return applyVerificationMode(res, err, spec.Mode)
	}
	if err := c.verifyWorksetSource(ctx, receipt); err != nil {
		res.Stderr = "workset_complete failed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, res.WorkDir)
		return applyVerificationMode(res, err, spec.Mode)
	}
	if receipt.ItemCount <= 0 || len(receipt.Children) != receipt.ItemCount {
		err := fmt.Errorf("workset %q has invalid receipt cardinality: expected %d child mapping(s), got %d", receipt.WorksetID, receipt.ItemCount, len(receipt.Children))
		res.Stderr = "workset_complete failed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, res.WorkDir)
		return applyVerificationMode(res, err, spec.Mode)
	}
	accepted := make(map[string]struct{}, len(spec.WorksetAcceptedStatuses))
	for _, status := range spec.WorksetAcceptedStatuses {
		accepted[strings.ToLower(strings.TrimSpace(status))] = struct{}{}
	}
	items := c.taskTracker.TodoList().Items()
	seenChildren := make(map[string]struct{}, len(receipt.Children))
	var failures []string
	completed, verified := 0, 0
	for key, childID := range receipt.Children {
		item := findTodoItem(items, childID)
		if item == nil || item.WorksetBinding == nil || item.WorksetBinding.WorksetID != receipt.WorksetID || item.WorksetBinding.ItemKey != key {
			failures = append(failures, fmt.Sprintf("item %q is not bound to child task %q", key, childID))
			continue
		}
		seenChildren[item.ID] = struct{}{}
		if spec.WorksetRequireTerminal && !isTerminalTaskStatus(item.Status) {
			failures = append(failures, fmt.Sprintf("item %q is not terminal (%s)", key, item.Status))
			continue
		}
		if item.TypedResult == nil {
			failures = append(failures, fmt.Sprintf("item %q has no canonical task result", key))
			continue
		}
		if _, ok := accepted[strings.ToLower(strings.TrimSpace(item.TypedResult.Status))]; !ok {
			failures = append(failures, fmt.Sprintf("item %q has unaccepted result status %q", key, item.TypedResult.Status))
			continue
		}
		if item.Status != TaskDone {
			failures = append(failures, fmt.Sprintf("item %q has non-success task state %s", key, item.Status))
			continue
		}
		if item.VerifyResult != nil && !isVerifySuccess(item.VerifyResult) {
			failures = append(failures, fmt.Sprintf("item %q objective verification failed", key))
			continue
		}
		if spec.WorksetRequireVerified && !isVerifySuccess(item.VerifyResult) {
			failures = append(failures, fmt.Sprintf("item %q has no successful objective verification", key))
			continue
		}
		completed++
		if isVerifySuccess(item.VerifyResult) {
			verified++
		}
	}
	for _, item := range items {
		if item != nil && item.WorksetBinding != nil && item.WorksetBinding.WorksetID == receipt.WorksetID {
			if _, ok := seenChildren[item.ID]; !ok {
				failures = append(failures, fmt.Sprintf("unbound child task %q is present", item.ID))
			}
		}
	}
	if len(failures) > 0 {
		err := errors.New(strings.Join(failures, "; "))
		res.Stderr = "workset_complete failed: " + err.Error()
		res.Fingerprint = ComputeVerificationFingerprint(spec, res, res.WorkDir)
		return applyVerificationMode(res, err, spec.Mode)
	}
	res.ExitCode = 0
	res.Stdout = fmt.Sprintf("workset_complete passed: %d/%d completed, %d/%d verified", completed, receipt.ItemCount, verified, receipt.ItemCount)
	res.Fingerprint = ComputeVerificationFingerprint(spec, res, res.WorkDir)
	return applyVerificationMode(res, nil, spec.Mode)
}

func (c *Coordinator) findWorksetReceipt(sourceTask string) (*WorksetExpansionReceipt, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil, errors.New("coordinator task state is unavailable")
	}
	logicalSourceID := normalizeTaskReferenceID(sourceTask)
	if logicalSourceID == "" {
		return nil, fmt.Errorf("no expansion receipt found for source-task %q", sourceTask)
	}
	visible := make([]*WorksetExpansionReceipt, 0)
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.WorksetReceipt != nil {
			visible = append(visible, item.WorksetReceipt)
		}
	}
	receipts, conflicts := collectWorksetReceipts(visible)
	if err := worksetReceiptConflictError(conflicts); err != nil {
		return nil, err
	}
	matches := make(map[string]*WorksetExpansionReceipt)
	for worksetID, receipt := range receipts {
		if normalizeTaskReferenceID(receipt.ParentTaskID) != logicalSourceID {
			continue
		}
		matches[worksetID] = receipt
	}
	if len(matches) == 1 {
		for _, receipt := range matches {
			return cloneWorksetReceipt(receipt), nil
		}
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("source-task %q resolves to ambiguous workset receipts", sourceTask)
	}
	return nil, fmt.Errorf("no expansion receipt found for source-task %q", sourceTask)
}

func (c *Coordinator) verifyWorksetSource(ctx context.Context, receipt *WorksetExpansionReceipt) error {
	if receipt == nil || strings.TrimSpace(receipt.SourceArtifactID) == "" || strings.TrimSpace(receipt.SourceSHA256) == "" {
		return errors.New("expansion receipt has no source artifact identity")
	}
	if c == nil || c.session == nil {
		return errors.New("coordinator workspace is unavailable")
	}
	if strings.TrimSpace(receipt.RunID) == "" || strings.TrimSpace(c.executionRunID) == "" || receipt.RunID != c.executionRunID {
		return fmt.Errorf("workset receipt belongs to run %q, want current run %q", receipt.RunID, c.executionRunID)
	}
	producerRef, _, err := c.currentWorksetSourceOccurrence(receipt)
	if err != nil {
		return err
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return err
	}
	// CAS metadata is immutable first-writer content metadata. It is not the
	// authority for producer occurrence provenance, which was validated above
	// from the current canonical typed result.
	ref, err := store.Resolve(ctx, producerRef)
	if err != nil {
		return fmt.Errorf("source artifact %q is not registered: %w", receipt.SourceArtifactID, err)
	}
	if ref.ID != receipt.SourceArtifactID || ref.SHA256 != receipt.SourceSHA256 {
		return fmt.Errorf("source artifact %q immutable identity changed", receipt.SourceArtifactID)
	}
	return nil
}

// currentWorksetSourceOccurrence resolves occurrence authority from the
// canonical producer result, not from the workset receipt's parent task. The
// parent identifies the expansion group; the source artifact's committed
// reference identifies the producer task. This also supports replay seams
// where the canonical result is present in the coordinator cache without a
// corresponding Todo projection.
func (c *Coordinator) currentWorksetSourceOccurrence(receipt *WorksetExpansionReceipt) (ArtifactRef, string, error) {
	if c == nil || receipt == nil {
		return ArtifactRef{}, "", errors.New("workset source occurrence is unavailable")
	}
	if err := validateWorksetSourceOccurrence(receipt.SourceArtifact, receipt.SourceArtifactID, receipt.SourceSHA256, true); err != nil {
		return ArtifactRef{}, "", fmt.Errorf("source artifact %q has invalid persisted occurrence: %w", receipt.SourceArtifactID, err)
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return ArtifactRef{}, "", err
	}
	if _, err := resolveWorksetArtifactContent(context.Background(), store, receipt.SourceArtifact); err != nil {
		return ArtifactRef{}, "", fmt.Errorf("source artifact %q has invalid persisted content claims: %w", receipt.SourceArtifactID, err)
	}
	producerTaskID := receipt.SourceArtifact.TaskID
	result := c.GetTaskResult(producerTaskID)
	ref, err := resolveTaskResultArtifact(result, receipt.SourceArtifactID)
	if err != nil {
		return ArtifactRef{}, "", fmt.Errorf("source artifact %q is not declared by its intended canonical producer task %q: %w", receipt.SourceArtifactID, producerTaskID, err)
	}
	if err := c.validateCurrentProducerArtifactOccurrence(ref, producerTaskID, result); err != nil {
		return ArtifactRef{}, "", fmt.Errorf("source artifact %q has invalid current producer occurrence: %w", receipt.SourceArtifactID, err)
	}
	if _, err := resolveWorksetArtifactContent(context.Background(), store, ref); err != nil {
		return ArtifactRef{}, "", fmt.Errorf("source artifact %q has invalid current producer content claims: %w", receipt.SourceArtifactID, err)
	}
	if !sameArtifactOccurrence(ref, receipt.SourceArtifact) {
		return ArtifactRef{}, "", fmt.Errorf("source artifact %q current producer occurrence does not match expansion receipt", receipt.SourceArtifactID)
	}
	return ref, producerTaskID, nil
}

func validateWorksetSourceOccurrence(ref ArtifactRef, artifactID, digest string, requireProducer bool) error {
	if strings.TrimSpace(ref.ID) == "" || ref.ID != artifactID {
		return fmt.Errorf("artifact id does not match persisted identity")
	}
	if strings.TrimSpace(ref.SHA256) == "" || ref.SHA256 != digest {
		return fmt.Errorf("artifact digest does not match persisted identity")
	}
	if requireProducer && (strings.TrimSpace(ref.RunID) == "" || strings.TrimSpace(ref.TaskID) == "" || ref.Attempt <= 0 || strings.TrimSpace(ref.Agent) == "") {
		return fmt.Errorf("producer run, task, attempt, and agent are required")
	}
	if ref.Bytes < 0 || ref.ByteSize < 0 || ref.Bytes != ref.ByteSize {
		return fmt.Errorf("artifact byte counts are inconsistent")
	}
	return nil
}

// resolveWorksetArtifactContent applies the workset-only closed content claim
// contract. FileArtifactStore.Resolve intentionally treats zero values as
// omitted claims for unrelated consumers; workset sources must distinguish a
// verified zero-byte object from a missing claim and therefore compare both
// size fields exactly after immutable CAS verification.
func resolveWorksetArtifactContent(ctx context.Context, store *FileArtifactStore, supplied ArtifactRef) (ArtifactRef, error) {
	if store == nil {
		return ArtifactRef{}, errors.New("workset artifact store is unavailable")
	}
	if strings.TrimSpace(supplied.ID) == "" || strings.TrimSpace(supplied.SHA256) == "" {
		return ArtifactRef{}, errors.New("workset artifact requires immutable id and sha256")
	}
	if supplied.Bytes < 0 || supplied.ByteSize < 0 || supplied.Bytes != supplied.ByteSize {
		return ArtifactRef{}, errors.New("workset artifact byte counts are inconsistent")
	}
	canonical, err := store.Resolve(ctx, supplied)
	if err != nil {
		return ArtifactRef{}, err
	}
	if supplied.Bytes != canonical.Bytes || supplied.ByteSize != canonical.ByteSize {
		return ArtifactRef{}, fmt.Errorf("workset artifact byte count claims (%d/%d) do not match verified CAS metadata (%d/%d)", supplied.Bytes, supplied.ByteSize, canonical.Bytes, canonical.ByteSize)
	}
	return canonical, nil
}
