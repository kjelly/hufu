package team

import (
	"context"
	"fmt"
	"strings"
)

// LoadCanonicalRunFinishedSnapshot returns the latest confirmed run_finished
// reducer snapshot for a workspace. It never consults session.json or an
// in-memory coordinator result; those are projections and may be stale or
// uncommitted. An empty result means the event has not been confirmed yet.
func LoadCanonicalRunFinishedSnapshot(workspace, requestedRunID string) (*RunResult, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, nil
	}
	store, err := OpenEventStore(workspace)
	if err != nil {
		return nil, fmt.Errorf("open canonical event store: %w", err)
	}
	defer func() {
		_ = store.Close()
	}()
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return nil, fmt.Errorf("load canonical session tree: %w", err)
	}
	activeBranch := tree.ActiveBranch
	if strings.TrimSpace(activeBranch) == "" {
		activeBranch = "main"
	}
	events, err := store.ReadEvents()
	if err != nil {
		return nil, fmt.Errorf("read canonical event store: %w", err)
	}
	lineage := FilterEventsForBranch(events, tree, activeBranch)
	for index := len(lineage) - 1; index >= 0; index-- {
		event := lineage[index]
		if event.Type != "run_finished" {
			continue
		}
		if strings.TrimSpace(requestedRunID) != "" && event.RunID != requestedRunID {
			continue
		}
		if strings.TrimSpace(event.RunID) == "" {
			return nil, fmt.Errorf("canonical run_finished event has no run_id")
		}
		projected := ReduceToSessionData(lineage[:index+1])
		if projected == nil || projected.RunResult == nil {
			return nil, fmt.Errorf("canonical run_finished event %q did not reduce to a result", event.ID)
		}
		result := projected.RunResult
		if result.RunID != event.RunID {
			return nil, fmt.Errorf("canonical run_finished run_id mismatch: event=%q result=%q", event.RunID, result.RunID)
		}
		if strings.TrimSpace(requestedRunID) != "" && result.RunID != requestedRunID {
			return nil, fmt.Errorf("canonical run_finished run_id %q does not match requested %q", result.RunID, requestedRunID)
		}
		if manifest := result.EvidenceManifest; manifest != nil {
			if manifest.RunID != result.RunID {
				return nil, fmt.Errorf("canonical evidence run_id %q does not match run_finished %q", manifest.RunID, result.RunID)
			}
			if strings.TrimSpace(manifest.ManifestHash) == "" {
				return nil, fmt.Errorf("canonical evidence manifest for run %q has no digest", result.RunID)
			}
			artifactStore, storeErr := NewFileArtifactStore(workspace, workspace)
			if storeErr != nil {
				return nil, fmt.Errorf("open canonical artifact store: %w", storeErr)
			}
			if verifyErr := manifest.Verify(context.Background(), artifactStore); verifyErr != nil {
				return nil, fmt.Errorf("verify canonical evidence digest for run %q: %w", result.RunID, verifyErr)
			}
		}
		return result, nil
	}
	return nil, nil
}
