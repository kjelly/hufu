package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

func validateRuntimeEvidenceBinding(root map[string]any, spec *resultEvidenceInputsSpec) error {
	array, err := fixedCollectionSchemaAt(root, spec.ItemsPointer)
	if err != nil || array["type"] != "array" {
		return fmt.Errorf("runtime_bind items_pointer must select a directly declared array")
	}
	item, _ := array["items"].(map[string]any)
	pointers := []string{spec.TaskIDPointer, spec.HashPointer}
	for target := range spec.ValueBindings {
		pointers = append(pointers, target)
	}
	for i, pointer := range pointers {
		if _, err := fixedCollectionSchemaAt(item, pointer); err != nil {
			return fmt.Errorf("runtime_bind fields must select directly declared object properties")
		}
		for _, other := range pointers[:i] {
			if resultPointersOverlap(pointer, other) {
				return fmt.Errorf("runtime_bind fields must not overlap")
			}
		}
	}
	if resultPointersOverlap(spec.ItemsPointer, spec.CompletePointer) {
		return fmt.Errorf("runtime_bind target must not overlap completeness")
	}
	return nil
}

func resultPointersOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// assembleRuntimeEvidenceInputs copies only accepted, hash-checked evidence.
// The worker cannot replace, omit, or edit entries at the runtime-owned path.
// Replay validates the stored array through validateResultEvidenceInputs; it
// never reassembles a historical payload from newer source records.
func assembleRuntimeEvidenceInputs(raw []byte, spec *resultEvidenceInputsSpec, ids []string, results []TaskResult) ([]byte, error) {
	if spec == nil || !spec.RuntimeBind {
		return raw, nil
	}
	if len(raw) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("runtime_bind payload exceeds the byte limit")
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, err
	}
	if _, err := resolveJSONPointer(value, spec.ItemsPointer); err == nil {
		return nil, fmt.Errorf("%s is runtime-owned; omit it and submit decisions only", spec.ItemsPointer)
	}
	entries, err := runtimeBoundEvidenceEntries(spec, ids, results)
	if err != nil {
		return nil, err
	}
	if err := setFixedCollectionValue(value, spec.ItemsPointer, entries); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err == nil && len(encoded) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("runtime-bound payload is %d bytes, above the %d byte limit", len(encoded), resultPayloadMaxBytes)
	}
	return encoded, err
}

func runtimeBoundEvidenceEntries(spec *resultEvidenceInputsSpec, ids []string, results []TaskResult) ([]any, error) {
	if len(ids) == 0 || len(ids) > maxConfiguredEvidenceSources {
		return nil, fmt.Errorf("runtime_bind requires bounded nonempty evidence_from")
	}
	entries := make([]any, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == "" || seen[id] {
			return nil, fmt.Errorf("runtime_bind requires unique evidence_from IDs")
		}
		seen[id] = true
		index := slices.IndexFunc(results, func(r TaskResult) bool { return r.TaskID == id })
		if index < 0 || !taskResultStatusIsSuccessful(results[index].Status) || results[index].StructuredPayload == nil {
			return nil, fmt.Errorf("runtime_bind source %s has no accepted structured payload", id)
		}
		payload := results[index].StructuredPayload
		sum := sha256.Sum256(payload.Value)
		if len(payload.Value) > resultPayloadMaxBytes || payload.SHA256 != hex.EncodeToString(sum[:]) {
			return nil, fmt.Errorf("runtime_bind source %s payload hash mismatch or oversized", id)
		}
		source, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return nil, err
		}
		entry := make(map[string]any)
		setRuntimeEvidenceField(entry, spec.TaskIDPointer, id)
		setRuntimeEvidenceField(entry, spec.HashPointer, payload.SHA256)
		for target, pointer := range spec.ValueBindings {
			copied, err := resolveJSONPointer(source, pointer)
			if err != nil {
				return nil, fmt.Errorf("runtime_bind source %s lacks %s", id, pointer)
			}
			setRuntimeEvidenceField(entry, target, copied)
		}
		entries = append(entries, entry)
	}
	if spec.arraySchema != nil {
		if err := spec.arraySchema.Validate(entries); err != nil {
			return nil, fmt.Errorf("runtime-bound evidence violates its array schema: %s", formatResultPayloadValidationError(err))
		}
	}
	encoded, err := json.Marshal(entries)
	if err == nil && len(encoded) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("runtime-bound payload is %d bytes, above the %d byte limit", len(encoded), resultPayloadMaxBytes)
	}
	return entries, err
}

func setRuntimeEvidenceField(root map[string]any, pointer string, value any) {
	tokens := fixedCollectionPointerTokens(pointer)
	for _, token := range tokens[:len(tokens)-1] {
		next, _ := root[token].(map[string]any)
		if next == nil {
			next = make(map[string]any)
			root[token] = next
		}
		root = next
	}
	root[tokens[len(tokens)-1]] = value
}

// omitRuntimeEvidenceInputs changes only worker guidance. The canonical schema,
// its policy hash, and all validation of the assembled result remain intact.
func omitRuntimeEvidenceInputs(root map[string]any, spec *resultEvidenceInputsSpec) {
	if spec == nil || !spec.RuntimeBind {
		return
	}
	tokens := fixedCollectionPointerTokens(spec.ItemsPointer)
	parent := root
	for _, token := range tokens[:len(tokens)-1] {
		properties, _ := parent["properties"].(map[string]any)
		parent, _ = properties[token].(map[string]any)
	}
	properties, _ := parent["properties"].(map[string]any)
	name := tokens[len(tokens)-1]
	delete(properties, name)
	if required, ok := parent["required"].([]any); ok {
		parent["required"] = slices.DeleteFunc(required, func(value any) bool { return value == name })
	}
}
