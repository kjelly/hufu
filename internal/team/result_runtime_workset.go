package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// Clone a referenced parent at its use site before removing runtime-owned
// children. Shared definitions and unrelated uses retain their model fields.
func materializeRuntimeInputParents(root map[string]any, pointer string) error {
	current := root
	tokens := fixedCollectionPointerTokens(pointer)
	for _, name := range tokens[:len(tokens)-1] {
		properties, _ := current["properties"].(map[string]any)
		child, ok := properties[name].(map[string]any)
		if !ok {
			return fmt.Errorf("runtime input parent is unavailable")
		}
		for hops := 0; child["$ref"] != nil; hops++ {
			ref, ok := child["$ref"].(string)
			if !ok || !strings.HasPrefix(ref, "#/") || hops >= 64 {
				return fmt.Errorf("runtime input parent reference is invalid")
			}
			value, err := resolveJSONPointer(root, strings.TrimPrefix(ref, "#"))
			if err != nil {
				return err
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			decoded, err := decodeResultContractJSON(raw)
			if err != nil {
				return err
			}
			copy, ok := decoded.(map[string]any)
			if !ok {
				return fmt.Errorf("runtime input parent schema is not an object")
			}
			for _, annotation := range []string{"title", "description", "$comment"} {
				if value, exists := child[annotation]; exists {
					copy[annotation] = value
				}
			}
			child = copy
			properties[name] = child
		}
		current = child
	}
	return nil
}

// Runtime-owned scalars come from the immutable manifest occurrence assigned
// to this child, never from model prose or a sibling's mutable task metadata.
func (c *Coordinator) resultWorksetBindingValue(taskID, name string) (any, error) {
	if owner := c.dependencyViewOwner(taskID); owner != c {
		return owner.resultWorksetBindingValue(taskID, name)
	}
	item := c.todoItemByID(taskID)
	if item == nil || item.WorksetBinding == nil {
		return nil, fmt.Errorf("workset runtime input requires a bound child")
	}
	binding := item.WorksetBinding
	receipt, err := c.findWorksetReceipt(binding.ParentTaskID)
	if err != nil {
		return nil, err
	}
	if receipt.WorksetID != binding.WorksetID || receipt.Children[binding.ItemKey] != item.ID ||
		receipt.SourceArtifactID != binding.SourceArtifactID || receipt.SourceSHA256 != binding.SourceSHA256 ||
		!sameArtifactOccurrence(receipt.SourceArtifact, binding.SourceArtifact) {
		return nil, fmt.Errorf("workset runtime input has a mismatched expansion receipt")
	}
	if err := c.verifyWorksetSource(context.Background(), receipt); err != nil {
		return nil, err
	}
	if receipt.RunID != c.executionRunID {
		if err := c.validateRecoveredWorksetReceipt(context.Background(), receipt, item); err != nil {
			return nil, err
		}
	}
	data, source, err := c.readStructuredWorksetSource(TaskDef{FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: receipt.SourceArtifact.TaskID, Artifact: receipt.SourceArtifactID}}})
	if err != nil {
		return nil, err
	}
	manifest, err := normalizeStructuredWorkset(data)
	if err != nil {
		return nil, err
	}
	manifest, err = c.validateWorksetInputs(manifest, receipt.SourceArtifact.TaskID)
	if err != nil {
		return nil, err
	}
	if worksetID(source.ID, source.SHA256, manifest.Items) != binding.WorksetID || len(manifest.Items) != receipt.ItemCount {
		return nil, fmt.Errorf("workset runtime input source manifest changed")
	}
	for _, row := range manifest.Items {
		if row.Key != binding.ItemKey {
			continue
		}
		if !sameCanonicalEvidence(row.Bindings, binding.Bindings) || !sameCanonicalEvidence(row.Inputs, binding.Inputs) ||
			!sameCanonicalEvidence(row.RequiredReads, binding.RequiredReads) || !sameCanonicalEvidence(row.TouchedPaths, binding.TouchedPaths) {
			return nil, fmt.Errorf("workset runtime input differs from its frozen manifest row")
		}
		value, ok := row.Bindings[name]
		if !ok || len(value) > maxWorksetValueBytes {
			return nil, fmt.Errorf("workset runtime input %q is missing or oversized", name)
		}
		return value, nil
	}
	return nil, fmt.Errorf("workset runtime input has no assigned manifest row")
}

// Permit bounded local references along an object path without mutating the
// canonical schema. Ambiguous ref siblings and external references fail closed.
func runtimeInputSchemaAt(root map[string]any, pointer string) (map[string]any, error) {
	current := root
	for _, name := range fixedCollectionPointerTokens(pointer) {
		seen := make(map[string]bool)
		for hops := 0; current["$ref"] != nil; hops++ {
			ref, ok := current["$ref"].(string)
			if !ok || !strings.HasPrefix(ref, "#/") || seen[ref] || hops >= 64 {
				return nil, fmt.Errorf("runtime input requires bounded local schema references")
			}
			seen[ref] = true
			for key := range current {
				if key != "$ref" && key != "description" && key != "title" && key != "$comment" {
					return nil, fmt.Errorf("runtime input path has ambiguous reference siblings")
				}
			}
			node, err := resolveJSONPointer(root, strings.TrimPrefix(ref, "#"))
			if err != nil {
				return nil, err
			}
			current, ok = node.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("runtime input schema reference must select an object")
			}
		}
		properties, _ := current["properties"].(map[string]any)
		next, ok := properties[name].(map[string]any)
		if !ok || current["type"] != "object" {
			return nil, fmt.Errorf("runtime input path must traverse object properties")
		}
		current = next
	}
	return current, nil
}
