package team

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strconv"

	"github.com/kjelly/hufu/internal/utils"
)

type actionPayloadReconstructionKey struct{}

// WithActionPayloadReconstruction explicitly adds bounded public numeric
// metadata reconstruction to a read-only Go action source repair.
func WithActionPayloadReconstruction(ctx context.Context) context.Context {
	return context.WithValue(ctx, actionPayloadReconstructionKey{}, true)
}

func actionPayloadReconstructionRequested(ctx context.Context) bool {
	selected, _ := ctx.Value(actionPayloadReconstructionKey{}).(bool)
	return selected
}

// ActionPayloadReconstruction proves an exact original materialization, while
// preserving the source's redacted bytes and all original native receipts.
type ActionPayloadReconstruction struct {
	StoredSHA256       string   `json:"stored_sha256"`
	MaterializedSHA256 string   `json:"materialized_sha256"`
	SnapshotID         string   `json:"snapshot_id"`
	SnapshotSHA256     string   `json:"snapshot_sha256"`
	Paths              []string `json:"paths"`
	Payload            string   `json:"payload"`
}

func prepareRecoveredActionPayload(ctx context.Context, definition TaskDef, item *TodoItem, session *SessionData) (*TodoItem, *ActionPayloadReconstruction, error) {
	if !actionPayloadReconstructionRequested(ctx) || item.Action == nil || definition.Action == nil || item.Action.Payload == "" {
		return item, nil, nil
	}
	bindings := actionInputBindings(definition.Action)
	if len(bindings) == 0 {
		if item.Action.Payload != definition.Action.Payload {
			return nil, nil, fmt.Errorf("action payload reconstruction requires original frozen action bindings")
		}
		return item, nil, nil
	}
	snapshot := contractRecoveryInputSnapshot(item, session)
	materialized, identity, err := MaterializeAction(*definition.Action, bindings, snapshot)
	if err != nil {
		return nil, nil, err
	}
	if materialized.Payload == item.Action.Payload {
		return item, nil, nil
	}
	if identity.PayloadHash != item.MaterializedActionPayloadHash || !sameCanonicalEvidence(identity.BoundInputs, item.BoundInputs) ||
		!sameCanonicalEvidence(canonicalActionBindings(bindings), canonicalActionBindings(item.ActionInputBindings)) ||
		materialized.Capability != item.Action.Capability || materialized.Type != item.Action.Type {
		return nil, nil, fmt.Errorf("%w: reconstructed action does not match its original hash", errTaskInputDrift)
	}
	proof := &ActionPayloadReconstruction{StoredSHA256: runInputHash([]byte(item.Action.Payload)), MaterializedSHA256: identity.PayloadHash, SnapshotID: item.RunInputSnapshotID, SnapshotSHA256: item.RunInputSnapshotHash, Payload: materialized.Payload}
	proof.Paths, err = reconstructedPayloadPaths(item.Action.Payload, proof.Payload)
	if err != nil {
		return nil, nil, err
	}
	if err = validateReconstructedActionPayload(item, proof); err != nil {
		return nil, nil, err
	}
	candidate := cloneTodoItem(item)
	candidate.Action.Payload = proof.Payload
	return candidate, proof, nil
}

func validateReconstructedActionPayload(item *TodoItem, proof *ActionPayloadReconstruction) error {
	if proof == nil {
		return nil
	}
	if item == nil || item.Action == nil || !metadataReadOnlyTask(item.SideEffect, item.Execution) || item.WorkerWorkspace != nil || len(item.ActionInputBindings) == 0 || len(item.BoundInputs) == 0 || !runInputHashPattern.MatchString(proof.SnapshotSHA256) ||
		proof.StoredSHA256 != runInputHash([]byte(item.Action.Payload)) || proof.MaterializedSHA256 != item.MaterializedActionPayloadHash || !runInputHashPattern.MatchString(proof.MaterializedSHA256) ||
		proof.SnapshotID == "" || proof.SnapshotID != item.RunInputSnapshotID || proof.SnapshotSHA256 != item.RunInputSnapshotHash ||
		len(proof.Payload) > maxMaterializedActionPayloadBytes || runInputHash([]byte(proof.Payload)) != proof.MaterializedSHA256 {
		return fmt.Errorf("invalid action payload reconstruction identity")
	}
	// Ordinary persistence must preserve these bytes. Process redactors remain
	// authoritative; a proof cannot opt a value out of secret protection.
	if utils.RedactSecrets(proof.Payload) != proof.Payload {
		return fmt.Errorf("reconstructed action payload contains sensitive data")
	}
	paths, err := reconstructedPayloadPaths(item.Action.Payload, proof.Payload)
	if err != nil {
		return err
	}
	if !slices.Equal(paths, proof.Paths) {
		return fmt.Errorf("action payload reconstruction paths changed")
	}
	return nil
}

