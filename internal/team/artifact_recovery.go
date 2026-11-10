package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

func (c *Coordinator) recoveredTaskOutputOrigins(items []*TodoItem) (map[string]string, error) {
	request := EvidenceBuildRequest{RunID: coordinatorRuntimeRunID(c), Workspace: c.artifactStoreRootPath(), Items: items}
	if err := c.bindRecoveredEvidence(&request); err != nil {
		return nil, err
	}
	return request.RecoveredTaskRuns, nil
}

func (c *Coordinator) runtimeActionRecoveryOrigins() (map[string]string, error) {
	origins, err := c.recoveredTaskOutputOrigins(c.taskTracker.TodoList().Items())
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(origins)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 64<<10 {
		return nil, fmt.Errorf("runtime action recovery origins exceed the 64 KiB environment limit")
	}
	return origins, nil
}

// validateRecoveredArtifactOrigin uses the same selected-lineage authority as
// terminal evidence. Loading a receipt or an old result alone grants nothing.
func (c *Coordinator) validateRecoveredArtifactOrigin(ref ArtifactRef, producerTaskID string, result *TaskResult) error {
	item := c.todoItemByID(producerTaskID)
	if item == nil || item.Status != TaskDone || !sameCanonicalEvidence(item.TypedResult, result) {
		return fmt.Errorf("artifact producer is not a retained canonical result")
	}
	request := EvidenceBuildRequest{RunID: c.executionRunID, Workspace: c.artifactStoreRootPath(), Items: []*TodoItem{item}}
	if err := c.bindRecoveredEvidence(&request); err != nil {
		return err
	}
	if request.RecoveredTaskRuns[producerTaskID] != ref.RunID {
		return fmt.Errorf("artifact origin is not admitted by the selected recovery inputs")
	}
	snapshot, _ := c.terminalRunInputs(c.executionRunID)
	receipt := recoveredEvidenceReceipt(item, snapshot)
	if receipt == nil || receipt.RunID != ref.RunID || receipt.Attempt != ref.Attempt || receipt.ProducerID != ref.Agent {
		return fmt.Errorf("artifact origin differs from the original successful receipt")
	}
	return c.validateCompletedResumeEvidence(context.Background(), item, snapshot)
}

// An expansion can outlive its invocation, but only its exact canonical
// receipt on the selected input-bound lineage can authorize that reuse.
func (c *Coordinator) validateRecoveredWorksetReceipt(ctx context.Context, receipt *WorksetExpansionReceipt, consumer *TodoItem) error {
	snapshot, _ := c.terminalRunInputs(c.executionRunID)
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil {
		return fmt.Errorf("workset recovery has no selected frozen inputs")
	}
	lineage, err := c.dependencyViewLineage(ctx)
	if err != nil {
		return err
	}
	items, err := ReplayTodoList(lineage)
	if err != nil {
		return err
	}
	if !canonicalRecoveryWorksetConsumer(items, consumer, snapshot, receipt) {
		return fmt.Errorf("workset consumer differs from its canonical frozen binding")
	}
	for _, original := range items {
		if original.RunInputSnapshotID != snapshot.ID || original.RunInputSnapshotHash != snapshot.SnapshotHash ||
			original.WorksetReceipt == nil || !equivalentWorksetReceipts(original.WorksetReceipt, receipt) {
			continue
		}
		current := c.todoItemByID(original.ID)
		if current != nil && sameCanonicalEvidence(current.WorksetBinding, original.WorksetBinding) &&
			sameCanonicalEvidence(current.WorksetReceipt, original.WorksetReceipt) {
			return nil
		}
	}
	return fmt.Errorf("workset receipt has no matching canonical recovery source")
}

// The expansion receipt is stored once for the entire workset, usually on its
// first child. Authorize each consumer's own frozen binding independently of
// where that shared receipt is recorded; never borrow a sibling's binding.
func canonicalRecoveryWorksetConsumer(items []*TodoItem, consumer *TodoItem, snapshot *RunInputSnapshot, receipt *WorksetExpansionReceipt) bool {
	if consumer == nil {
		return true
	}
	binding := consumer.WorksetBinding
	if binding == nil || receipt == nil || binding.WorksetID != receipt.WorksetID || receipt.Children[binding.ItemKey] != consumer.ID ||
		binding.SourceArtifactID != receipt.SourceArtifactID || binding.SourceSHA256 != receipt.SourceSHA256 || normalizeTaskReferenceID(binding.ParentTaskID) != normalizeTaskReferenceID(receipt.ParentTaskID) {
		return false
	}
	for _, original := range items {
		if original.ID == consumer.ID {
			return original.RunInputSnapshotID == snapshot.ID && original.RunInputSnapshotHash == snapshot.SnapshotHash &&
				consumer.RunInputSnapshotID == snapshot.ID && consumer.RunInputSnapshotHash == snapshot.SnapshotHash &&
				sameCanonicalEvidence(original.WorksetBinding, consumer.WorksetBinding)
		}
	}
	return false
}
