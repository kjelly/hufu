package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
)

// This extension binds schema-declared review entries to accepted evidence_from
// payloads. It interprets no prose and has no consumer-specific vocabulary.
type resultEvidenceInputsSpec struct {
	ItemsPointer    string            `json:"items_pointer"`
	TaskIDPointer   string            `json:"task_id_pointer"`
	HashPointer     string            `json:"hash_pointer"`
	CompletePointer string            `json:"complete_pointer"`
	ValueBindings   map[string]string `json:"value_bindings,omitempty"`
}

func compileResultEvidenceInputs(root map[string]any) (*resultEvidenceInputsSpec, error) {
	raw, present := root["x-hufu-evidence-inputs"]
	if !present {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	spec := new(resultEvidenceInputsSpec)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(spec); err != nil {
		return nil, fmt.Errorf("x-hufu-evidence-inputs: %w", err)
	}
	for _, pointer := range []string{spec.ItemsPointer, spec.TaskIDPointer, spec.HashPointer, spec.CompletePointer} {
		if pointer == "" {
			return nil, fmt.Errorf("x-hufu-evidence-inputs requires all pointer fields")
		}
		if err := validateJSONPointer(pointer); err != nil {
			return nil, err
		}
	}
	for target, source := range spec.ValueBindings {
		if target == "" || source == "" {
			return nil, fmt.Errorf("input value bindings require non-empty pointers")
		}
		if err := validateJSONPointer(target); err != nil {
			return nil, err
		}
		if err := validateJSONPointer(source); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

func validateResultEvidenceInputs(value any, spec *resultEvidenceInputsSpec, ids []string, results []TaskResult) error {
	if spec == nil {
		return nil
	}
	completeValue, err := resolveJSONPointer(value, spec.CompletePointer)
	if err != nil {
		return err
	}
	complete, ok := completeValue.(bool)
	if !ok {
		return fmt.Errorf("input review completeness must be a boolean")
	}
	raw, err := resolveJSONPointer(value, spec.ItemsPointer)
	if err != nil {
		return err
	}
	entries, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("input review must be an array")
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		id := evidenceString(entry, spec.TaskIDPointer)
		if !slices.Contains(ids, id) || seen[id] {
			return fmt.Errorf("input review task %q is not a unique evidence_from ID", id)
		}
		seen[id] = true
		index := slices.IndexFunc(results, func(result TaskResult) bool { return result.TaskID == id })
		if index < 0 || !taskResultStatusIsSuccessful(results[index].Status) || results[index].StructuredPayload == nil {
			return fmt.Errorf("input review task %q has no accepted structured payload", id)
		}
		payload := results[index].StructuredPayload
		sum := sha256.Sum256(payload.Value)
		if payload.SHA256 != hex.EncodeToString(sum[:]) || evidenceString(entry, spec.HashPointer) != payload.SHA256 {
			return fmt.Errorf("input review task %q payload hash does not match accepted evidence", id)
		}
		source, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return err
		}
		for targetPointer, sourcePointer := range spec.ValueBindings {
			targetValue, targetErr := resolveJSONPointer(entry, targetPointer)
			sourceValue, sourceErr := resolveJSONPointer(source, sourcePointer)
			if targetErr != nil || sourceErr != nil || !reflect.DeepEqual(targetValue, sourceValue) {
				return fmt.Errorf("input review task %q field %s does not match accepted evidence %s", id, targetPointer, sourcePointer)
			}
		}
	}
	if complete && (len(ids) == 0 || len(seen) != len(ids)) {
		return fmt.Errorf("complete input review must bind every evidence_from task")
	}
	return nil
}

func (c *Coordinator) validateSubmittedEvidenceInputs(todoID string, payload *ResultPayload) error {
	item := c.todoItemByID(todoID)
	if item == nil || item.ResultContract == nil {
		return nil
	}
	compiled, err := c.compiledResultContract(item.ResultContract)
	if err != nil {
		return err
	}
	if compiled.evidenceInputs == nil {
		return nil
	}
	if payload == nil {
		return fmt.Errorf("input review requires a structured payload")
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return err
	}
	results := c.dependencyResultsForTask(todoID)
	if err := c.validateDependencyPayloads(results); err != nil {
		return err
	}
	return validateResultEvidenceInputs(value, compiled.evidenceInputs, item.EvidenceFrom, results)
}