func reconstructedPayloadPaths(stored, rebuilt string) ([]string, error) {
	if len(stored) > maxMaterializedActionPayloadBytes || len(rebuilt) > maxMaterializedActionPayloadBytes {
		return nil, fmt.Errorf("action payload reconstruction exceeds byte limit")
	}
	before, err := decodeUniqueJSON([]byte(stored))
	if err != nil {
		return nil, err
	}
	after, err := decodeUniqueJSON([]byte(rebuilt))
	if err != nil {
		return nil, err
	}
	if _, ok := before.(map[string]any); !ok {
		return nil, fmt.Errorf("action payload reconstruction requires objects")
	}
	if _, ok := after.(map[string]any); !ok {
		return nil, fmt.Errorf("action payload reconstruction requires objects")
	}
	paths := []string{}
	if err = collectReconstructedPayloadPaths(before, after, "", "", 0, &paths); err != nil {
		return nil, err
	}
	if len(paths) == 0 || len(paths) > 64 {
		return nil, fmt.Errorf("action payload reconstruction requires bounded numeric metadata changes")
	}
	sort.Strings(paths)
	return paths, nil
}

func collectReconstructedPayloadPaths(before, after any, path, key string, depth int, paths *[]string) error {
	if depth > 64 || len(*paths) > 64 {
		return fmt.Errorf("action payload reconstruction exceeds structural limits")
	}
	if reflect.DeepEqual(before, after) {
		return nil
	}
	if before == "[REDACTED]" && utils.IsPublicTokenBudget(key, after) {
		*paths = append(*paths, path)
		return nil
	}
	switch old := before.(type) {
	case map[string]any:
		next, ok := after.(map[string]any)
		if !ok || len(old) != len(next) {
			break
		}
		for name, value := range old {
			other, exists := next[name]
			if !exists {
				return fmt.Errorf("action payload reconstruction changed object members")
			}
			if err := collectReconstructedPayloadPaths(value, other, path+"/"+escapeJSONPointerToken(name), name, depth+1, paths); err != nil {
				return err
			}
		}
		return nil
	case []any:
		next, ok := after.([]any)
		if !ok || len(old) != len(next) {
			break
		}
		for index, value := range old {
			if err := collectReconstructedPayloadPaths(value, next[index], path+"/"+strconv.Itoa(index), "", depth+1, paths); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("action payload reconstruction changed data beyond redacted public token budgets")
}

func validateActionPayloadReconstructionProofs(events []RunEvent, tasks []*TodoItem, plan *ContractRecoveryPlan) error {
	var session *SessionData
	count, total := 0, 0
	for _, patch := range plan.Tasks {
		proof := patch.PayloadReconstruction
		if proof == nil {
			continue
		}
		if session == nil {
			session = ReduceToSessionData(events)
		}
		count++
		total += len(proof.Payload)
		if count > 32 || total > 1<<20 || plan.ActionRepair == nil || patch.Action == "repair_result" {
			return fmt.Errorf("action payload reconstruction requires a bounded read-only Go action repair")
		}
		item := todoItemByID(tasks, patch.ID)
		if err := validateReconstructedActionPayload(item, proof); err != nil {
			return err
		}
		snapshot := contractRecoveryInputSnapshot(item, session)
		if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil || snapshot.SnapshotHash != proof.SnapshotSHA256 || session.ExecutionPolicySnapshot == nil || snapshot.SchemaHash != session.ExecutionPolicySnapshot.RunInputSchemaHash {
			return fmt.Errorf("action payload reconstruction frozen inputs changed")
		}
		action := cloneActionValue(*item.Action)
		action.Payload = proof.Payload
		materialized, identity, err := MaterializeAction(action, item.ActionInputBindings, snapshot)
		if err != nil || materialized.Payload != proof.Payload || identity.PayloadHash != proof.MaterializedSHA256 || !sameCanonicalEvidence(identity.BoundInputs, item.BoundInputs) {
			return fmt.Errorf("action payload reconstruction frozen bindings changed")
		}
	}
	return nil
}
