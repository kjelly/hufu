package team

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
)

// bindRecoveredEvidence authorizes reuse from the selected canonical lineage,
// never from an arbitrary receipt or an unadmitted session snapshot. Original
// execution identities and immutable transcript contents remain unchanged.
func (c *Coordinator) bindRecoveredEvidence(req *EvidenceBuildRequest) error {
	var recovered []*TodoItem
	for _, item := range req.Items {
		if item != nil && item.Status == TaskDone && !IsPrimaryOccurrence(item) && latestSuccessfulExecutionReceipt(item, req.RunID) == nil {
			recovered = append(recovered, item)
		}
	}
	if len(recovered) == 0 {
		return nil
	}
	snapshot, _ := c.terminalRunInputs(req.RunID)
	if snapshot == nil || c.eventStore == nil {
		// Legacy runs without admitted typed inputs retain fail-closed evidence
		// behavior; loading a session alone does not authorize cross-run reuse.
		return nil
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return err
	}
	tree, err := LoadSessionTree(req.Workspace)
	if err != nil {
		return err
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, c.eventStore.BranchID())
	if err != nil {
		return err
	}
	foundInputs := false
	for _, event := range lineage {
		if event.Type != string(EventRunInputsResolved) || event.RunID != snapshot.RunID {
			continue
		}
		var source RunInputSnapshot
		if json.Unmarshal(event.Payload, &source) == nil && ValidateRunInputSnapshot(&source) == nil && sameCanonicalEvidence(&source, snapshot) {
			foundInputs = true
			break
		}
	}
	if !foundInputs {
		return fmt.Errorf("evidence recovery inputs have no matching canonical source in the selected branch")
	}
	canonical, err := ReplayTodoList(lineage)
	if err != nil {
		return err
	}
	byID := make(map[string]*TodoItem, len(canonical))
	for _, item := range canonical {
		byID[item.ID] = item
	}
	for _, item := range recovered {
		source := byID[item.ID]
		if source == nil || source.Status != TaskDone || source.Agent != item.Agent || source.ContractID != item.ContractID || source.ContractHash != item.ContractHash ||
			source.RunInputSnapshotID != snapshot.ID || source.RunInputSnapshotHash != snapshot.SnapshotHash ||
			item.RunInputSnapshotID != snapshot.ID || item.RunInputSnapshotHash != snapshot.SnapshotHash ||
			!sameCanonicalEvidence(source.TypedResult, item.TypedResult) || !sameCanonicalEvidence(source.VerifyResult, item.VerifyResult) {
			return fmt.Errorf("task %q recovery evidence does not match a completed canonical input-bound source", item.ID)
		}
		receipt := recoveredEvidenceReceipt(source, snapshot)
		if receipt == nil || !sameCanonicalEvidence(latestSuccessfulExecutionReceipt(item, receipt.RunID), receipt) {
			return fmt.Errorf("task %q recovery evidence has no matching canonical successful receipt", item.ID)
		}
		if req.RecoveredTaskRuns == nil {
			req.RecoveredTaskRuns = make(map[string]string)
		}
		req.RecoveredTaskRuns[item.ID] = receipt.RunID
	}
	if len(req.RecoveredTaskRuns) > 0 {
		req.RecoverySnapshotID, req.RecoverySnapshotHash = snapshot.ID, snapshot.SnapshotHash
	}
	return nil
}

func sameCanonicalEvidence(left, right any) bool {
	// Existing receipt/result structs persist numeric time.Duration values.
	// Retain those wire semantics while ignoring process-local time metadata.
	a, aErr := json.Marshal(left, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	b, bErr := json.Marshal(right, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	return aErr == nil && bErr == nil && bytes.Equal(a, b)
}

func recoveredEvidenceReceipt(item *TodoItem, snapshot *RunInputSnapshot) *ExecutionReceipt {
	if item == nil || snapshot == nil {
		return nil
	}
	match := func(receipt *ExecutionReceipt) bool {
		return receipt != nil && receipt.Succeeded() && receipt.TaskID == item.ID && receipt.RunID != "" && validArtifactID(receipt.TranscriptRef) &&
			receipt.RunInputSnapshotID == snapshot.ID && receipt.RunInputSnapshotHash == snapshot.SnapshotHash
	}
	if match(item.ExecutionReceipt) {
		return latestSuccessfulExecutionReceipt(item, item.ExecutionReceipt.RunID)
	}
	for i := len(item.ExecutionReceipts) - 1; i >= 0; i-- {
		if match(&item.ExecutionReceipts[i]) {
			return latestSuccessfulExecutionReceipt(item, item.ExecutionReceipts[i].RunID)
		}
	}
	return nil
}

func (m EvidenceManifest) recoveryBindingMatches(binding *EvidenceBinding) bool {
	return binding != nil && binding.RunID != "" && m.RecoverySnapshotID != "" && runInputHashPattern.MatchString(m.RecoverySnapshotHash) &&
		binding.RunInputSnapshotID == m.RecoverySnapshotID && binding.RunInputSnapshotHash == m.RecoverySnapshotHash
}

// ValidateEvidenceRecoveryInputs binds a manifest's cross-run evidence to the
// exact invocation inputs, whose origin is independently validated on replay.
func ValidateEvidenceRecoveryInputs(manifest *EvidenceManifest, snapshot *RunInputSnapshot) error {
	if manifest == nil || manifest.RecoverySnapshotID == "" && manifest.RecoverySnapshotHash == "" {
		return nil
	}
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || manifest.RecoverySnapshotID != snapshot.ID || manifest.RecoverySnapshotHash != snapshot.SnapshotHash {
		return fmt.Errorf("evidence recovery snapshot does not match the admitted run inputs")
	}
	return nil
}
