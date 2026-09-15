package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"slices"
)

const (
	maxDynamicDescriptorBytes = 256 * 1024
	maxDynamicSchemaDepth     = 16
	maxDynamicSchemaNodes     = 2048
)

var dynamicSchemaKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true,
	"enum": true, "const": true, "items": true,
	"description": true, "title": true, "default": true, "examples": true,
}

var dynamicSchemaTypes = map[string]bool{
	"string": true, "number": true, "integer": true, "boolean": true,
	"object": true, "array": true, "null": true,
}

func dynamicSchemaEligible(schema map[string]any) bool {
	if schema == nil {
		return false
	}
	raw, err := json.Marshal(schema)
	if err != nil || len(raw) > maxDynamicDescriptorBytes {
		return false
	}
	nodes := 0
	if err := validateDynamicSchemaDefinition(schema, 1, &nodes); err != nil {
		return false
	}
	typeName, _ := schema["type"].(string)
	return typeName == "object"
}

func validateDynamicSchemaDefinition(schema map[string]any, depth int, nodes *int) error {
	if depth > maxDynamicSchemaDepth {
		return fmt.Errorf("schema depth exceeds %d", maxDynamicSchemaDepth)
	}
	*nodes++
	if *nodes > maxDynamicSchemaNodes {
		return fmt.Errorf("schema nodes exceed %d", maxDynamicSchemaNodes)
	}
	for keyword := range schema {
		if !dynamicSchemaKeywords[keyword] {
			return fmt.Errorf("unsupported schema keyword %q", keyword)
		}
	}
	if value, ok := schema["type"]; ok {
		typeName, ok := value.(string)
		if !ok || !dynamicSchemaTypes[typeName] {
			return fmt.Errorf("unsupported schema type")
		}
	}
	if value, ok := schema["required"]; ok {
		if _, err := dynamicRequiredFields(value); err != nil {
			return err
		}
	}
	if value, ok := schema["additionalProperties"]; ok {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("additionalProperties must be boolean")
		}
	}
	if value, ok := schema["properties"]; ok {
		properties, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("properties must be an object")
		}
		for _, name := range slices.Sorted(mapsKeys(properties)) {
			child, ok := properties[name].(map[string]any)
			if !ok {
				return fmt.Errorf("property %q schema must be an object", name)
			}
			if err := validateDynamicSchemaDefinition(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	if value, ok := schema["items"]; ok {
		child, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("items must be one schema object")
		}
		if err := validateDynamicSchemaDefinition(child, depth+1, nodes); err != nil {
			return err
		}
	}
	if value, ok := schema["enum"]; ok {
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("enum must be an array")
		}
	}
	return nil
}

func mapsKeys[V any](values map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range values {
			if !yield(key) {
				return
			}
		}
	}
}

func dynamicRequiredFields(value any) ([]string, error) {
	array, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("required must be an array")
	}
	fields := make([]string, len(array))
	for i, entry := range array {
		field, ok := entry.(string)
		if !ok || field == "" {
			return nil, fmt.Errorf("required entries must be non-empty strings")
		}
		fields[i] = field
	}
	return fields, nil
}

func validateDynamicArguments(schema map[string]any, raw []byte) (string, error) {
	if !dynamicSchemaEligible(schema) {
		return "", fmt.Errorf("target schema is not supported by gateway v1")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("arguments must be one JSON object: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return "", err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("arguments must be a JSON object")
	}
	if err := validateDynamicValue(schema, object, "arguments"); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return "", fmt.Errorf("encode validated arguments: %w", err)
	}
	return string(encoded), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("input contains more than one JSON value")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func validateDynamicValue(schema map[string]any, value any, path string) error {
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(candidate any) bool { return dynamicJSONEqual(candidate, value) }) {
		return fmt.Errorf("%s is not an allowed enum value", path)
	}
	if expected, ok := schema["const"]; ok && !dynamicJSONEqual(expected, value) {
		return fmt.Errorf("%s does not match const", path)
	}
	typeName, _ := schema["type"].(string)
	switch typeName {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be string", path)
		}
	case "number":
		if _, ok := value.(json.Number); !ok {
			return fmt.Errorf("%s must be number", path)
		}
	case "integer":
		number, ok := value.(json.Number)
		if !ok || !dynamicInteger(number) {
			return fmt.Errorf("%s must be integer", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be array", path)
		}
		if itemSchema, ok := schema["items"].(map[string]any); ok {
			for i, item := range array {
				if err := validateDynamicValue(itemSchema, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be object", path)
		}
		if err := validateDynamicObject(schema, object, path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s has unsupported schema type", path)
	}
	return nil
}

func validateDynamicObject(schema, object map[string]any, path string) error {
	if required, err := dynamicRequiredFieldsOrEmpty(schema["required"]); err != nil {
		return err
	} else {
		for _, field := range required {
			if _, ok := object[field]; !ok {
				return fmt.Errorf("%s.%s is required", path, field)
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	additional, _ := schema["additionalProperties"].(bool)
	_, additionalSpecified := schema["additionalProperties"]
	for name, value := range object {
		child, known := properties[name]
		if !known {
			if additionalSpecified && !additional {
				return fmt.Errorf("%s.%s is not allowed", path, name)
			}
			continue
		}
		childSchema, _ := child.(map[string]any)
		if err := validateDynamicValue(childSchema, value, path+"."+name); err != nil {
			return err
		}
	}
	return nil
}

func dynamicRequiredFieldsOrEmpty(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	return dynamicRequiredFields(value)
}

func dynamicInteger(number json.Number) bool {
	rat, ok := new(big.Rat).SetString(number.String())
	return ok && rat.Denom().Cmp(big.NewInt(1)) == 0
}

func dynamicJSONEqual(left, right any) bool {
	leftNumber, leftOK := left.(json.Number)
	rightNumber, rightOK := right.(json.Number)
	if leftOK || rightOK {
		if !leftOK || !rightOK {
			return false
		}
		leftRat, leftValid := new(big.Rat).SetString(leftNumber.String())
		rightRat, rightValid := new(big.Rat).SetString(rightNumber.String())
		return leftValid && rightValid && leftRat.Cmp(rightRat) == 0
	}
	switch leftValue := left.(type) {
	case map[string]any:
		rightValue, ok := right.(map[string]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for key, value := range leftValue {
			candidate, ok := rightValue[key]
			if !ok || !dynamicJSONEqual(value, candidate) {
				return false
			}
		}
		return true
	case []any:
		rightValue, ok := right.([]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for i := range leftValue {
			if !dynamicJSONEqual(leftValue[i], rightValue[i]) {
				return false
			}
		}
		return true
	default:
		return fmt.Sprint(left) == fmt.Sprint(right) && fmt.Sprintf("%T", left) == fmt.Sprintf("%T", right)
	}
}
