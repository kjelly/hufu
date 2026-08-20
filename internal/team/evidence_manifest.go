package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (c *Coordinator) buildEvidenceManifest(ctx context.Context, strict bool) (*EvidenceManifest, error) {
	if c == nil || c.session == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil, fmt.Errorf("evidence manifest requires a task tracker")
	}
	runID := c.executionRunID
	if runID == "" {
		runID = "run-unknown"
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil, err
	}
	manifest := &EvidenceManifest{RunID: runID, Status: "accepted"}
	completedCount := 0
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil {
			continue
		}
		result, refs, failed, err := c.buildTaskEvidence(ctx, store, runID, item, strict)
		if err != nil {
			return nil, err
		}
		if item.Status != TaskDone {
			manifest.Status = "failed"
		} else {
			completedCount++
		}
		if failed {
			manifest.Status = "failed"
		}
		manifest.ArtifactRefs = append(manifest.ArtifactRefs, refs...)
		manifest.EvidenceResults = append(manifest.EvidenceResults, result)
	}
	if completedCount == 0 && len(c.taskTracker.TodoList().Items()) > 0 {
		manifest.Status = "failed"
		if strict {
			return nil, fmt.Errorf("no completed tasks with evidence")
		}
	} else if completedCount == 0 && strict {
		return nil, fmt.Errorf("no completed tasks with evidence")
	}
	if err := manifest.Seal(); err != nil {
		return nil, err
	}
	if err := manifest.Verify(ctx, store); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(c.session.Workspace, logsDir, "evidence_manifest.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return nil, fmt.Errorf("persist evidence manifest: %w", err)
	}
	c.lastEvidenceManifestMu.Lock()
	c.lastEvidenceManifest = manifest
	c.lastEvidenceManifestMu.Unlock()
	return manifest, nil
}

func (c *Coordinator) buildTaskEvidence(ctx context.Context, store *FileArtifactStore, runID string, item *TodoItem, strict bool) (EvidenceResult, []ArtifactRef, bool, error) {
	result := EvidenceResult{RequirementID: "task:" + item.ID, Validator: "task-verification", CheckedAt: nowUTC()}
	if item.Status != TaskDone {
		result.Status = "failed"
		var refs []ArtifactRef
		if receipt := latestExecutionReceiptWithTranscript(item, runID); receipt != nil {
			ref, err := transcriptEvidenceRef(ctx, store, item, receipt, runID)
			if err != nil {
				if strict {
					return EvidenceResult{}, nil, true, fmt.Errorf("failed task %q transcript evidence: %w", item.ID, err)
				}
			} else {
				refs = append(refs, ref)
				result.ArtifactRefs = append(result.ArtifactRefs, ref)
			}
		}
		return result, refs, true, nil
	}

	var refs []ArtifactRef
	if item.VerifyResult != nil && item.VerifyResult.ExitCode == 0 {
		result.Status = "passed"
	} else if item.TypedResult != nil && len(item.TypedResult.Evidence) > 0 {
		result.Status = "passed"
	} else {
		receipt := latestSuccessfulExecutionReceipt(item, runID)
		if receipt == nil || strings.TrimSpace(receipt.TranscriptRef) == "" {
			result.Status = "failed"
			if strict {
				return EvidenceResult{}, nil, true, fmt.Errorf("completed task %q (%s) is missing verification evidence", item.ID, item.Desc)
			}
		} else if ref, err := transcriptEvidenceRef(ctx, store, item, receipt, runID); err != nil {
			result.Status = "failed"
			if strict {
				return EvidenceResult{}, nil, true, fmt.Errorf("task %q transcript evidence: %w", item.ID, err)
			}
		} else {
			result.Status = "passed"
			refs = append(refs, ref)
			result.ArtifactRefs = append(result.ArtifactRefs, ref)
		}
	}
	// A successful verifier or typed result still needs the runner-owned
	// transcript in the exact task membership. Without this canonical addition,
	// a receipt-backed task with no separately declared artifacts reaches
	// binding with an empty ArtifactRefs set and is rejected as incomplete.
	if result.Status == "passed" {
		if receipt := latestSuccessfulExecutionReceipt(item, runID); receipt != nil {
			ref, err := transcriptEvidenceRef(ctx, store, item, receipt, runID)
			if err != nil {
				result.Status = "failed"
				if strict {
					return EvidenceResult{}, nil, true, fmt.Errorf("task %q transcript evidence: %w", item.ID, err)
				}
			} else if !containsArtifactID(refs, ref.ID) {
				refs = append(refs, ref)
				result.ArtifactRefs = append(result.ArtifactRefs, ref)
			}
		}
	}

	failed := false
	artifactRefs, artifactFailed, err := taskArtifactRefs(ctx, store, runID, item, strict)
	if err != nil {
		return EvidenceResult{}, nil, true, err
	}
	existingRefs := append([]ArtifactRef(nil), refs...)
	for _, ref := range artifactRefs {
		if containsArtifactID(existingRefs, ref.ID) {
			continue
		}
		refs = append(refs, ref)
		existingRefs = append(existingRefs, ref)
		result.ArtifactRefs = append(result.ArtifactRefs, ref)
	}
	if artifactFailed {
		result.Status = "failed"
	}
	failed = artifactFailed || result.Status == "failed"
	if result.Status == "passed" {
		if err := bindEvidenceResult(runID, item, latestSuccessfulExecutionReceipt(item, runID), &result); err != nil {
			// Binding corruption is a terminal evidence failure even in the
			// non-strict/report path. "unverified" is reserved for an otherwise
			// successful run whose acceptance gate was not configured.
			result.Status = "failed"
			failed = true
			if strict {
				return EvidenceResult{}, nil, true, fmt.Errorf("task %q execution binding: %w", item.ID, err)
			}
		}
	}
	return result, refs, failed, nil
}

