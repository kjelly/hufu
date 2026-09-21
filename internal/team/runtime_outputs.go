package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	maxRuntimeOutputKeys  = 128
	maxRuntimeOutputBytes = 256 * 1024
	maxRuntimeOutputDepth = 8
)

// CanonicalizeRuntimeOutputs validates, detaches, and hashes runtime-owned
// output values. The returned digest always covers the complete output map.
func CanonicalizeRuntimeOutputs(outputs map[string]any) (map[string]any, string, error) {
	if outputs == nil {
		return nil, "", nil
	}
	if len(outputs) > maxRuntimeOutputKeys {
		return nil, "", fmt.Errorf("runtime outputs contain %d keys (maximum %d)", len(outputs), maxRuntimeOutputKeys)
	}
	normalized := make(map[string]any, len(outputs))
	for key, value := range outputs {
		trimmed := strings.TrimSpace(key)
		if trimmed == "" {
			return nil, "", fmt.Errorf("runtime output key must not be empty after trimming")
		}
		if _, exists := normalized[trimmed]; exists {
			return nil, "", fmt.Errorf("runtime output key %q collides after trimming", trimmed)
		}
		if err := validateRuntimeOutputValue(value, 1); err != nil {
			return nil, "", fmt.Errorf("runtime output %q: %w", trimmed, err)
		}
		normalized[trimmed] = value
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, "", fmt.Errorf("canonical runtime outputs: %w", err)
	}
	if len(encoded) > maxRuntimeOutputBytes {
		return nil, "", fmt.Errorf("canonical runtime outputs contain %d bytes (maximum %d)", len(encoded), maxRuntimeOutputBytes)
	}
	encoded, err = utils.RedactJSON(encoded)
	if err != nil {
		return nil, "", fmt.Errorf("redact canonical runtime outputs: %w", err)
	}
	if len(encoded) > maxRuntimeOutputBytes {
		return nil, "", fmt.Errorf("redacted canonical runtime outputs contain %d bytes (maximum %d)", len(encoded), maxRuntimeOutputBytes)
	}
	var detached map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&detached); err != nil {
		return nil, "", fmt.Errorf("decode canonical runtime outputs: %w", err)
	}
	encoded, err = json.MarshalIndent(detached, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("normalize canonical runtime outputs: %w", err)
	}
	return detached, runInputHash(encoded), nil
}

func validateRuntimeOutputValue(value any, depth int) error {
	if depth > maxRuntimeOutputDepth {
		return fmt.Errorf("value exceeds maximum depth %d", maxRuntimeOutputDepth)
	}
	switch typed := value.(type) {
	case nil, string, bool,
		float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		json.Number:
		_, err := json.Marshal(typed)
		if err != nil {
			return fmt.Errorf("value is not valid JSON: %w", err)
		}
		return nil
	case map[string]any:
		for key, child := range typed {
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("nested object key must not be empty after trimming")
			}
			if err := validateRuntimeOutputValue(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case []any:
		for _, child := range typed {
			if err := validateRuntimeOutputValue(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported JSON value type %T", value)
	}
}

func validateRuntimeTaskResult(result *TaskResult) error {
	if result == nil || result.Source != "runtime" {
		return fmt.Errorf("runtime outputs require a runtime-owned task result")
	}
	outputs, digest, err := hashCanonicalRuntimeOutputs(result.RuntimeOutputs)
	if err != nil {
		return err
	}
	if digest == "" || result.RuntimeOutputsHash != digest {
		return fmt.Errorf("runtime outputs digest mismatch")
	}
	result.RuntimeOutputs = outputs
	return nil
}

// hashCanonicalRuntimeOutputs validates and hashes outputs that have already
// crossed CanonicalizeRuntimeOutputs. It deliberately does not run redaction
// again: process-local learned secrets can grow after an action completes,
// while an execution receipt must keep attesting to the exact canonical value
// accepted at that boundary.
func hashCanonicalRuntimeOutputs(outputs map[string]any) (map[string]any, string, error) {
	if outputs == nil {
		return nil, "", nil
	}
	if len(outputs) > maxRuntimeOutputKeys {
		return nil, "", fmt.Errorf("runtime outputs contain %d keys (maximum %d)", len(outputs), maxRuntimeOutputKeys)
	}
	for key, value := range outputs {
		if strings.TrimSpace(key) != key || key == "" {
			return nil, "", fmt.Errorf("runtime output key %q is not canonical", key)
		}
		if err := validateRuntimeOutputValue(value, 1); err != nil {
			return nil, "", fmt.Errorf("runtime output %q: %w", key, err)
		}
	}
	encoded, err := json.MarshalIndent(outputs, "", "  ")
	if err != nil {
		return nil, "", fmt.Errorf("hash canonical runtime outputs: %w", err)
	}
	if len(encoded) > maxRuntimeOutputBytes {
		return nil, "", fmt.Errorf("canonical runtime outputs contain %d bytes (maximum %d)", len(encoded), maxRuntimeOutputBytes)
	}
	var detached map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&detached); err != nil {
		return nil, "", fmt.Errorf("decode canonical runtime outputs: %w", err)
	}
	return detached, runInputHash(encoded), nil
}

const preservedRuntimeOutputsKey = "__hufu_preserved_runtime_outputs_index__"

// redactJSONPreservingRuntimeOutputs applies the process redaction policy to a
// durable projection without rewriting runtime outputs that were already
// redacted and receipt-hashed by CanonicalizeRuntimeOutputs.
func redactJSONPreservingRuntimeOutputs(data []byte) ([]byte, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	preserved := make([]any, 0)
	maskCanonicalRuntimeOutputs(document, &preserved)
	masked, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	redacted, err := utils.RedactJSON(masked)
	if err != nil {
		return nil, err
	}
	decoder = json.NewDecoder(bytes.NewReader(redacted))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	document, err = restoreCanonicalRuntimeOutputs(document, preserved)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(document, "", "  ")
}

func maskCanonicalRuntimeOutputs(value any, preserved *[]any) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			maskCanonicalRuntimeOutputs(child, preserved)
		}
	case map[string]any:
		if typed["source"] == "runtime" && typed["runtime_outputs"] != nil {
			index := len(*preserved)
			*preserved = append(*preserved, typed["runtime_outputs"])
			typed["runtime_outputs"] = map[string]any{preservedRuntimeOutputsKey: index}
		}
		for key, child := range typed {
			if key != "runtime_outputs" {
				maskCanonicalRuntimeOutputs(child, preserved)
			}
		}
	}
}

func restoreCanonicalRuntimeOutputs(value any, preserved []any) (any, error) {
	switch typed := value.(type) {
	case []any:
		for index, child := range typed {
			restored, err := restoreCanonicalRuntimeOutputs(child, preserved)
			if err != nil {
				return nil, err
			}
			typed[index] = restored
		}
	case map[string]any:
		if len(typed) == 1 {
			if rawIndex, ok := typed[preservedRuntimeOutputsKey].(json.Number); ok {
				index, err := rawIndex.Int64()
				if err != nil || index < 0 || index >= int64(len(preserved)) {
					return nil, fmt.Errorf("restore canonical runtime outputs: invalid index %q", rawIndex)
				}
				return preserved[index], nil
			}
		}
		for key, child := range typed {
			restored, err := restoreCanonicalRuntimeOutputs(child, preserved)
			if err != nil {
				return nil, err
			}
			typed[key] = restored
		}
	}
	return value, nil
}
