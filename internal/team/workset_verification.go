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
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.WorksetReceipt == nil {
			continue
		}
		receipt := item.WorksetReceipt
		if receipt.ParentTaskID == sourceTask || item.ID == sourceTask || item.PlanTaskID == sourceTask {
			return cloneWorksetReceipt(receipt), nil
		}
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
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return err
	}
	ref, err := store.Get(ctx, receipt.SourceArtifactID)
	if err != nil {
		return fmt.Errorf("source artifact %q is not registered: %w", receipt.SourceArtifactID, err)
	}
	if ref.SHA256 != receipt.SourceSHA256 {
		return fmt.Errorf("source artifact %q digest changed", receipt.SourceArtifactID)
	}
	if receipt.RunID != "" && ref.RunID != "" && ref.RunID != receipt.RunID {
		return fmt.Errorf("source artifact %q belongs to run %q, want %q", receipt.SourceArtifactID, ref.RunID, receipt.RunID)
	}
	return store.Verify(ctx, ref)
}
