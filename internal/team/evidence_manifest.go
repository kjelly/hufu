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
		if item.Status != TaskDone {
			manifest.Status = "failed"
			// A terminal failure must remain auditable even though it cannot
			// satisfy the task requirement. Preserve its immutable transcript in
			// the manifest instead of dropping the most useful failure evidence.
			result := EvidenceResult{RequirementID: "task:" + item.ID, Status: "failed", Validator: "task-verification", CheckedAt: nowUTC()}
			if receipt := latestExecutionReceiptWithTranscript(item); receipt != nil {
				ref, refErr := transcriptEvidenceRef(ctx, store, item, receipt, runID)
				if refErr != nil {
					if strict {
						return nil, fmt.Errorf("failed task %q transcript evidence: %w", item.ID, refErr)
					}
				} else {
					manifest.ArtifactRefs = append(manifest.ArtifactRefs, ref)
					result.ArtifactRefs = append(result.ArtifactRefs, ref)
				}
			}
			manifest.EvidenceResults = append(manifest.EvidenceResults, result)
			continue
		}
		completedCount++
		result := EvidenceResult{RequirementID: "task:" + item.ID, Validator: "task-verification", CheckedAt: nowUTC()}
		if item.VerifyResult != nil && item.VerifyResult.ExitCode == 0 {
			result.Status = "passed"
		} else if item.TypedResult != nil && len(item.TypedResult.Evidence) > 0 {
			result.Status = "passed"
		} else {
			// A successful worker execution always has a runner-owned transcript.
			// Treat that immutable transcript as evidence when the worker did not
			// provide a separate verify command or signed evidence reference. The
			// submit_result schema permits summary/findings-only results, so waiting
			// until finalization to reject those results made the task contract
			// internally inconsistent and marked otherwise successful tool tasks as
			// failed. A non-zero worker exit remains insufficient evidence.
			receipt := latestSuccessfulExecutionReceipt(item)
			if receipt != nil && strings.TrimSpace(receipt.TranscriptRef) != "" {
				ref, putErr := transcriptEvidenceRef(ctx, store, item, receipt, runID)
				if putErr == nil {
					result.Status = "passed"
					manifest.ArtifactRefs = append(manifest.ArtifactRefs, ref)
					result.ArtifactRefs = append(result.ArtifactRefs, ref)
				} else {
					manifest.Status = "failed"
					result.Status = "failed"
					if strict {
						return nil, fmt.Errorf("task %q transcript evidence: %w", item.ID, putErr)
					}
				}
			} else {
				manifest.Status = "failed"
				result.Status = "failed"
				if strict {
					return nil, fmt.Errorf("completed task %q (%s) is missing verification evidence", item.ID, item.Desc)
				}
			}
		}
		if item.TypedResult != nil {
			for _, artifact := range item.TypedResult.Artifacts {
				if artifact.ID != "" {
					if err := store.Verify(ctx, artifact); err != nil {
						manifest.Status = "failed"
						if strict {
							return nil, fmt.Errorf("task %q artifact %q: %w", item.ID, artifact.ID, err)
						}
						result.Status = "failed"
						continue
					}
					manifest.ArtifactRefs = append(manifest.ArtifactRefs, artifact)
					result.ArtifactRefs = append(result.ArtifactRefs, artifact)
					continue
				}
				if strings.TrimSpace(artifact.Path) == "" {
					manifest.Status = "failed"
					if strict {
						return nil, fmt.Errorf("task %q has artifact without path or id", item.ID)
					}
					result.Status = "failed"
					continue
				}
				ref, putErr := store.Put(ctx, PutArtifactRequest{Kind: artifact.Kind, Path: artifact.Path, Description: artifact.Description,
					SourcePath: artifact.Path, RunID: runID, TaskID: item.ID, Attempt: item.Retries + 1, Agent: item.Agent})
				if putErr != nil {
					manifest.Status = "failed"
					if strict {
						return nil, putErr
					}
					result.Status = "failed"
					continue
				}
				manifest.ArtifactRefs = append(manifest.ArtifactRefs, ref)
				result.ArtifactRefs = append(result.ArtifactRefs, ref)
			}
		}
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

// transcriptEvidenceRef resolves current opaque transcript references from the
// artifact store. Filesystem paths remain supported for legacy receipts, but an
// opaque ID is never reinterpreted as a path.
func transcriptEvidenceRef(ctx context.Context, store *FileArtifactStore, item *TodoItem, receipt *ExecutionReceipt, runID string) (ArtifactRef, error) {
	transcriptRef := strings.TrimSpace(receipt.TranscriptRef)
	if strings.HasPrefix(transcriptRef, "sha256-") && validArtifactID(transcriptRef) {
		ref, err := store.Get(ctx, transcriptRef)
		if err != nil {
			return ArtifactRef{}, fmt.Errorf("resolve transcript artifact for task %s: %w", item.ID, err)
		}
		if err := store.Verify(ctx, ref); err != nil {
			return ArtifactRef{}, err
		}
		// Content addressing can legitimately deduplicate identical transcripts.
		// Bind this manifest entry to the current receipt without mutating the
		// immutable store metadata first written for that content.
		ref.RunID = runID
		ref.TaskID = item.ID
		ref.Attempt = receipt.Attempt
		ref.Agent = item.Agent
		return ref, nil
	}
	return store.Put(ctx, PutArtifactRequest{
		Kind:        "task_transcript",
		Path:        transcriptRef,
		Description: fmt.Sprintf("runner transcript for task %s", item.ID),
		SourcePath:  transcriptRef,
		RunID:       runID,
		TaskID:      item.ID,
		Attempt:     receipt.Attempt,
		Agent:       item.Agent,
	})
}

// latestSuccessfulExecutionReceipt returns runner-owned transcript evidence for
// a completed task. Retries are searched newest-first so a stale failed attempt
// cannot satisfy the final task evidence requirement.
func latestSuccessfulExecutionReceipt(item *TodoItem) *ExecutionReceipt {
	if item == nil {
		return nil
	}
	for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
		receipt := &item.ExecutionReceipts[i]
		if receipt.ExitCode != nil && *receipt.ExitCode != 0 {
			continue
		}
		if strings.TrimSpace(receipt.TranscriptRef) != "" {
			return receipt
		}
	}
	if item.ExecutionReceipt != nil {
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
func latestExecutionReceiptWithTranscript(item *TodoItem) *ExecutionReceipt {
	if item == nil {
		return nil
	}
	for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
		receipt := &item.ExecutionReceipts[i]
		if strings.TrimSpace(receipt.TranscriptRef) != "" {
			return receipt
		}
	}
	if item.ExecutionReceipt != nil && strings.TrimSpace(item.ExecutionReceipt.TranscriptRef) != "" {
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
