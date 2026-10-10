package team

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// taskResultProjection compares ordered arrays of objects using an explicitly
// configured set of shared fields. Its policy is configuration-owned, not a
// worker claim. A missing left-hand array is allowed only when explicitly
// requested and the right-hand array exists and is empty (omitempty results).
type taskResultProjection struct {
	Pointer      string   `json:"pointer"`
	Fields       []string `json:"fields"`
	AllowMissing bool     `json:"allow_missing,omitzero"`
}

func decodeTaskResultProjection(value any) (taskResultProjection, error) {
	var projection taskResultProjection
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxTaskResultValueBytes {
		return projection, fmt.Errorf("equals_projection value must fit within %d bytes", maxTaskResultValueBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&projection); err != nil {
		return projection, fmt.Errorf("equals_projection requires {pointer, fields, optional allow_missing}: %w", err)
	}
	if len(projection.Pointer) > maxTaskResultPointerBytes {
		return projection, fmt.Errorf("equals_projection pointer exceeds %d bytes", maxTaskResultPointerBytes)
	}
	if projection.Pointer == "" {
		return projection, fmt.Errorf("equals_projection requires a nonempty pointer")
	}
	if err := validateJSONPointer(projection.Pointer); err != nil {
		return projection, fmt.Errorf("equals_projection has invalid pointer: %w", err)
	}
	if len(projection.Fields) == 0 || len(projection.Fields) > 16 {
		return projection, fmt.Errorf("equals_projection requires 1 to 16 fields")
	}
	seen := make(map[string]bool, len(projection.Fields))
	for _, field := range projection.Fields {
		if field == "" || len(field) > 128 || seen[field] {
			return projection, fmt.Errorf("equals_projection fields must be unique nonempty keys of at most 128 bytes")
		}
		seen[field] = true
	}
	return projection, nil
}

func evaluateTaskResultProjection(document, value any, resolveErr error, policy any) error {
	projection, err := decodeTaskResultProjection(policy)
	if err != nil {
		return err
	}
	expected, err := resolveJSONPointer(document, projection.Pointer)
	if err != nil {
		return fmt.Errorf("equals_projection reference %q could not be resolved: %w", projection.Pointer, err)
	}
	right, ok := expected.([]any)
	if !ok || len(right) > maxTaskResultAssertionItems {
		return fmt.Errorf("equals_projection reference %q must be a bounded array", projection.Pointer)
	}
	if resolveErr != nil {
		if projection.AllowMissing && len(right) == 0 {
			return nil
		}
		return fmt.Errorf("equals_projection array could not be resolved: %w", resolveErr)
	}
	left, ok := value.([]any)
	if !ok || len(left) != len(right) {
		return fmt.Errorf("equals_projection requires an array with exactly %d items matching %q", len(right), projection.Pointer)
	}
	for i := range left {
		leftItem, leftOK := left[i].(map[string]any)
		rightItem, rightOK := right[i].(map[string]any)
		if !leftOK || !rightOK {
			return fmt.Errorf("equals_projection item %d must be an object in both arrays", i)
		}
		for _, field := range projection.Fields {
			leftValue, leftExists := leftItem[field]
			rightValue, rightExists := rightItem[field]
			if !leftExists || !rightExists || !equalJSONValues(leftValue, rightValue) {
				// Do not echo worker-controlled field values (possibly secrets).
				return fmt.Errorf("equals_projection item %d field %q must match %q at the same index", i, field, projection.Pointer)
			}
		}
	}
	return nil
}
