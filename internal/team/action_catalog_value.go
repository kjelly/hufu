package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

var errCatalogArgumentsNotRedactionStable = errors.New("team_action_arguments_not_redaction_stable: arguments would change under durable-event redaction")

// canonicalizeCatalogArguments validates raw model JSON against an input schema and
// returns canonical bytes plus their sha256 hash.
func canonicalizeCatalogArguments(schema RunInputSchema, raw []byte) (json.RawMessage, string, error) {
	decoded, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, "", fmt.Errorf("arguments: %w", err)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, "", errors.New("arguments must be a JSON object")
	}
	canonical, err := validateAndCanonicalizeRunInput(schema, raw)
	if err != nil {
		return nil, "", fmt.Errorf("arguments: %w", err)
	}
	canonical, err = canonicalizeCatalogIntegers(canonical)
	if err != nil {
		return nil, "", err
	}
	if !redactionStableJSON(canonical) {
		return nil, "", errCatalogArgumentsNotRedactionStable
	}
	return canonical, runInputHash(canonical), nil
}

// canonicalizeCatalogIntegers re-encodes every number in canonical arguments
// through ParseInt/FormatInt. A catalog input schema has no number type, so
// every number is an integer and has exactly one canonical spelling.
func canonicalizeCatalogIntegers(canonical json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode canonical arguments: %w", err)
	}
	normalized, err := normalizeCatalogIntegerValue(value)
	if err != nil {
		return nil, err
	}
	return canonicalJSONValue(normalized)
}

func normalizeCatalogIntegerValue(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		integer, err := strconv.ParseInt(typed.String(), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("arguments: invalid integer %q: %w", typed.String(), err)
		}
		return json.Number(strconv.FormatInt(integer, 10)), nil
	case map[string]any:
		for key, child := range typed {
			normalized, err := normalizeCatalogIntegerValue(child)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
		return typed, nil
	case []any:
		for index, child := range typed {
			normalized, err := normalizeCatalogIntegerValue(child)
			if err != nil {
				return nil, err
			}
			typed[index] = normalized
		}
		return typed, nil
	default:
		return value, nil
	}
}

// validateCatalogOutputs validates canonical runtime outputs (json.Number values).
func validateCatalogOutputs(schema RunInputSchema, outputs map[string]any) error {
	value := any(outputs)
	if outputs == nil {
		value = map[string]any{}
	}
	if err := validateRunInputValue(schema, value, 1); err != nil {
		return fmt.Errorf("outputs: %w", err)
	}
	return nil
}
