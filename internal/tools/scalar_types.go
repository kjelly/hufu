package tools

import (
	"encoding/json"
	"encoding/json/jsontext"
	"strings"
)

// CanonicalSchemaScalarTypes converts strings only where a schema explicitly
// requires a boolean or number. It mutates decoded JSON in place. json.Number
// preserves the exact decimal spelling used by the runtime's JSON decoders.
// Unions and conditional-only types are deliberately not interpreted.
func CanonicalSchemaScalarTypes(value any, root map[string]any) (any, bool) {
	remaining := 65536
	var visit func(any, map[string]any, int) (any, bool)
	visit = func(value any, schema map[string]any, depth int) (any, bool) {
		remaining--
		if schema == nil || depth > 64 || remaining < 0 || schema["$id"] != nil {
			return value, false
		}
		if ref, ok := schema["$ref"].(string); ok {
			// Only local JSON pointers are resolved, without I/O or guessing
			// which branch of a union the model intended.
			if !strings.HasPrefix(ref, "#/") {
				return value, false
			}
			target := root
			for token := range strings.SplitSeq(strings.TrimPrefix(ref, "#/"), "/") {
				token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
				target, _ = target[token].(map[string]any)
			}
			// Reference siblings may change the allowed type. Leave such
			// inputs to the validator rather than choosing an interpretation.
			for key := range schema {
				if key != "$ref" && key != "description" && key != "title" {
					return value, false
				}
			}
			return visit(value, target, depth+1)
		}
		if schema["anyOf"] != nil || schema["oneOf"] != nil {
			return value, false
		}
		if text, ok := value.(string); ok {
			return canonicalScalarString(text, schema)
		}
		changed := false
		switch node := value.(type) {
		case map[string]any:
			properties, _ := schema["properties"].(map[string]any)
			for name, child := range node {
				childSchema, _ := properties[name].(map[string]any)
				if childSchema == nil && schema["patternProperties"] == nil {
					childSchema, _ = schema["additionalProperties"].(map[string]any)
				}
				if next, ok := visit(child, childSchema, depth+1); ok {
					node[name], changed = next, true
				}
			}
		case []any:
			items, _ := schema["items"].(map[string]any)
			prefix, _ := schema["prefixItems"].([]any)
			for index, child := range node {
				selected := items
				if index < len(prefix) {
					selected, _ = prefix[index].(map[string]any)
				}
				if next, ok := visit(child, selected, depth+1); ok {
					node[index], changed = next, true
				}
			}
		}
		return value, changed
	}
	return visit(value, root, 0)
}

func canonicalScalarString(text string, schema map[string]any) (any, bool) {
	typ, _ := schema["type"].(string)
	if schema["type"] == nil {
		if _, ok := schema["const"].(bool); ok {
			typ = "boolean"
		}
	}
	trimmed := strings.TrimSpace(text)
	switch typ {
	case "boolean":
		switch trimmed {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	case "integer", "number":
		if typ == "integer" && strings.ContainsAny(trimmed, ".eE") {
			return text, false
		}
		if raw := jsontext.Value(trimmed); raw.IsValid() && raw.Kind() == '0' {
			return json.Number(trimmed), true
		}
	}
	return text, false
}