func containsArtifactID(refs []ArtifactRef, id string) bool {
	if id == "" {
		return false
	}
	for _, ref := range refs {
		if ref.ID == id {
			return true
		}
	}
	return false
}

func taskArtifactRefs(ctx context.Context, store *FileArtifactStore, runID string, item *TodoItem, strict bool) ([]ArtifactRef, bool, error) {
	if item.TypedResult == nil {
		return nil, false, nil
	}
	var refs []ArtifactRef
	failed := false
	attempt := item.Retries + 1
	if receipt := latestSuccessfulExecutionReceipt(item, runID); receipt != nil && receipt.Attempt > 0 {
		attempt = receipt.Attempt
	}
	for _, artifact := range item.TypedResult.Artifacts {
		if artifact.ID != "" {
			occurrence, err := projectArtifactOccurrence(ctx, store, artifact, runID, item.ID, attempt, item.Agent)
			if err != nil {
				if strict {
					return nil, true, fmt.Errorf("task %q artifact %q: %w", item.ID, artifact.ID, err)
				}
				failed = true
				continue
			}
			refs = append(refs, occurrence)
			continue
		}
		if strings.TrimSpace(artifact.Path) == "" {
			if strict {
				return nil, true, fmt.Errorf("task %q has artifact without path or id", item.ID)
			}
			failed = true
			continue
		}
		ref, err := store.Put(ctx, PutArtifactRequest{Kind: artifact.Kind, Path: artifact.Path, Description: artifact.Description,
			SourcePath: artifact.Path, RunID: runID, TaskID: item.ID, Attempt: item.Retries + 1, Agent: item.Agent})
		if err != nil {
			if strict {
				return nil, true, err
			}
			failed = true
			continue
		}
		occurrence, err := projectArtifactOccurrence(ctx, store, ref, runID, item.ID, attempt, item.Agent)
		if err != nil {
			if strict {
				return nil, true, fmt.Errorf("task %q artifact %q: %w", item.ID, ref.ID, err)
			}
			failed = true
			continue
		}
		refs = append(refs, occurrence)
	}
	return refs, failed, nil
}

