package team

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

func validateHistoricalResultManifestAgainstSource(manifest ContextInjectionManifest, expected *historicalResultContext) error {
	if err := validateHistoricalResultManifest(manifest); err != nil {
		return err
	}
	byID := make(map[string]ContextManifestItem)
	for _, entry := range manifest.Items {
		if entry.HistoricalResult != nil {
			byID[entry.ID] = entry
		}
	}
	for _, item := range expected.items {
		entry, ok := byID[item.ID]
		if !ok || !reflect.DeepEqual(entry.HistoricalResult, item.HistoricalResult) ||
			entry.ContentHash != historicalResultDigest([]byte(item.Content)) {
			return fmt.Errorf("historical result delivery differs from its frozen source or presentation")
		}
		delete(byID, item.ID)
	}
	if len(byID) != 0 {
		return fmt.Errorf("historical result delivery includes an unauthorized source")
	}
	return nil
}

func validateFrozenHistoricalResultContexts(item *TodoItem, expected *historicalResultContext) error {
	for _, manifest := range item.ContextManifests {
		if !authoritativeDependencyViewRequest(manifest, item) {
			continue
		}
		if err := validateHistoricalResultManifestAgainstSource(manifest, expected); err != nil {
			return err
		}
	}
	return nil
}

// Completion verifies only real model tasks. Native actions and declared tool
// steps do not invent provider calls to satisfy an inherited-context grant.
func (c *Coordinator) validateHistoricalResultDelivery(todoID string, attempt int, runIDs ...string) error {
	owner := c.dependencyViewOwner(todoID)
	item := owner.todoItemByID(todoID)
	if item == nil || item.HistoricalResultBinding == nil || item.Action != nil || item.CatalogAction != nil || len(item.Execution.Steps) > 0 {
		return nil
	}
	expected, err := owner.expectedHistoricalResultContext(context.Background(), item)
	if err != nil {
		return err
	}
	if expected == nil {
		return ErrHistoricalResultTaskBinding
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
		if err := validateHistoricalResultManifestAgainstSource(manifest, expected); err != nil {
			return err
		}
		if err := owner.validateRecordedDependencyViewDispatch(context.Background(), manifest); err != nil {
			return err
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("historical result delivery is unproven for task %s attempt %d", todoID, attempt)
	}
	return nil
}

func (c *Coordinator) validateHistoricalResultRepairContext(ctx context.Context, item *TodoItem, manifest *ContextInjectionManifest) error {
	if item == nil || item.HistoricalResultBinding == nil {
		return nil
	}
	owner := c.dependencyViewOwner(item.ID)
	expected, err := owner.expectedHistoricalResultContext(ctx, item)
	if err != nil {
		return err
	}
	if manifest == nil || expected == nil {
		return fmt.Errorf("historical result repair requires its original dispatched context")
	}
	if err := validateHistoricalResultManifestAgainstSource(*manifest, expected); err != nil {
		return err
	}
	return owner.validateRecordedDependencyViewDispatch(ctx, *manifest)
}

// Keep the frozen work input intact in storage, while withholding runtime-
// injected historical presentations from the auxiliary collector's projection.
// Worker-authored reports and observations remain evidence, not grants.
func (c *Coordinator) historicalResultCollectionInput(ctx context.Context, taskID string, input []byte) (string, error) {
	owner := c.dependencyViewOwner(taskID)
	item := owner.todoItemByID(taskID)
	if item == nil || item.HistoricalResultBinding == nil {
		return string(input), nil
	}
	expected, err := owner.expectedHistoricalResultContext(ctx, item)
	if err != nil {
		return "", err
	}
	if expected == nil {
		return "", ErrHistoricalResultTaskBinding
	}
	projected := string(input)
	for _, source := range expected.items {
		projected = strings.ReplaceAll(projected, source.Content, "[Historical reference withheld from collector: "+source.ID+"]")
	}
	return projected, nil
}
