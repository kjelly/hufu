package team

import (
	"encoding/json/v2"
	"fmt"
)

// The contract owns both labels. Missing, null or non-array references never
// count as empty, and the comparison never rewrites either submitted field.
type taskResultItemsProjection struct {
	Pointer  string  `json:"pointer"`
	Empty    *string `json:"empty"`
	NonEmpty *string `json:"non_empty"`
}

func decodeTaskResultItemsProjection(value any) (taskResultItemsProjection, error) {
	var projection taskResultItemsProjection
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > maxTaskResultValueBytes {
		return projection, fmt.Errorf("equals_by_items value must fit within %d bytes", maxTaskResultValueBytes)
	}
	if err := json.Unmarshal(encoded, &projection, json.RejectUnknownMembers(true)); err != nil {
		return projection, fmt.Errorf("equals_by_items requires {pointer, empty, non_empty}: %w", err)
	}
	if projection.Pointer == "" || len(projection.Pointer) > maxTaskResultPointerBytes {
		return projection, fmt.Errorf("equals_by_items requires a nonempty pointer of at most %d bytes", maxTaskResultPointerBytes)
	}
	if err := validateJSONPointer(projection.Pointer); err != nil {
		return projection, fmt.Errorf("equals_by_items has invalid pointer: %w", err)
	}
	if projection.Empty == nil || projection.NonEmpty == nil {
		return projection, fmt.Errorf("equals_by_items requires explicit empty and non_empty string values")
	}
	return projection, nil
}

func evaluateTaskResultItemsProjection(document, value any, resolveErr error, policy any) error {
	projection, err := decodeTaskResultItemsProjection(policy)
	if err != nil {
		return err
	}
	source, err := resolveJSONPointer(document, projection.Pointer)
	if err != nil {
		return fmt.Errorf("equals_by_items reference %q could not be resolved: %w", projection.Pointer, err)
	}
	items, ok := source.([]any)
	if !ok || len(items) > maxTaskResultAssertionItems {
		return fmt.Errorf("equals_by_items reference %q must be a bounded array", projection.Pointer)
	}
	if resolveErr != nil {
		return fmt.Errorf("equals_by_items target could not be resolved: %w", resolveErr)
	}
	expected, branch := *projection.Empty, "empty"
	if len(items) > 0 {
		expected, branch = *projection.NonEmpty, "non_empty"
	}
	if !equalJSONValues(value, expected) {
		return fmt.Errorf("equals_by_items requires the configured %s label for %q (%d items); preserve the referenced array", branch, projection.Pointer, len(items))
	}
	return nil
}

func taskResultAssertionReferencePointer(assertion TaskResultAssertion) string {
	switch assertion.Op {
	case "equals_projection":
		if projection, err := decodeTaskResultProjection(assertion.Value); err == nil {
			return projection.Pointer
		}
	case "equals_by_items":
		if projection, err := decodeTaskResultItemsProjection(assertion.Value); err == nil {
			return projection.Pointer
		}
	}
	return ""
}
