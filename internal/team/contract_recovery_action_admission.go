package team

import "context"

// An operator-approved action repair may change a frozen input-policy hash,
// which changes occurrence contract hashes even when scope and task inputs are
// identical. Reconstruct the original digest through validated child proofs;
// never ignore the admission or normalize any other hash-covered field.
func actionRepairDecisionAdmissionMatches(ctx context.Context, journal EventJournal, current *TodoItem, admission DecisionAdmission) (bool, error) {
	if journal == nil || current == nil {
		return false, nil
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return false, err
	}
	candidate := cloneTodoItem(current)
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != string(EventContractRecovery) {
			continue
		}
		plan, err := decodeContractRecovery(event)
		if err != nil {
			return false, err
		}
		if plan.ActionRepair == nil {
			continue
		}
		var patch *ContractRecoveryTask
		for j := range plan.Tasks {
			if plan.Tasks[j].ID == current.ID {
				patch = &plan.Tasks[j]
				break
			}
		}
		if patch == nil || patch.Action != "retry" || patch.ContractHash != candidate.ContractHash {
			continue
		}
		policy, err := executionPolicySnapshotFromEvents(events[:i])
		if err != nil {
			return false, err
		}
		if _, err = validateContractRecoveryLineage(event, events[:i], policy); err != nil {
			return false, err
		}
		original := todoItemByID(ReduceToTodoList(events[:i]), current.ID)
		if original == nil || candidate.RunInputSnapshotID != original.RunInputSnapshotID || candidate.RunInputSnapshotHash != original.RunInputSnapshotHash ||
			candidate.MaterializedActionPayloadHash != original.MaterializedActionPayloadHash || !sameCanonicalEvidence(candidate.BoundInputs, original.BoundInputs) ||
			!sameCanonicalEvidence(candidate.ActionInputBindings, original.ActionInputBindings) {
			return false, nil
		}
		candidate.ContractHash = original.ContractHash
		digest, err := decisionTaskInputDigest(candidate)
		if err != nil {
			return false, err
		}
		if digest == admission.TaskInputDigest {
			return true, nil
		}
	}
	return false, nil
}
