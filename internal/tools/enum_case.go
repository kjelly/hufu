package tools

import (
	"bytes"
	"encoding/json"
	"strings"

	"charm.land/fantasy"
)

// CanonicalToolArgumentCase rewrites every string argument that differs from
// exactly one of its schema enum values only in letter case or surrounding
// whitespace (for example "BLOCKED" for "blocked") to that enum value, and
// reports whether anything changed.
//
// Models often echo a status or category in the case their prompt used, and
// rejecting an otherwise unambiguous value wastes a tool call or a retry. The
// rule is deliberately narrow, so it never guesses:
//   - a value that already matches an enum value exactly is left alone;
//   - a value that folds to no enum value, or to more than one, is left alone
//     and still fails validation (for example "BLOCKED: reason");
//   - only string enums are considered; free-text fields are never touched.
//
// Unknown or invalid JSON is returned unchanged for the validator to report.
func CanonicalToolArgumentCase(input string, info fantasy.ToolInfo) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed == "{}" || len(info.Parameters) == 0 {
		return input, false
	}
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return input, false
	}
	root := map[string]any{"type": "object", "properties": info.Parameters}
	canonical, changed := canonicalEnumCase(value, root)
	if !changed {
		return input, false
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(canonical); err != nil {
		return input, false
	}
	return strings.TrimSuffix(encoded.String(), "\n"), true
}

func canonicalEnumCase(value any, schema map[string]any) (any, bool) {
	if schema == nil {
		return value, false
	}
	switch node := value.(type) {
	case string:
		if folded, ok := foldToSchemaEnum(node, schema); ok {
			return folded, true
		}
		return node, false
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		changed := false
		for name, child := range node {
			childSchema, _ := props[name].(map[string]any)
			if next, ok := canonicalEnumCase(child, childSchema); ok {
				node[name] = next
				changed = true
			}
		}
		return node, changed
	case []any:
		itemSchema, _ := schema["items"].(map[string]any)
		prefix, _ := schema["prefixItems"].([]any)
		changed := false
		for index, item := range node {
			selected := itemSchema
			if index < len(prefix) {
				selected, _ = prefix[index].(map[string]any)
			}
			if next, ok := canonicalEnumCase(item, selected); ok {
				node[index] = next
				changed = true
			}
		}
		return node, changed
	default:
		return value, false
	}
}

// foldToSchemaEnum returns the single enum value that text matches after case
// folding and trimming. oneOf alternatives contribute their enum values too.
func foldToSchemaEnum(text string, schema map[string]any) (string, bool) {
	values := schemaEnumStrings(schema["enum"])
	if alternatives, ok := schema["oneOf"].([]any); ok {
		for _, alternative := range alternatives {
			if candidate, ok := alternative.(map[string]any); ok {
				values = append(values, schemaEnumStrings(candidate["enum"])...)
			}
		}
	}
	if len(values) == 0 {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	match, matches := "", 0
	for _, value := range values {
		if value == text {
			return "", false
		}
		if strings.EqualFold(value, trimmed) && value != match {
			match = value
			matches++
		}
	}
	return match, matches == 1
}

func schemaEnumStrings(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}
