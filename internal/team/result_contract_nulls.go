package team

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// canonicalResultScalars accepts schema-declared boolean and number strings
// and exact serialized nulls without floating-point rounding. It never guesses
// conditional types; integer/null conversions retain strict syntax and numeric
// enum/constant restrictions. Full contract and evidence validation follows.
func canonicalResultScalars(value any, schema *jsonschema.Schema, depth int) any {
	if schema == nil || depth > maxDynamicSchemaDepth {
		return value
	}
	if boolean, converted := canonicalResultBoolean(value, schema); converted {
		return boolean
	}
	if schema.Enum != nil || schema.Const != nil {
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
			if (slices.Contains(types, "integer") && canonicalResultInteger(text)) || slices.Contains(types, "number") {
				// Reuse the exact-number decoder; float64 would round large IDs
				// or indices before schema bounds and canonical hashing run.
				if number, err := decodeUniqueJSON([]byte(text)); err == nil && schema.Validate(number) == nil {
					if _, numeric := number.(json.Number); numeric {
						return number
					}
				}
			}
		}
	}
	switch node := value.(type) {
	case map[string]any:
		for name, child := range node {
			selected := schema.Properties[name]
			if selected == nil && len(schema.PatternProperties) == 0 {
				selected, _ = schema.AdditionalProperties.(*jsonschema.Schema)
			}
			node[name] = canonicalResultScalars(child, selected, depth+1)
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

// Boolean strings from providers may satisfy an explicitly boolean field or
// constant; validate the converted value before accepting it.
func canonicalResultBoolean(value any, schema *jsonschema.Schema) (any, bool) {
	text, ok := value.(string)
	if !ok {
		return value, false
	}
	text = strings.TrimSpace(text)
	booleanType := false
	if schema.Types != nil {
		types := schema.Types.ToStrings()
		booleanType = slices.Contains(types, "boolean") && !slices.Contains(types, "string")
	}
	if (text == "true" || text == "false") && (booleanType || schema.Const != nil) {
		boolean := text == "true"
		if schema.Validate(boolean) == nil {
			return boolean, true
		}
	}
	return value, false
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
