package team

import (
	"encoding/json/v2"
	"fmt"
	"strings"
)

// The schema selects a result array and a frozen JSON-array workset binding.
// Domain names and the meaning of each result entry remain schema-owned.
type resultWorksetInputsSpec struct {
	ItemsPointer  string `json:"items_pointer"`
	ItemIDPointer string `json:"item_id_pointer"`
	Binding       string `json:"binding"`
}

func compileResultWorksetInputs(root map[string]any) (*resultWorksetInputsSpec, error) {
	raw, present := root["x-hufu-workset-inputs"]
	if !present {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	spec := new(resultWorksetInputsSpec)
	if err := json.Unmarshal(encoded, spec, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("x-hufu-workset-inputs: %w", err)
	}
	if strings.TrimSpace(spec.Binding) == "" || len(spec.Binding) > maxWorksetKeyBytes {
		return nil, fmt.Errorf("x-hufu-workset-inputs requires a bounded binding name")
	}
	for _, pointer := range []string{spec.ItemsPointer, spec.ItemIDPointer} {
		if pointer == "" || len(pointer) > maxTaskResultPointerBytes {
			return nil, fmt.Errorf("x-hufu-workset-inputs requires bounded, non-empty pointers")
		}
		if err := validateJSONPointer(pointer); err != nil {
			return nil, fmt.Errorf("x-hufu-workset-inputs: %w", err)
		}
	}
	return spec, nil
}

func validateResultWorksetInputs(value any, spec *resultWorksetInputsSpec, binding *WorksetBinding) error {
	if spec == nil {
		return nil
	}
	if binding == nil {
		return fmt.Errorf("workset input validation requires a frozen workset binding")
	}
	raw, present := binding.Bindings[spec.Binding]
	if !present || len(raw) > maxWorksetValueBytes {
		return fmt.Errorf("workset input binding %q is missing or oversized", spec.Binding)
	}
	expected, err := decodeResultContractJSON([]byte(raw))
	if err != nil {
		return fmt.Errorf("workset input binding %q must be a JSON array of unique non-empty strings", spec.Binding)
	}
	ids, ok := expected.([]any)
	if !ok || len(ids) > maxTaskResultAssertionItems {
		return fmt.Errorf("workset input binding %q must be a bounded JSON array", spec.Binding)
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		text, ok := id.(string)
		if !ok || strings.TrimSpace(text) == "" || seen[text] {
			return fmt.Errorf("workset input binding %q must contain unique non-empty strings", spec.Binding)
		}
		seen[text] = true
	}
	rawItems, err := resolveJSONPointer(value, spec.ItemsPointer)
	items, ok := rawItems.([]any)
	if err != nil || !ok {
		return fmt.Errorf("%s must be an array matching workset binding %q", spec.ItemsPointer, spec.Binding)
	}
	if len(items) != len(ids) {
		return fmt.Errorf("%s requires exactly %d items matching workset binding %q; got %d; resubmit using only the assigned IDs", spec.ItemsPointer, len(ids), spec.Binding, len(items))
	}
	for i, item := range items {
		id, err := resolveJSONPointer(item, spec.ItemIDPointer)
		if err != nil || id != ids[i] {
			// Do not echo arbitrary binding or model values, which may be private.
			return fmt.Errorf("%s/%d%s must match workset binding %q at the same index; resubmit using the assigned order", spec.ItemsPointer, i, spec.ItemIDPointer, spec.Binding)
		}
	}
	return nil
}
