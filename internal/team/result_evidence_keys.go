package team

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const evidenceKeyPrefix = "ev_"
const evidenceKeyPattern = "^ev_[0-9a-f]{12}$"

// Paths select individual strings in the worker's payload, before fixed-row
// assembly. A wildcard selects an array element or an issued row key. The
// canonical schema and public stored result continue to require full IDs.
type resultEvidenceKeysSpec struct {
	Pointers []string `json:"pointers"`
}

func compileResultEvidenceKeys(root map[string]any, schema *jsonschema.Schema) (*resultEvidenceKeysSpec, error) {
	raw, exists := root["x-hufu-evidence-keys"]
	if !exists {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	spec := new(resultEvidenceKeysSpec)
	if err := json.Unmarshal(data, spec, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("x-hufu-evidence-keys: %w", err)
	}
	if len(spec.Pointers) == 0 || len(spec.Pointers) > 16 {
		return nil, fmt.Errorf("x-hufu-evidence-keys requires 1..16 pointers")
	}
	seen := make(map[string]bool)
	for _, pointer := range spec.Pointers {
		if err := validateJSONPointer(pointer); err != nil {
			return nil, fmt.Errorf("x-hufu-evidence-keys: %w", err)
		}
		tokens := fixedCollectionPointerTokens(pointer)
		if pointer == "" || len(tokens) > 12 || seen[pointer] || !evidenceKeySchemaPath(schema, tokens, 0) {
			return nil, fmt.Errorf("x-hufu-evidence-keys: %q must uniquely select schema-declared strings", pointer)
		}
		seen[pointer] = true
	}
	return spec, nil
}

func evidenceKeySchemaPath(schema *jsonschema.Schema, tokens []string, depth int) bool {
	if schema == nil || depth > maxDynamicSchemaDepth {
		return false
	}
	if schema.Ref != nil {
		return evidenceKeySchemaPath(schema.Ref, tokens, depth+1)
	}
	if len(tokens) == 0 {
		return schema.Types != nil && slices.Contains(schema.Types.ToStrings(), "string")
	}
	if tokens[0] == "*" {
		return evidenceKeySchemaPath(schema.Items2020, tokens[1:], depth+1)
	}
	return evidenceKeySchemaPath(schema.Properties[tokens[0]], tokens[1:], depth+1)
}

// Only worker-facing guidance is widened. Expansion happens before DTO/schema
// validation, and the original compiled schema validates the canonical result.
func advertiseResultEvidenceKeys(schema map[string]any, spec *resultEvidenceKeysSpec) {
	if spec == nil {
		return
	}
	for _, pointer := range spec.Pointers {
		advertiseEvidenceKeyPath(schema, fixedCollectionPointerTokens(pointer))
	}
}

func advertiseEvidenceKeyPath(schema map[string]any, tokens []string) {
	if schema == nil {
		return
	}
	if len(tokens) == 0 {
		if schema["type"] != "string" {
			return
		}
		if pattern, ok := schema["pattern"].(string); ok && !strings.Contains(pattern, evidenceKeyPattern) {
			schema["pattern"] = "(?:" + pattern + ")|(?:" + evidenceKeyPattern + ")"
		}
		// The canonical schema still enforces these constraints after resolution.
		for _, key := range []string{"enum", "const", "minLength", "maxLength", "format"} {
			delete(schema, key)
		}
		schema["description"] = "Full artifact ID, or an ev_ evidence key issued by a successful view in this task attempt. Runtime resolves keys before canonical validation."
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	if tokens[0] == "*" {
		if items, ok := schema["items"].(map[string]any); ok {
			advertiseEvidenceKeyPath(items, tokens[1:])
			return
		}
		// Fixed collection submission schemas replace arrays with issued row maps.
		for _, raw := range properties {
			child, _ := raw.(map[string]any)
			advertiseEvidenceKeyPath(child, tokens[1:])
		}
		return
	}
	child, _ := properties[tokens[0]].(map[string]any)
	advertiseEvidenceKeyPath(child, tokens[1:])
}

// transformEvidenceKeyPath never traverses undeclared fields or rewrites prose.
func transformEvidenceKeyPath(value any, tokens []string, resolve func(string) (string, error)) (any, bool, error) {
	if len(tokens) == 0 {
		text, ok := value.(string)
		if !ok || !strings.HasPrefix(text, evidenceKeyPrefix) {
			return value, false, nil
		}
		next, err := resolve(text)
		return next, err == nil, err
	}
	changed := false
	apply := func(child any) (any, error) {
		next, didChange, err := transformEvidenceKeyPath(child, tokens[1:], resolve)
		changed = changed || didChange
		return next, err
	}
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if tokens[0] != "*" && key != tokens[0] {
				continue
			}
			next, err := apply(child)
			if err != nil {
				return nil, false, err
			}
			node[key] = next
		}
	case []any:
		if tokens[0] != "*" {
			return value, false, nil
		}
		for index, child := range node {
			next, err := apply(child)
			if err != nil {
				return nil, false, err
			}
			node[index] = next
		}
	}
	return value, changed, nil
}
