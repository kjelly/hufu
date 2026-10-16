package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
)

// Schema-owned pointers bind a result's references to accepted dependency reads.
// The runtime interprets neither task prose nor the schema's domain vocabulary.
type resultEvidenceReferencesSpec struct {
	ItemsPointer         string                `json:"items_pointer"`
	TaskIDPointer        string                `json:"task_id_pointer"`
	ReferencesPointer    string                `json:"references_pointer"`
	ExcludeTaskIDPointer string                `json:"exclude_task_id_pointer,omitempty"`
	When                 *resultReferenceMatch `json:"when,omitempty"`
	SourceMatch          *resultReferenceMatch `json:"source_match,omitempty"`
}

type resultReferenceMatch struct {
	Pointer string `json:"pointer"`
	Value   string `json:"value"`
}

func compileResultEvidenceReferences(root map[string]any) (*resultEvidenceReferencesSpec, error) {
	raw, present := root["x-hufu-evidence-references"]
	if !present {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	spec := new(resultEvidenceReferencesSpec)
	if err := json.Unmarshal(data, spec, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("x-hufu-evidence-references: %w", err)
	}
	for _, pointer := range []string{spec.ItemsPointer, spec.TaskIDPointer, spec.ReferencesPointer} {
		if pointer == "" {
			return nil, fmt.Errorf("x-hufu-evidence-references requires all pointer fields")
		}
		if err := validateJSONPointer(pointer); err != nil {
			return nil, err
		}
	}
	if spec.ExcludeTaskIDPointer != "" {
		if err := validateJSONPointer(spec.ExcludeTaskIDPointer); err != nil {
			return nil, err
		}
	}
	for _, match := range []*resultReferenceMatch{spec.When, spec.SourceMatch} {
		if match == nil {
			continue
		}
		if match.Pointer == "" || match.Value == "" {
			return nil, fmt.Errorf("reference conditions require a pointer and non-empty value")
		}
		if err := validateJSONPointer(match.Pointer); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

// reads is nil during payload admission; occurrence commit supplies the proposed
// files_read (including an explicitly empty slice), and replay supplies stored reads.
func validateResultEvidenceReferences(value any, spec *resultEvidenceReferencesSpec, ids []string, results []TaskResult, reads *[]FileRef) error {
	if spec == nil {
		return nil
	}
	raw, err := resolveJSONPointer(value, spec.ItemsPointer)
	if err != nil {
		return err
	}
	entries, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("evidence references must be an array")
	}
	for index, entry := range entries {
		if spec.When != nil {
			v, err := resolveJSONPointer(entry, spec.When.Pointer)
			if err != nil {
				return err
			}
			if v != spec.When.Value {
				continue
			}
		}
		id := evidenceString(entry, spec.TaskIDPointer)
		if !slices.Contains(ids, id) || (spec.ExcludeTaskIDPointer != "" && id == evidenceString(entry, spec.ExcludeTaskIDPointer)) {
			return fmt.Errorf("%s/%d: reference task %q must be a different accepted evidence_from task", spec.ItemsPointer, index, id)
		}
		i := slices.IndexFunc(results, func(r TaskResult) bool { return r.TaskID == id })
		if i < 0 || !taskResultStatusIsSuccessful(results[i].Status) || results[i].StructuredPayload == nil {
			return fmt.Errorf("reference task %q has no accepted structured result", id)
		}
		payload := results[i].StructuredPayload
		digest := sha256.Sum256(payload.Value)
		if payload.SHA256 != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("reference task %q payload hash does not match accepted evidence", id)
		}
		if spec.SourceMatch != nil {
			source, err := decodeResultContractJSON(results[i].StructuredPayload.Value)
			if err != nil {
				return err
			}
			v, err := resolveJSONPointer(source, spec.SourceMatch.Pointer)
			if err != nil || v != spec.SourceMatch.Value {
				return fmt.Errorf("reference task %q does not satisfy the declared source condition", id)
			}
		}
		rawRefs, err := resolveJSONPointer(entry, spec.ReferencesPointer)
		if err != nil {
			return err
		}
		refs, ok := rawRefs.([]any)
		if !ok || len(refs) == 0 {
			return fmt.Errorf("reference task %q requires non-empty evidence", id)
		}
		for _, rawRef := range refs {
			ref, ok := rawRef.(string)
			if !ok || ref == "" {
				return fmt.Errorf("evidence reference must be a non-empty string")
			}
			if !slices.ContainsFunc(results[i].FilesRead, func(f FileRef) bool { return f.Path == ref }) {
				owners := []string{}
				for _, r := range results {
					if taskResultStatusIsSuccessful(r.Status) && slices.Contains(ids, r.TaskID) && slices.ContainsFunc(r.FilesRead, func(f FileRef) bool { return f.Path == ref }) {
						owners = append(owners, r.TaskID)
					}
				}
				return fmt.Errorf("%s/%d: evidence %q was not read by task %q; accepted readers: %v", spec.ItemsPointer, index, ref, id, owners)
			}
			if reads != nil && !slices.ContainsFunc(*reads, func(f FileRef) bool { return f.Path == ref }) {
				return fmt.Errorf("%s/%d: evidence %q is missing from this result's files_read", spec.ItemsPointer, index, ref)
			}
		}
	}
	return nil
}

func (c *Coordinator) validateResultReferenceReads(todoID string, result *TaskResult) error {
	item := c.todoItemByID(todoID)
	if item == nil || item.ResultContract == nil {
		return nil
	}
	compiled, err := c.compiledResultContract(item.ResultContract)
	if err != nil {
		return err
	}
	if compiled.evidenceReferences == nil {
		return nil
	}
	if result == nil || result.StructuredPayload == nil {
		return fmt.Errorf("evidence references require a structured result")
	}
	value, err := decodeResultContractJSON(result.StructuredPayload.Value)
	if err != nil {
		return err
	}
	return validateResultEvidenceReferences(value, compiled.evidenceReferences, item.EvidenceFrom, c.dependencyResultsForTask(todoID), &result.FilesRead)
}
