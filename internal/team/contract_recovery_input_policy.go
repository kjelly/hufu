package team

import (
	"context"
	"fmt"
)

type frozenInputPolicyRecoveryKey struct{}

// WithFrozenInputPolicyRecovery explicitly permits a previewed resolver policy
// change. It never permits resolving new input values or changing execution
// authority. Ordinary resume has no corresponding override.
func WithFrozenInputPolicyRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, frozenInputPolicyRecoveryKey{}, true)
}

func frozenInputPolicyRecoveryRequested(ctx context.Context) bool {
	requested, _ := ctx.Value(frozenInputPolicyRecoveryKey{}).(bool)
	return requested
}

// RunInputPolicyRecovery binds an operator-approved policy transition to the
// exact canonical input snapshot. Both policies and this proof are plan-hashed
// and independently checked again during event replay.
type RunInputPolicyRecovery struct {
	SnapshotID     string `json:"snapshot_id"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	OriginalSHA256 string `json:"original_sha256"`
	AcceptedSHA256 string `json:"accepted_sha256"`
}

func (c *Coordinator) assessRunInputPolicyRecovery(session *SessionData, old, current *ExecutionPolicySnapshot) (*RunInputPolicyRecovery, error) {
	if err := c.validateInputPolicyRecoveryContracts(session); err != nil {
		return nil, err
	}
	var snapshot *RunInputSnapshot
	for i := range session.RunInputSnapshots {
		if session.RunInputSnapshots[i].ID == session.ActiveRunInputSnapshotID {
			snapshot = &session.RunInputSnapshots[i]
			break
		}
	}
	if snapshot == nil {
		return nil, fmt.Errorf("input policy recovery requires selected frozen inputs")
	}
	proof := &RunInputPolicyRecovery{SnapshotID: snapshot.ID, SnapshotSHA256: snapshot.SnapshotHash, OriginalSHA256: old.RunInputPolicyHash, AcceptedSHA256: current.RunInputPolicyHash}
	if err := validateRunInputPolicyRecovery(session, old, current, proof); err != nil {
		return nil, err
	}
	return proof, nil
}

func (c *Coordinator) validateInputPolicyRecoveryContracts(session *SessionData) error {
	if !c.session.Config.Delegation.BindTaskGoalContracts {
		return fmt.Errorf("input policy recovery requires static task-goal contracts")
	}
	for _, definition := range c.session.ContractTasks {
		worker := c.session.Agents[normalizedName(definition.Agent)]
		if worker == nil {
			return fmt.Errorf("input policy recovery requires configured worker %q", definition.Agent)
		}
		effect, _, _ := resolveTaskRecovery(worker, definition)
		if !metadataReadOnlyTask(effect, definition.Execution) {
			return fmt.Errorf("input policy recovery refuses writing contract %q", definition.ID)
		}
	}
	for _, item := range session.Tasks {
		if item != nil && !IsPrimaryOccurrence(item) && !metadataReadOnlyTask(item.SideEffect, item.Execution) {
			return fmt.Errorf("input policy recovery refuses writing task %s", item.ID)
		}
	}
	return nil
}

func validateRunInputPolicyRecovery(session *SessionData, old, current *ExecutionPolicySnapshot, proof *RunInputPolicyRecovery) error {
	if proof == nil || old == nil || current == nil || session == nil ||
		!runInputHashPattern.MatchString(old.RunInputPolicyHash) || !runInputHashPattern.MatchString(current.RunInputPolicyHash) ||
		old.RunInputPolicyHash == current.RunInputPolicyHash || proof.OriginalSHA256 != old.RunInputPolicyHash || proof.AcceptedSHA256 != current.RunInputPolicyHash ||
		!runInputHashPattern.MatchString(old.RunInputSchemaHash) || old.RunInputSchemaHash != current.RunInputSchemaHash ||
		proof.SnapshotID == "" || proof.SnapshotID != session.ActiveRunInputSnapshotID {
		return fmt.Errorf("input policy recovery changed input schema or has an invalid policy transition")
	}
	matched := 0
	for i := range session.RunInputSnapshots {
		snapshot := &session.RunInputSnapshots[i]
		if snapshot.ID != proof.SnapshotID {
			continue
		}
		if ValidateRunInputSnapshot(snapshot) != nil || snapshot.SnapshotHash != proof.SnapshotSHA256 || snapshot.SchemaHash != old.RunInputSchemaHash {
			return fmt.Errorf("input policy recovery frozen inputs cannot be verified")
		}
		matched++
	}
	if matched != 1 {
		return fmt.Errorf("input policy recovery requires one canonical frozen input snapshot")
	}
	for _, item := range session.Tasks {
		if item == nil || IsPrimaryOccurrence(item) {
			continue
		}
		if !metadataReadOnlyTask(item.SideEffect, item.Execution) || item.RunInputSnapshotID != proof.SnapshotID || item.RunInputSnapshotHash != proof.SnapshotSHA256 {
			return fmt.Errorf("input policy recovery refuses task %s with side effects or different frozen inputs", item.ID)
		}
	}
	return nil
}
