package team

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const evidenceKeyPrefix = "ev_"
const evidenceKeyPattern = "^ev_[0-9a-f]{12}$"

// Paths select individual strings in the worker's payload, before fixed-row
// assembly. A wildcard selects an array element or an issued row key. The
// canonical schema and public stored result continue to require full IDs.
type resultEvidenceKeysSpec struct {
	Pointers              []string `json:"pointers"`
	RuntimeOwnedFilesRead bool     `json:"runtime_owned_files_read,omitzero"`
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
		leaf := resultReferenceSchemaAt(schema, tokens, 0)
		if pointer == "" || len(tokens) > 12 || seen[pointer] || leaf == nil || leaf.Types == nil || !slices.Contains(leaf.Types.ToStrings(), "string") {
			return nil, fmt.Errorf("x-hufu-evidence-keys: %q must uniquely select schema-declared strings", pointer)
		}
		seen[pointer] = true
	}
	return spec, nil
}

func resultReferenceSchemaAt(schema *jsonschema.Schema, tokens []string, depth int) *jsonschema.Schema {
	if schema == nil || depth > maxDynamicSchemaDepth {
		return nil
	}
	if schema.Ref != nil {
		return resultReferenceSchemaAt(schema.Ref, tokens, depth+1)
	}
	if len(tokens) == 0 {
		return schema
	}
	if tokens[0] == "*" {
		return resultReferenceSchemaAt(schema.Items2020, tokens[1:], depth+1)
	}
	return resultReferenceSchemaAt(schema.Properties[tokens[0]], tokens[1:], depth+1)
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
	advertiseStringReferencePath(schema, tokens, evidenceKeyPattern, "Full artifact ID, or an ev_ evidence key issued by a successful view in this task attempt. Runtime resolves keys before canonical validation.")
}

func advertiseStringReferencePath(schema map[string]any, tokens []string, keyPattern, description string) {
	if schema == nil {
		return
	}
	if len(tokens) == 0 {
		if schema["type"] != "string" {
			return
		}
		if pattern, ok := schema["pattern"].(string); ok && !strings.Contains(pattern, keyPattern) {
			schema["pattern"] = "(?:" + pattern + ")|(?:" + keyPattern + ")"
		}
		// The canonical schema still enforces these constraints after resolution.
		for _, key := range []string{"enum", "const", "minLength", "maxLength", "format"} {
			delete(schema, key)
		}
		schema["description"] = description
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	if tokens[0] == "*" {
		if items, ok := schema["items"].(map[string]any); ok {
			advertiseStringReferencePath(items, tokens[1:], keyPattern, description)
			return
		}
		// Fixed collection submission schemas replace arrays with issued row maps.
		for _, raw := range properties {
			child, _ := raw.(map[string]any)
			advertiseStringReferencePath(child, tokens[1:], keyPattern, description)
		}
		return
	}
	child, _ := properties[tokens[0]].(map[string]any)
	advertiseStringReferencePath(child, tokens[1:], keyPattern, description)
}

// transformEvidenceKeyPath never traverses undeclared fields or rewrites prose.
func transformEvidenceKeyPath(value any, tokens []string, pointer string, resolve func(string, string) (any, error)) (any, error) {
	if len(tokens) == 0 {
		text, ok := value.(string)
		if !ok || !strings.HasPrefix(text, evidenceKeyPrefix) {
			return value, nil
		}
		return resolve(text, pointer)
	}
	apply := func(child any, key string) (any, error) {
		return transformEvidenceKeyPath(child, tokens[1:], pointer+"/"+escapeJSONPointerToken(key), resolve)
	}
	switch node := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(node)) {
			if tokens[0] != "*" && key != tokens[0] {
				continue
			}
			next, err := apply(node[key], key)
			if err != nil {
				return nil, err
			}
			node[key] = next
		}
	case []any:
		if tokens[0] != "*" {
			return value, nil
		}
		for index, child := range node {
			next, err := apply(child, strconv.Itoa(index))
			if err != nil {
				return nil, err
			}
			node[index] = next
		}
	}
	return value, nil
}
