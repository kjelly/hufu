package team

import (
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// canonicalResultScalars accepts exact serialized nulls and decimal integers
// only where the original contract explicitly requires those types and excludes
// strings. It never guesses types from conditional branches or rewrites enums
// or constants. Full contract and evidence validation still follows.
func canonicalResultScalars(value any, schema *jsonschema.Schema, depth int) any {
	if schema == nil || depth > maxDynamicSchemaDepth || schema.Enum != nil || schema.Const != nil {
		return value
	}
	if schema.Ref != nil {
		// References with siblings can constrain a value in another way. Leave
		// them to validation rather than combining schemas during normalization.
		if schema.Types != nil || len(schema.Properties) != 0 || len(schema.AllOf) != 0 || len(schema.AnyOf) != 0 || len(schema.OneOf) != 0 || schema.Not != nil || schema.If != nil {
			return value
		}
		return canonicalResultScalars(value, schema.Ref, depth+1)
	}
	if text, ok := value.(string); ok && schema.Types != nil && len(schema.AllOf) == 0 && len(schema.AnyOf) == 0 && len(schema.OneOf) == 0 {
		types := schema.Types.ToStrings()
		if !slices.Contains(types, "string") {
			if text == "null" && slices.Contains(types, "null") && schema.Validate(nil) == nil {
				return nil
			}
			if slices.Contains(types, "integer") && canonicalResultInteger(text) {
				// Reuse the exact-number decoder; float64 would round large IDs
				// or indices before schema bounds and canonical hashing run.
				if number, err := decodeUniqueJSON([]byte(text)); err == nil && schema.Validate(number) == nil {
					return number
				}
			}
		}
	}
	switch node := value.(type) {
	case map[string]any:
		for name, child := range node {
			node[name] = canonicalResultScalars(child, schema.Properties[name], depth+1)
		}
	case []any:
		for index, child := range node {
			selected := schema.Items2020
			if index < len(schema.PrefixItems) {
				selected = schema.PrefixItems[index]
			}
			node[index] = canonicalResultScalars(child, selected, depth+1)
		}
	}
	return value
}

func canonicalResultInteger(text string) bool {
	digits, _ := strings.CutPrefix(text, "-")
	if digits == "" || (len(digits) > 1 && digits[0] == '0') {
		return false
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return false
		}
	}
	return true
}

// This is only the provider-facing submission shape. The original compiled
// schema remains strict and owns conversion, bounds and cross-field checks.
func providerResultIntegerStrings(schema map[string]any, depth int) map[string]any {
	if schema == nil || depth > maxDynamicSchemaDepth {
		return schema
	}
	if schema["type"] == "integer" && schema["enum"] == nil && schema["const"] == nil && schema["$ref"] == nil {
		return map[string]any{"oneOf": []any{schema, map[string]any{
			"type": "string", "pattern": `^-?(0|[1-9][0-9]*)$`,
		}}}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for name, value := range properties {
			if child, ok := value.(map[string]any); ok {
				properties[name] = providerResultIntegerStrings(child, depth+1)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := schema[key].(map[string]any); ok {
			schema[key] = providerResultIntegerStrings(child, depth+1)
		}
	}
	return schema
}
