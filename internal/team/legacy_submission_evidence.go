package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// Version-one candidates hashed whole TodoItems, including process timing that
// canonical events do not retain. A legacy migration can replace that volatile
// binding only with the exact dependency content dispatched for this attempt.
// The old wrapper/hash remains immutable; the new proof seals canonical sources.
func (c *Coordinator) loadLegacyCandidate(ctx context.Context, item *TodoItem, candidate *ResultCandidate, branch string) (map[string]any, error) {
	if item == nil || candidate == nil {
		return nil, errTaskResultStale
	}
	evidence, err := c.resultCandidateEvidenceHash(item)
	if err != nil {
		return nil, err
	}
	if evidence != candidate.EvidenceHash {
		if candidate.Version != 1 || len(resultDependencyIDs(item)) == 0 {
			return nil, errTaskResultStale
		}
		if err := c.validateLegacyDependencyDispatch(ctx, item); err != nil {
			return nil, err
		}
	}
	return c.readResultCandidateAtBranch(ctx, item, candidate, branch)
}

func (c *Coordinator) validateLegacyDependencyDispatch(ctx context.Context, item *TodoItem) error {
	r := item.ExecutionReceipt
	if r == nil || r.ContextManifest == nil {
		return fmt.Errorf("legacy candidate requires its original dependency dispatch proof")
	}
	manifest := *r.ContextManifest
	if !authoritativeDependencyViewRequest(manifest, item) || manifest.RunID != r.RunID || manifest.Attempt != r.Attempt || manifest.ModelExecutionID != r.ModelExecutionID || !supportedDeliveryContextManifest(manifest) {
		return fmt.Errorf("legacy dependency dispatch identity changed")
	}
	expected := hashContentKey(FormatDependencyResults(c.dependencyResultsForTask(item.ID)))
	matched := 0
	for _, entry := range manifest.Items {
		if entry.Kind != "dependency_result" {
			continue
		}
		if entry.ID != "dependency_results" || !entry.Included || entry.Compressed || entry.ContentHash != expected || entry.DependencyResultView != nil || entry.HistoricalResult != nil {
			return fmt.Errorf("legacy dependency content changed")
		}
		matched++
	}
	if matched != 1 {
		return fmt.Errorf("legacy dependency dispatch proof missing or ambiguous")
	}
	if err := c.validateRecordedDependencyViewDispatch(ctx, manifest); err != nil {
		return err
	}
	return c.validateLegacyDispatchSources(ctx, item, manifest)
}

// Rendering alone cannot distinguish a replaced receipt with identical claims.
// Require each complete canonical source to predate the recorded dispatch.
func (c *Coordinator) validateLegacyDispatchSources(ctx context.Context, item *TodoItem, manifest ContextInjectionManifest) error {
	events, err := c.dependencyViewLineage(ctx)
	if err != nil {
		return err
	}
	for i, event := range events {
		if event.Type != "task_context_manifest" || event.RunID != manifest.RunID || event.TaskID != item.ID {
			continue
		}
		var payload struct {
			Manifests []ContextInjectionManifest `json:"context_manifests"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return err
		}
		for _, recorded := range payload.Manifests {
			if !sameCanonicalEvidence(recorded, manifest) {
				continue
			}
			frozen := reduceToTodoList(events[:i]).tasks
			for _, id := range resultDependencyIDs(item) {
				source := todoItemByID(frozen, id)
				if source == nil || source.Status != TaskDone || compareSingleTaskProjection(source, c.todoItemByID(id)) != nil {
					return fmt.Errorf("legacy source %s changed after original dispatch", id)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("legacy dispatch source boundary unavailable")
}
