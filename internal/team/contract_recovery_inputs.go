package team

import "fmt"

// Recovery compares occurrence contracts, including the original frozen input
// binding. Re-materializing is pure: it verifies identity without executing an
// action, resolving new inputs, or replacing the stored payload.
func contractRecoveryTaskHash(definition TaskDef, item *TodoItem, session *SessionData) (string, error) {
	base, err := effectiveContractHash(definition.ID, normalizedName(definition.Agent), definition.Execution, definition.OutputMode, definition.SideEffect, definition.Recovery, definition.MaxRetries, definition.Action, definition.FanOut, definition.Optional, definition)
	if err != nil {
		return "", err
	}
	bindings := actionInputBindings(definition.Action)
	if len(bindings) == 0 {
		if len(item.ActionInputBindings) > 0 || item.MaterializedActionPayloadHash != "" || len(item.BoundInputs) > 0 {
			return "", fmt.Errorf("%w: static action bindings changed", errTaskInputDrift)
		}
		return base, nil
	}
	policy := session.ExecutionPolicySnapshot
	snapshot := contractRecoveryInputSnapshot(item, session)
	if policy == nil || snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.SnapshotHash != item.RunInputSnapshotHash || snapshot.SchemaHash != policy.RunInputSchemaHash {
		return "", fmt.Errorf("%w: original action input snapshot cannot be verified", errTaskInputDrift)
	}
	materialized, identity, err := MaterializeAction(*definition.Action, bindings, snapshot)
	if err != nil {
		return "", fmt.Errorf("%w: reconstruct action binding: %v", errTaskInputDrift, err)
	}
	if !sameCanonicalEvidence(canonicalActionBindings(bindings), canonicalActionBindings(item.ActionInputBindings)) ||
		!sameCanonicalEvidence(identity.BoundInputs, item.BoundInputs) || identity.PayloadHash != item.MaterializedActionPayloadHash ||
		item.Action == nil || materialized.Capability != item.Action.Capability || materialized.Type != item.Action.Type || materialized.Payload != item.Action.Payload {
		if item.Action != nil && identity.PayloadHash == item.MaterializedActionPayloadHash && materialized.Payload != item.Action.Payload {
			return "", fmt.Errorf("%w: stored frozen action payload bytes differ from their verified materialization; redacted or altered historical payloads require separate recovery", errTaskInputDrift)
		}
		return "", fmt.Errorf("%w: frozen action payload or input binding changed", errTaskInputDrift)
	}
	return boundTaskContractHash(base, identity.ContractHash, policy.RunInputPolicyHash), nil
}

func contractRecoveryInputSnapshot(item *TodoItem, session *SessionData) *RunInputSnapshot {
	for i := range session.RunInputSnapshots {
		if session.RunInputSnapshots[i].ID == item.RunInputSnapshotID {
			return &session.RunInputSnapshots[i]
		}
	}
	return nil
}