func bindEvidenceResult(runID string, item *TodoItem, receipt *ExecutionReceipt, result *EvidenceResult) error {
	if item == nil || receipt == nil || result == nil {
		return fmt.Errorf("missing execution receipt")
	}
	if receipt.RunID != runID || receipt.TaskID != item.ID || receipt.Attempt <= 0 || receipt.ModelExecutionID == "" || receipt.ProducerID == "" || strings.TrimSpace(receipt.TranscriptRef) == "" {
		return fmt.Errorf("receipt is not fully bound to run, task, attempt, producer, and transcript")
	}
	ids := make([]string, 0, len(result.ArtifactRefs))
	for _, ref := range result.ArtifactRefs {
		if ref.ID == "" {
			return fmt.Errorf("artifact membership contains an unsealed reference")
		}
		ids = append(ids, ref.ID)
	}
	result.Binding = &EvidenceBinding{
		RunID: runID, TaskID: item.ID, Attempt: receipt.Attempt,
		ModelExecutionID: receipt.ModelExecutionID, ProducerID: receipt.ProducerID,
		TranscriptRef: receipt.TranscriptRef, ArtifactIDs: ids,
	}
	return nil
}

// transcriptEvidenceRef resolves current opaque transcript references from the
// artifact store. Filesystem paths remain supported for legacy receipts, but an
// opaque ID is never reinterpreted as a path.
func transcriptEvidenceRef(ctx context.Context, store *FileArtifactStore, item *TodoItem, receipt *ExecutionReceipt, runID string) (ArtifactRef, error) {
	transcriptRef := strings.TrimSpace(receipt.TranscriptRef)
	if strings.HasPrefix(transcriptRef, "sha256-") && validArtifactID(transcriptRef) {
		ref, err := projectArtifactOccurrence(ctx, store, ArtifactRef{ID: transcriptRef}, runID, item.ID, receipt.Attempt, item.Agent)
		if err != nil {
			return ArtifactRef{}, fmt.Errorf("resolve transcript artifact for task %s: %w", item.ID, err)
		}
		return ref, nil
	}
	ref, err := store.Put(ctx, PutArtifactRequest{
		Kind:        "task_transcript",
		Path:        transcriptRef,
		Description: fmt.Sprintf("runner transcript for task %s", item.ID),
		SourcePath:  transcriptRef,
		RunID:       runID,
		TaskID:      item.ID,
		Attempt:     receipt.Attempt,
		Agent:       item.Agent,
	})
	if err != nil {
		return ArtifactRef{}, err
	}
	return projectArtifactOccurrence(ctx, store, ref, runID, item.ID, receipt.Attempt, item.Agent)
}

// projectArtifactOccurrence validates the immutable content-addressed record
// and returns a manifest-local occurrence. The store metadata is first-write
// immutable; run/task/attempt/agent belong to this occurrence and must never
// be written back to the store.
func projectArtifactOccurrence(ctx context.Context, store *FileArtifactStore, source ArtifactRef, runID, taskID string, attempt int, agentName string) (ArtifactRef, error) {
	if store == nil || !validArtifactID(source.ID) {
		return ArtifactRef{}, fmt.Errorf("artifact reference has no valid id")
	}
	immutable, err := store.Get(ctx, source.ID)
	if err != nil {
		return ArtifactRef{}, err
	}
	if source.SHA256 != "" && source.SHA256 != immutable.SHA256 {
		return ArtifactRef{}, fmt.Errorf("artifact %q digest conflicts with immutable metadata", source.ID)
	}
	if source.ByteSize != 0 && source.ByteSize != immutable.ByteSize {
		return ArtifactRef{}, fmt.Errorf("artifact %q size conflicts with immutable metadata", source.ID)
	}
	if source.Bytes != 0 && source.Bytes != immutable.Bytes {
		return ArtifactRef{}, fmt.Errorf("artifact %q byte count conflicts with immutable metadata", source.ID)
	}
	if immutable.SHA256 == "" || immutable.ByteSize < 0 || immutable.Bytes != 0 && immutable.Bytes != immutable.ByteSize {
		return ArtifactRef{}, fmt.Errorf("artifact %q has invalid immutable metadata", source.ID)
	}
	if err := store.Verify(ctx, immutable); err != nil {
		return ArtifactRef{}, err
	}
	occurrence := immutable
	occurrence.RunID = runID
	occurrence.TaskID = taskID
	occurrence.Attempt = attempt
	occurrence.Agent = agentName
	return occurrence, nil
}

