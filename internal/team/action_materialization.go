package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	maxMaterializedActionPayloadBytes = 256 * 1024
	maxActionInputBindings            = 64
)

// ActionInputBinding authorizes one frozen run input to replace one existing
// JSON Pointer target in a repository-authored action payload.
type ActionInputBinding struct {
	Input  string `json:"input" yaml:"input"`
	Target string `json:"target" yaml:"target"`
}

// MaterializedActionIdentity binds the effective payload to the static action
// contract and frozen run-input snapshot without embedding resolved values.
type MaterializedActionIdentity struct {
	ContractHash         string            `json:"contract_hash"`
	RunInputSnapshotID   string            `json:"run_input_snapshot_id"`
	RunInputSnapshotHash string            `json:"run_input_snapshot_hash"`
	BoundInputs          map[string]string `json:"bound_inputs"`
	PayloadHash          string            `json:"materialized_action_payload_hash"`
}

// MaterializeAction performs bounded, shape-preserving RFC 6901 replacement.
// It is pure: callers own both the returned Action and identity maps.
func MaterializeAction(staticAction Action, bindings []ActionInputBinding, snapshot *RunInputSnapshot) (Action, MaterializedActionIdentity, error) {
	if len(bindings) == 0 {
		return cloneActionValue(staticAction), MaterializedActionIdentity{}, nil
	}
	if snapshot == nil {
		return Action{}, MaterializedActionIdentity{}, fmt.Errorf("action input binding requires a frozen run input snapshot")
	}
	if err := ValidateRunInputSnapshot(snapshot); err != nil {
		return Action{}, MaterializedActionIdentity{}, fmt.Errorf("action input binding snapshot: %w", err)
	}
	payload, err := decodeActionPayload(staticAction.Payload)
	if err != nil {
		return Action{}, MaterializedActionIdentity{}, err
	}
	values := make(map[string]json.RawMessage, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		values[input.Name] = append(json.RawMessage(nil), input.CanonicalValue...)
	}
	paths, err := validateBindingPaths(bindings)
	if err != nil {
		return Action{}, MaterializedActionIdentity{}, err
	}
	bound := make(map[string]string, len(bindings))
	for index, binding := range bindings {
		value, ok := values[strings.TrimSpace(binding.Input)]
		if !ok {
			return Action{}, MaterializedActionIdentity{}, fmt.Errorf("input-bindings[%d]: input %q is not present in frozen snapshot", index, binding.Input)
		}
		var replacement any
		if err := decodeSingleJSON(value, &replacement); err != nil {
			return Action{}, MaterializedActionIdentity{}, fmt.Errorf("input-bindings[%d]: decode input %q: %w", index, binding.Input, err)
		}
		if err := replaceJSONPointer(payload, paths[index], replacement); err != nil {
			return Action{}, MaterializedActionIdentity{}, fmt.Errorf("input-bindings[%d] target %q: %w", index, binding.Target, err)
		}
		bound[strings.TrimSpace(binding.Input)] = runInputHash(value)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Action{}, MaterializedActionIdentity{}, fmt.Errorf("canonical action payload: %w", err)
	}
	if len(encoded) > maxMaterializedActionPayloadBytes {
		return Action{}, MaterializedActionIdentity{}, fmt.Errorf("materialized action payload exceeds %d bytes", maxMaterializedActionPayloadBytes)
	}
	contractHash, err := materializedActionContractHash(staticAction, bindings, snapshot.SchemaHash)
	if err != nil {
		return Action{}, MaterializedActionIdentity{}, err
	}
	materialized := cloneActionValue(staticAction)
	materialized.Payload = string(encoded)
	materialized.InputBindings = nil
	return materialized, MaterializedActionIdentity{
		ContractHash: contractHash, RunInputSnapshotID: snapshot.ID,
		RunInputSnapshotHash: snapshot.SnapshotHash, BoundInputs: bound,
		PayloadHash: runInputHash(encoded),
	}, nil
}

