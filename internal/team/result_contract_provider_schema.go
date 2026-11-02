package team

import (
	"encoding/json/v2"
	"net/url"
	"strings"
)

// projectProviderResultSchema retains the portable shape of a result contract.
// Unsupported constraints are omitted only from the provider-facing guidance;
// the original compiled schema remains the runtime validation boundary. Local
// definitions are inlined so a conditional or length constraint does not erase
// the required fields of the entire payload. Unresolvable, cyclic or oversized
// shapes retain the existing open-schema fallback.
func projectProviderResultSchema(root map[string]any) map[string]any {
	return projectResultSchema(root, false)
}

// Prompt guidance is not a provider tool descriptor. Retain scalar and
// collection bounds here even when a provider's portable schema omits them.
func projectResultGuidanceSchema(root map[string]any) map[string]any {
	return projectResultSchema(root, true)
}

func projectResultSchema(root map[string]any, guidance bool) map[string]any {
	nodes := 0
	var project func(map[string]any, int, map[string]bool) map[string]any
	project = func(schema map[string]any, depth int, visiting map[string]bool) map[string]any {
		nodes++
		if depth > maxDynamicSchemaDepth || nodes > maxDynamicSchemaNodes || schema["$id"] != nil {
			return nil
		}
		if ref, exists := schema["$ref"]; exists {
			path, ok := ref.(string)
			if !ok || !strings.HasPrefix(path, "#/$defs/") {
				return nil
			}
			// Schema siblings can add constraints to the reference. Do not
			// merge their properties or types and accidentally narrow the shape.
			for key := range schema {
				if key != "$ref" && key != "description" && key != "title" {
					return nil
				}
			}
			name, err := url.PathUnescape(strings.TrimPrefix(path, "#/$defs/"))
			if err != nil || strings.Contains(name, "/") {
				return nil
			}
			name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
			defs, _ := root["$defs"].(map[string]any)
			definition, ok := defs[name].(map[string]any)
			if !ok || visiting[name] {
				return nil
			}
			visiting[name] = true
			result := project(definition, depth+1, visiting)
			delete(visiting, name)
			if result != nil {
				for _, key := range []string{"description", "title"} {
					if value, exists := schema[key]; exists {
						result[key] = value
					}
				}
			}
			return result
		}
		result := make(map[string]any)
		for key, value := range schema {
			if !resultSchemaKeyword(key, guidance) {
				continue
			}
			switch key {
			case "properties":
				properties, ok := value.(map[string]any)
				if !ok {
					return nil
				}
				projected := make(map[string]any, len(properties))
				for name, value := range properties {
					child, ok := value.(map[string]any)
					if !ok {
						return nil
					}
					shape := project(child, depth+1, visiting)
					if shape == nil {
						return nil
					}
					projected[name] = shape
				}
				result[key] = projected
			case "items":
				child, ok := value.(map[string]any)
				if !ok {
					return nil
				}
				shape := project(child, depth+1, visiting)
				if shape == nil {
					return nil
				}
				result[key] = shape
			case "additionalProperties":
				// Removing patternProperties while retaining a closed object
				// would reject otherwise valid keys. Leave that object open.
				if _, ok := value.(bool); ok && schema["patternProperties"] == nil {
					result[key] = value
				}
			case "type":
				if name, ok := value.(string); ok && dynamicSchemaTypes[name] {
					result[key] = name
				}
			default:
				result[key] = value
			}
		}
		return result
	}
	projected := project(root, 1, make(map[string]bool))
	if guidance {
		encoded, err := json.Marshal(projected)
		if err != nil || len(encoded) > maxDynamicDescriptorBytes {
			return nil
		}
	}
	if !guidance && !dynamicSchemaEligible(projected) {
		return nil
	}
	return projected
}

func resultSchemaKeyword(keyword string, guidance bool) bool {
	if dynamicSchemaKeywords[keyword] {
		return true
	}
	if !guidance {
		return false
	}
	switch keyword {
	case "minLength", "maxLength", "pattern", "minItems", "maxItems", "uniqueItems",
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf":
		return true
	default:
		// Object property counts can include omitted runtime-owned fields.
		// Their canonical constraints cannot validate the model's projection.
		return false
	}
}