// latestSuccessfulExecutionReceipt returns runner-owned transcript evidence for
// a completed task. Retries are searched newest-first so a stale failed attempt
// cannot satisfy the final task evidence requirement.
func latestSuccessfulExecutionReceipt(item *TodoItem, runID string) *ExecutionReceipt {
	if item == nil {
		return nil
	}
	for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
		receipt := &item.ExecutionReceipts[i]
		if receipt.RunID != runID {
			continue
		}
		if receipt.ExitCode != nil && *receipt.ExitCode != 0 {
			continue
		}
		if strings.TrimSpace(receipt.TranscriptRef) != "" {
			return receipt
		}
	}
	if item.ExecutionReceipt != nil {
		if item.ExecutionReceipt.RunID != runID {
			return nil
		}
		if item.ExecutionReceipt.ExitCode == nil || *item.ExecutionReceipt.ExitCode == 0 {
			if strings.TrimSpace(item.ExecutionReceipt.TranscriptRef) != "" {
				return item.ExecutionReceipt
			}
		}
	}
	return nil
}

// latestExecutionReceiptWithTranscript returns forensic evidence regardless of
// exit status. It is used only for failed task manifest entries; successful
// requirements continue to require latestSuccessfulExecutionReceipt.
func latestExecutionReceiptWithTranscript(item *TodoItem, runID string) *ExecutionReceipt {
	if item == nil {
		return nil
	}
	for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
		receipt := &item.ExecutionReceipts[i]
		if receipt.RunID != runID {
			continue
		}
		if strings.TrimSpace(receipt.TranscriptRef) != "" {
			return receipt
		}
	}
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RunID == runID && strings.TrimSpace(item.ExecutionReceipt.TranscriptRef) != "" {
		return item.ExecutionReceipt
	}
	return nil
}

// finalizeEvidenceManifest binds the run-level acceptance observation to the
// task/artifact evidence before the run outcome is published.
func (c *Coordinator) finalizeEvidenceManifest(ctx context.Context, acceptance *AcceptanceResult) error {
	if c == nil {
		return fmt.Errorf("nil coordinator")
	}
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if manifest == nil {
		var err error
		manifest, err = c.buildEvidenceManifest(ctx, false)
		if err != nil {
			return err
		}
	}
	for i := range manifest.EvidenceResults {
		if manifest.EvidenceResults[i].RequirementID == "run:acceptance" {
			manifest.EvidenceResults = append(manifest.EvidenceResults[:i], manifest.EvidenceResults[i+1:]...)
			break
		}
	}
	acceptanceResult := EvidenceResult{RequirementID: "run:acceptance", Validator: "acceptance-gate", CheckedAt: nowUTC()}
	switch {
	case acceptance == nil || acceptance.EffectiveState() == AcceptanceNotConfigured:
		acceptanceResult.Status = "not_configured"
		if manifest.Status == "accepted" {
			manifest.Status = "unverified"
		}
	case acceptance.IsPassed():
		acceptanceResult.Status = "passed"
	default:
		acceptanceResult.Status = "failed"
		manifest.Status = "failed"
		acceptanceResult.Assertions = append(acceptanceResult.Assertions, acceptance.Errors...)
	}
	manifest.EvidenceResults = append(manifest.EvidenceResults, acceptanceResult)
	if err := manifest.Seal(); err != nil {
		return err
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return err
	}
	if err := manifest.Verify(ctx, store); err != nil {
		return err
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(c.session.Workspace, logsDir, "evidence_manifest.json"), b, 0o644); err != nil {
		return fmt.Errorf("persist evidence manifest: %w", err)
	}
	c.lastEvidenceManifestMu.Lock()
	c.lastEvidenceManifest = manifest
	c.lastEvidenceManifestMu.Unlock()
	return nil
}

func nowUTC() time.Time { return time.Now().UTC() }