func (c *Coordinator) materializeTaskActions(tasks []TaskDef) ([]TaskDef, error) {
	result := make([]TaskDef, len(tasks))
	for index := range tasks {
		result[index] = cloneTaskDef(tasks[index])
	}
	var snapshot *RunInputSnapshot
	var policyHash string
	for index := range result {
		action := result[index].Action
		if action == nil {
			continue
		}
		bindings := actionInputBindings(action)
		if len(bindings) == 0 {
			bindings = append([]ActionInputBinding(nil), result[index].ActionInputBindings...)
		}
		if len(bindings) == 0 {
			continue
		}
		if result[index].MaterializedActionPayloadHash != "" &&
			runInputHash([]byte(action.Payload)) == result[index].MaterializedActionPayloadHash {
			if err := c.validateMaterializedActionIdentity(result[index]); err != nil {
				return nil, fmt.Errorf("task %q action materialization: %w", result[index].ID, err)
			}
			continue
		}
		if snapshot == nil {
			snapshot = c.RunInputSnapshot()
			policy := c.ExecutionPolicySnapshot()
			if policy != nil {
				policyHash = policy.RunInputPolicyHash
			}
		}
		materialized, identity, err := MaterializeAction(*action, bindings, snapshot)
		if err != nil {
			return nil, fmt.Errorf("task %q action materialization: %w", result[index].ID, err)
		}
		identity.ContractHash = boundTaskContractHash(result[index].ContractHash, identity.ContractHash, policyHash)
		result[index].Action = &materialized
		result[index].ActionInputBindings = bindings
		result[index].ContractHash = identity.ContractHash
		result[index].RunInputSnapshotID = identity.RunInputSnapshotID
		result[index].RunInputSnapshotHash = identity.RunInputSnapshotHash
		result[index].MaterializedActionPayloadHash = identity.PayloadHash
		result[index].BoundInputs = cloneStringMap(identity.BoundInputs)
	}
	return result, nil
}

func boundTaskContractHash(baseContractHash, actionContractHash, policyHash string) string {
	encoded, _ := json.Marshal(struct {
		BaseContractHash   string `json:"base_contract_hash,omitempty"`
		ActionContractHash string `json:"action_contract_hash"`
		RunInputPolicyHash string `json:"run_input_policy_hash,omitempty"`
	}{baseContractHash, actionContractHash, policyHash})
	return runInputHash(encoded)
}

func (c *Coordinator) validateMaterializedActionIdentity(task TaskDef) error {
	if task.Action == nil || len(task.ActionInputBindings) == 0 {
		return nil
	}
	if task.RunInputSnapshotID == "" || !runInputHashPattern.MatchString(task.RunInputSnapshotHash) ||
		!runInputHashPattern.MatchString(task.MaterializedActionPayloadHash) || len(task.BoundInputs) == 0 {
		return fmt.Errorf("execution_input_drift: task action is missing frozen materialization identity")
	}
	snapshot := c.RunInputSnapshot()
	if snapshot == nil || snapshot.ID != task.RunInputSnapshotID || snapshot.SnapshotHash != task.RunInputSnapshotHash {
		return fmt.Errorf("execution_input_drift: active run input snapshot differs from task occurrence")
	}
	if runInputHash([]byte(task.Action.Payload)) != task.MaterializedActionPayloadHash {
		return fmt.Errorf("execution_input_drift: materialized action payload differs from task occurrence")
	}
	values := make(map[string]json.RawMessage, len(snapshot.Inputs))
	for _, input := range snapshot.Inputs {
		values[input.Name] = input.CanonicalValue
	}
	boundNames := make(map[string]struct{}, len(task.ActionInputBindings))
	for _, binding := range task.ActionInputBindings {
		boundNames[strings.TrimSpace(binding.Input)] = struct{}{}
	}
	if len(task.BoundInputs) != len(boundNames) {
		return fmt.Errorf("execution_input_drift: bound input set differs from task occurrence")
	}
	for _, binding := range task.ActionInputBindings {
		value, ok := values[strings.TrimSpace(binding.Input)]
		if !ok || task.BoundInputs[strings.TrimSpace(binding.Input)] != runInputHash(value) {
			return fmt.Errorf("execution_input_drift: bound input %q differs from task occurrence", binding.Input)
		}
	}
	return nil
}

