package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"reflect"
)

func (c *Coordinator) expectedDependencyResultView(ctx context.Context, item *TodoItem) (*dependencyResultView, error) {
	policy, err := item.DependencyResultView.normalized()
	if err != nil || policy == nil {
		return nil, err
	}
	results := c.dependencyResultsForTask(item.ID)
	sources, err := c.dependencyViewSources(ctx, item, results)
	if err != nil {
		return nil, err
	}
	return buildDependencyResultView(policy, results, sources)
}

func validateDependencyViewManifestAgainstSource(manifest ContextInjectionManifest, expected *dependencyResultView) error {
	if err := validateDependencyResultViewManifest(manifest); err != nil {
		return err
	}
	for _, entry := range manifest.Items {
		if entry.ID == "dependency_results" && entry.Kind == "dependency_result" {
			if !reflect.DeepEqual(entry.DependencyResultView, &expected.attribution) || entry.ContentHash != dependencyViewDigest([]byte(expected.content)) {
				return fmt.Errorf("selected dependency delivery differs from its frozen source or presentation")
			}
			return nil
		}
	}
	return fmt.Errorf("selected dependency delivery proof is missing")
}

// A mutable checkpoint cannot promote a prepared request into delivery merely
// by changing ModelCalled and recomputing its public fingerprint. Require the
// same dispatched manifest in the canonical, branch-visible event history.
func (c *Coordinator) validateRecordedDependencyViewDispatch(ctx context.Context, manifest ContextInjectionManifest) error {
	if !manifest.ModelCalled || !primaryDependencyViewRequest(manifest) {
		return fmt.Errorf("selected dependency request has not been dispatched")
	}
	events, err := c.dependencyViewLineage(ctx)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type != "task_context_manifest" || event.TaskID != manifest.TaskID || event.RunID != manifest.RunID || event.SchemaVersion != eventStoreSchemaVersion {
			continue
		}
		var payload struct {
			ID        string                     `json:"id"`
			Manifests []ContextInjectionManifest `json:"context_manifests"`
		}
		if err := json.Unmarshal(event.Payload, &payload, jsonv1.DefaultOptionsV1()); err != nil {
			return fmt.Errorf("decode selected dependency dispatch: %w", err)
		}
		for _, recorded := range payload.Manifests {
			if payload.ID == manifest.TaskID && recorded.ModelCalled && recorded.RequestID == manifest.RequestID && recorded.Fingerprint == manifest.Fingerprint && supportedDeliveryContextManifest(recorded) {
				return nil
			}
		}
	}
	return fmt.Errorf("selected dependency dispatch is absent from canonical event history")
}

func (c *Coordinator) validateSelectedDependencyDelivery(todoID string, attempt int, runIDs ...string) error {
	owner := c.dependencyViewOwner(todoID)
	item := owner.todoItemByID(todoID)
	if item == nil {
		return nil
	}
	expected, err := owner.expectedDependencyResultView(context.Background(), item)
	if err != nil || expected == nil {
		return err
	}
	if err := validateFrozenDependencyResultViews(item, &expected.attribution); err != nil {
		return err
	}
	runID := ""
	if len(runIDs) > 0 {
		runID = runIDs[0]
	} else if item.ExecutionReceipt != nil && item.ExecutionReceipt.Attempt == attempt {
		runID = item.ExecutionReceipt.RunID
	}
	matched := false
	for _, manifest := range item.ContextManifests {
		if !authoritativeDependencyViewRequest(manifest, item) || manifest.Attempt != attempt || (runID != "" && manifest.RunID != runID) {
			continue
		}
		if err := validateDependencyViewManifestAgainstSource(manifest, expected); err != nil {
			return err
		}
		if err := owner.validateRecordedDependencyViewDispatch(context.Background(), manifest); err != nil {
			return err
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("selected dependency delivery is unproven for task %s attempt %d", todoID, attempt)
	}
	return nil
}

func (c *Coordinator) validateSelectedRepairContext(ctx context.Context, item *TodoItem, manifest *ContextInjectionManifest) error {
	owner := c.dependencyViewOwner(item.ID)
	expected, err := owner.expectedDependencyResultView(ctx, item)
	if err != nil || expected == nil {
		return err
	}
	if manifest == nil {
		return fmt.Errorf("selected dependency repair requires the original dispatch manifest")
	}
	if err := validateDependencyViewManifestAgainstSource(*manifest, expected); err != nil {
		return err
	}
	return owner.validateRecordedDependencyViewDispatch(ctx, *manifest)
}
