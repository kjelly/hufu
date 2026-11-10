package team

import (
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Task display timings are absent from historical lifecycle events. They must
// not change the identity of accepted evidence after canonical replay.
func canonicalCollectionSources(sources []*TodoItem) []*TodoItem {
	result := make([]*TodoItem, len(sources))
	for i, source := range sources {
		result[i] = cloneTodoItem(source)
		result[i].StartedAt, result[i].EndedAt = time.Time{}, time.Time{}
		result[i].ModelTime, result[i].ToolTime = 0, 0
		result[i].ContextManifests = normalizeContextManifests(result[i].ContextManifests)
	}
	return result
}

func (c *Coordinator) resultCollectionEvidenceHash(item *TodoItem) (string, error) {
	sources, err := c.resultCandidateEvidenceSources(item)
	if err != nil {
		return "", err
	}
	return collectionDigest(canonicalCollectionSources(sources))
}

func (c *Coordinator) validateCollectionEvidence(item *TodoItem, checkpoint *ResultCollectionCheckpoint) error {
	sources, err := c.resultCandidateEvidenceSources(item)
	if err != nil {
		return err
	}
	if checkpoint.EvidenceRevision == 1 {
		sources = canonicalCollectionSources(sources)
	}
	digest, err := collectionDigest(sources)
	if err != nil {
		return err
	}
	if digest != checkpoint.EvidenceSHA256 {
		return fmt.Errorf("expected=%s actual=%s differing fields=%s: %w", checkpoint.EvidenceSHA256, digest, c.collectionEvidenceDifferences(sources, checkpoint.BranchID), errResultCollectionStale)
	}
	return nil
}

// Diagnose field names only; source reports and credentials are never included.
func (c *Coordinator) collectionEvidenceDifferences(sources []*TodoItem, branchID string) string {
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil || tree == nil || tree.GetBranch(branchID) == nil {
		return "snapshot_unavailable"
	}
	var paths []string
	for _, source := range sources {
		cached := todoItemByID(tree.GetBranch(branchID).State.TaskPlan, source.ID)
		if cached == nil {
			paths = append(paths, source.ID+".snapshot_missing")
			continue
		}
		a, _ := json.Marshal(source, json.Deterministic(true))
		b, _ := json.Marshal(cached, json.Deterministic(true))
		var current, saved map[string]jsontext.Value
		if json.Unmarshal(a, &current) != nil || json.Unmarshal(b, &saved) != nil {
			return "snapshot_invalid"
		}
		for key, value := range current {
			if !bytes.Equal(value, saved[key]) {
				paths = append(paths, source.ID+"."+key)
			}
		}
		for key := range saved {
			if _, present := current[key]; !present {
				paths = append(paths, source.ID+"."+key)
			}
		}
	}
	slices.Sort(paths)
	if len(paths) > 12 {
		paths = paths[:12]
	}
	return strings.Join(paths, ",")
}

// Old checkpoints hashed complete live TodoItems, including display timings.
// Recover display timings and manifest order from a snapshot that reproduces the
// entire original evidence hash and agrees with canonical task projections.
// Payloads, contracts, receipts and capabilities always come from replay.
// Normal collection validation must still reproduce the original full hash.
func restoreLegacyCollectionEvidence(tasks, cached []*TodoItem) {
	for _, item := range tasks {
		checkpoint := currentResultCollection(item)
		if checkpoint == nil || checkpoint.EvidenceRevision != 0 {
			continue
		}
		var current, original []*TodoItem
		for _, id := range resultDependencyIDs(item) {
			source, saved := todoItemByID(tasks, id), todoItemByID(cached, id)
			if source == nil || saved == nil || source.Status != TaskDone || saved.Status != TaskDone || compareSingleTaskProjection(source, saved) != nil {
				original = nil
				break
			}
			current, original = append(current, source), append(original, cloneTodoItem(saved))
		}
		if len(original) == 0 {
			continue
		}
		digest, err := collectionDigest(original)
		if err != nil || digest != checkpoint.EvidenceSHA256 {
			continue
		}
		for i, source := range current {
			source.StartedAt, source.EndedAt = original[i].StartedAt, original[i].EndedAt
			source.ModelTime, source.ToolTime = original[i].ModelTime, original[i].ToolTime
			source.ContextManifests = legacyCollectionManifestOrder(source.ContextManifests, original[i].ContextManifests)
		}
	}
}

// Reorder canonical entries; never copy manifest contents from the snapshot.
func legacyCollectionManifestOrder(current, original []ContextInjectionManifest) []ContextInjectionManifest {
	order := make(map[[2]string]int, len(original))
	for i, manifest := range original {
		order[[2]string{manifest.RequestID, manifest.ModelExecutionID}] = i
	}
	result := cloneContextInjectionManifests(current)
	slices.SortStableFunc(result, func(a, b ContextInjectionManifest) int {
		return cmp.Compare(order[[2]string{a.RequestID, a.ModelExecutionID}], order[[2]string{b.RequestID, b.ModelExecutionID}])
	})
	return result
}