func validateActionInputBindings(action *Action, definitions []RunInputDefinition) error {
	if action == nil || len(action.InputBindings) == 0 {
		return nil
	}
	payload, err := decodeActionPayload(action.Payload)
	if err != nil {
		return err
	}
	paths, err := validateBindingPaths(action.InputBindings)
	if err != nil {
		return err
	}
	byName := make(map[string]RunInputDefinition, len(definitions))
	for _, definition := range definitions {
		byName[definition.Name] = definition
	}
	for index, binding := range action.InputBindings {
		definition, ok := byName[strings.TrimSpace(binding.Input)]
		if !ok {
			return fmt.Errorf("input-bindings[%d]: input %q is not declared", index, binding.Input)
		}
		target, err := jsonPointerValue(payload, paths[index])
		if err != nil {
			return fmt.Errorf("input-bindings[%d] target %q: %w", index, binding.Target, err)
		}
		targetType := jsonTypeName(target)
		inputType := definition.Schema.Type
		if inputType == "integer" {
			inputType = "number"
		}
		if targetType != inputType {
			return fmt.Errorf("input-bindings[%d]: input type %s is incompatible with target type %s", index, definition.Schema.Type, targetType)
		}
	}
	return nil
}

func jsonPointerValue(root map[string]any, path []string) (any, error) {
	var current any = root
	for _, token := range path {
		switch container := current.(type) {
		case map[string]any:
			value, ok := container[token]
			if !ok {
				return nil, fmt.Errorf("target does not exist")
			}
			current = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(container) || strconv.Itoa(index) != token {
				return nil, fmt.Errorf("array target does not exist")
			}
			current = container[index]
		default:
			return nil, fmt.Errorf("target traverses scalar value")
		}
	}
	return current, nil
}

func decodeActionPayload(raw string) (map[string]any, error) {
	if len(raw) > maxMaterializedActionPayloadBytes {
		return nil, fmt.Errorf("static action payload exceeds %d bytes", maxMaterializedActionPayloadBytes)
	}
	var payload map[string]any
	if err := decodeSingleJSON([]byte(raw), &payload); err != nil {
		return nil, fmt.Errorf("static action payload must be one JSON object: %w", err)
	}
	if payload == nil {
		return nil, fmt.Errorf("static action payload must be a JSON object")
	}
	return payload, nil
}

func decodeSingleJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateBindingPaths(bindings []ActionInputBinding) ([][]string, error) {
	if len(bindings) > maxActionInputBindings {
		return nil, fmt.Errorf("%d input bindings exceed maximum %d", len(bindings), maxActionInputBindings)
	}
	paths := make([][]string, len(bindings))
	for index, binding := range bindings {
		if strings.TrimSpace(binding.Input) == "" {
			return nil, fmt.Errorf("input-bindings[%d].input is required", index)
		}
		path, err := parseJSONPointer(binding.Target)
		if err != nil {
			return nil, fmt.Errorf("input-bindings[%d].target: %w", index, err)
		}
		paths[index] = path
		for prior := 0; prior < index; prior++ {
			if pointerPrefix(paths[prior], path) || pointerPrefix(path, paths[prior]) {
				return nil, fmt.Errorf("input-bindings[%d].target %q overlaps input-bindings[%d].target %q", index, binding.Target, prior, bindings[prior].Target)
			}
		}
	}
	return paths, nil
}

func parseJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, fmt.Errorf("root replacement is not allowed")
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("must be an RFC 6901 JSON Pointer")
	}
	parts := strings.Split(pointer[1:], "/")
	for index, part := range parts {
		var decoded strings.Builder
		for offset := 0; offset < len(part); offset++ {
			if part[offset] != '~' {
				decoded.WriteByte(part[offset])
				continue
			}
			if offset+1 >= len(part) || part[offset+1] != '0' && part[offset+1] != '1' {
				return nil, fmt.Errorf("contains an invalid escape")
			}
			offset++
			if part[offset] == '0' {
				decoded.WriteByte('~')
			} else {
				decoded.WriteByte('/')
			}
		}
		parts[index] = decoded.String()
	}
	return parts, nil
}

func pointerPrefix(prefix, path []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for index := range prefix {
		if prefix[index] != path[index] {
			return false
		}
	}
	return true
}

func replaceJSONPointer(root map[string]any, path []string, replacement any) error {
	var current any = root
	for index, token := range path {
		last := index == len(path)-1
		switch container := current.(type) {
		case map[string]any:
			existing, ok := container[token]
			if !ok {
				return fmt.Errorf("target does not exist")
			}
			if last {
				if !compatibleJSONTypes(existing, replacement) {
					return fmt.Errorf("input type %s is incompatible with target type %s", jsonTypeName(replacement), jsonTypeName(existing))
				}
				container[token] = replacement
				return nil
			}
			current = existing
		case []any:
			arrayIndex, err := strconv.Atoi(token)
			if err != nil || arrayIndex < 0 || arrayIndex >= len(container) || strconv.Itoa(arrayIndex) != token {
				return fmt.Errorf("array target does not exist")
			}
			if last {
				if !compatibleJSONTypes(container[arrayIndex], replacement) {
					return fmt.Errorf("input type %s is incompatible with target type %s", jsonTypeName(replacement), jsonTypeName(container[arrayIndex]))
				}
				container[arrayIndex] = replacement
				return nil
			}
			current = container[arrayIndex]
		default:
			return fmt.Errorf("target traverses scalar value")
		}
	}
	return fmt.Errorf("root replacement is not allowed")
}

func compatibleJSONTypes(left, right any) bool {
	return jsonTypeName(left) == jsonTypeName(right)
}

func jsonTypeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

func materializedActionContractHash(action Action, bindings []ActionInputBinding, schemaHash string) (string, error) {
	identity := struct {
		Capability string                  `json:"capability"`
		Type       string                  `json:"type"`
		Payload    json.RawMessage         `json:"payload"`
		Bindings   []actionBindingIdentity `json:"input_bindings"`
		SchemaHash string                  `json:"run_input_schema_hash"`
	}{Capability: action.Capability, Type: action.Type, SchemaHash: schemaHash}
	var payload any
	if err := decodeSingleJSON([]byte(action.Payload), &payload); err != nil {
		return "", fmt.Errorf("static action contract payload: %w", err)
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	identity.Payload = canonical
	identity.Bindings = canonicalActionBindings(bindings)
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

type actionBindingIdentity struct {
	Input  string `json:"input"`
	Target string `json:"target"`
}

func canonicalActionBindings(bindings []ActionInputBinding) []actionBindingIdentity {
	result := make([]actionBindingIdentity, len(bindings))
	for index, binding := range bindings {
		result[index] = actionBindingIdentity{Input: strings.TrimSpace(binding.Input), Target: binding.Target}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Target != result[j].Target {
			return result[i].Target < result[j].Target
		}
		return result[i].Input < result[j].Input
	})
	return result
}

func cloneActionValue(action Action) Action {
	action.InputBindings = append([]ActionInputBinding(nil), action.InputBindings...)
	return action
}

func actionInputBindings(action *Action) []ActionInputBinding {
	if action == nil {
		return nil
	}
	return append([]ActionInputBinding(nil), action.InputBindings...)
}
