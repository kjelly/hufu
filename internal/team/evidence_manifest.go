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
			continue
		}
		completedCount++
		result := EvidenceResult{RequirementID: "task:" + item.ID, Validator: "task-verification", CheckedAt: nowUTC()}
		if item.VerifyResult != nil && item.VerifyResult.ExitCode == 0 {
			result.Status = "passed"
		} else if item.TypedResult != nil && len(item.TypedResult.Evidence) > 0 {
			result.Status = "passed"
		} else {
			manifest.Status = "failed"
			result.Status = "failed"
			if strict {
				return nil, fmt.Errorf("completed task %q (%s) is missing verification evidence", item.ID, item.Desc)
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
