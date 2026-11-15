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
		if guidance {
			appendVisibleLogicalGuidance(schema, result, depth, &nodes)
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

func appendVisibleLogicalGuidance(schema, result map[string]any, depth int, nodes *int) {
	// Preserve logical rules as a unit: retaining an if without its then (or
	// an incomplete anyOf) gives misleading instructions. Rules mentioning
	// hidden runtime fields remain canonical validation only.
	logical := make(map[string]any)
	for _, key := range []string{"allOf", "anyOf", "oneOf", "if", "then", "else", "not"} {
		if value, exists := schema[key]; exists {
			logical[key] = value
		}
	}
	if resultGuidanceConstraintVisible(logical, result, depth, nodes) {
		for key, value := range logical {
			result[key] = value
		}
	}
}

// Check complete scalar logical branches against the projected visible shape.
// Never partially project a branch: unsupported keywords, unresolved references
// and composite/hidden properties make the whole group unsuitable for prompt
// guidance. Composite collections can be replaced by keyed model decisions or
// contain runtime-owned values. Provider descriptors stay portable separately.
func resultGuidanceConstraintVisible(value any, shape map[string]any, depth int, nodes *int) bool {
	*nodes++
	if depth > maxDynamicSchemaDepth || *nodes > maxDynamicSchemaNodes {
		return false
	}
	if _, ok := value.(bool); ok {
		return true
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return false
	}
	properties, _ := shape["properties"].(map[string]any)
	for key, value := range schema {
		switch key {
		case "allOf", "anyOf", "oneOf":
			branches, ok := value.([]any)
			if !ok {
				return false
			}
			for _, branch := range branches {
				if !resultGuidanceConstraintVisible(branch, shape, depth+1, nodes) {
					return false
				}
			}
		case "if", "then", "else", "not":
			if !resultGuidanceConstraintVisible(value, shape, depth+1, nodes) {
				return false
			}
		case "properties":
			children, ok := value.(map[string]any)
			if !ok {
				return false
			}
			for name, child := range children {
				visible, ok := properties[name].(map[string]any)
				typeName, _ := visible["type"].(string)
				if !ok || typeName == "" || typeName == "object" || typeName == "array" || !resultGuidanceConstraintVisible(child, visible, depth+1, nodes) {
					return false
				}
			}
		case "required":
			names, ok := value.([]any)
			if !ok {
				return false
			}
			for _, name := range names {
				name, ok := name.(string)
				if !ok || properties[name] == nil {
					return false
				}
			}
		case "items", "additionalProperties":
			return false
		case "const":
			switch value.(type) {
			case map[string]any, []any:
				return false
			}
		case "enum":
			values, ok := value.([]any)
			if !ok {
				return false
			}
			for _, value := range values {
				switch value.(type) {
				case map[string]any, []any:
					return false
				}
			}
		default:
			if !resultSchemaKeyword(key, true) {
				return false
			}
		}
	}
	return true
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
