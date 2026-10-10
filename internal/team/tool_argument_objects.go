package team

import (
	"encoding/json"

	"charm.land/fantasy"
)

// canonicalToolArgumentObjects losslessly unwraps a serialized JSON object
// only where the tool schema explicitly requires an object. Free text, union
// types, duplicate keys and trailing JSON remain unchanged for validation.
// No authorization or result-contract validation is bypassed.
func canonicalToolArgumentObjects(input string, info fantasy.ToolInfo) string {
	value, err := decodeUniqueJSON([]byte(input))
	if err != nil {
		return input
	}
	root := map[string]any{"type": "object", "properties": info.Parameters}
	next, changed := canonicalSchemaObjects(value, root)
	if !changed {
		return input
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return input
	}
	return string(encoded)
}

func canonicalSchemaObjects(value any, schema map[string]any) (any, bool) {
	changed := false
	if text, ok := value.(string); ok && schema["type"] == "object" {
		decoded, err := decodeUniqueJSON([]byte(text))
		if err != nil {
			return value, false
		}
		object, ok := decoded.(map[string]any)
		if !ok {
			return value, false
		}
		value, changed = object, true
	}
	switch node := value.(type) {
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		for name, child := range node {
			childSchema, _ := properties[name].(map[string]any)
			if next, ok := canonicalSchemaObjects(child, childSchema); ok {
				node[name], changed = next, true
			}
		}
	case []any:
		itemSchema, _ := schema["items"].(map[string]any)
		prefix, _ := schema["prefixItems"].([]any)
		for index, child := range node {
			selected := itemSchema
			if index < len(prefix) {
				selected, _ = prefix[index].(map[string]any)
			}
			if next, ok := canonicalSchemaObjects(child, selected); ok {
				node[index], changed = next, true
			}
		}
	}
	return value, changed
}
