package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
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
	conditionRoot        *jsonschema.Schema
}

type resultReferenceMatch struct {
	Pointer string `json:"pointer"`
	Value   string `json:"value"`
}

type resultEvidenceReferenceShapeError struct{ error }

func (e *resultEvidenceReferenceShapeError) Unwrap() error { return e.error }

func (spec *resultEvidenceReferencesSpec) bindConditionSchema(schema *jsonschema.Schema) {
	if spec != nil && spec.When != nil {
		spec.conditionRoot = schema
	}
}

// Validate the actual containers and branches on the condition path. A schema
// error on a disjoint property does not invalidate an otherwise readable
// condition. A failing assertion on an enclosing value does, including
// anyOf/oneOf, contains, or an invalid ancestor type.
func conditionPathConforms(err error, path []string) bool {
	if err == nil {
		return true
	}
	failure, ok := errors.AsType[*jsonschema.ValidationError](err)
	return ok && !conditionPathHasSchemaFailure(failure, path)
}

func conditionPathHasSchemaFailure(failure *jsonschema.ValidationError, path []string) bool {
	location := failure.InstanceLocation
	shared := min(len(location), len(path))
	if !slices.Equal(location[:shared], path[:shared]) {
		return false
	}
	switch cause := failure.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.AllOf, *kind.Reference:
		return len(failure.Causes) == 0 || slices.ContainsFunc(failure.Causes, func(child *jsonschema.ValidationError) bool {
			return conditionPathHasSchemaFailure(child, path)
		})
	case *kind.Required:
		return len(location) >= len(path) || slices.Contains(cause.Missing, path[len(location)])
	case *kind.PropertyNames:
		return len(location) >= len(path) || cause.Property == path[len(location)]
	case *kind.AdditionalProperties:
		return len(location) >= len(path) || slices.Contains(cause.Properties, path[len(location)])
	default:
		return true
	}
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
		return &resultEvidenceReferenceShapeError{err}
	}
	entries, array := raw.([]any)
	var shapeErr error
	if !array {
		shapeErr = &resultEvidenceReferenceShapeError{fmt.Errorf("%s: evidence references must be an array", spec.ItemsPointer)}
		entries = []any{raw}
	}
	validateRoot := sync.OnceValue(func() error {
		if spec.conditionRoot == nil {
			return errors.New("condition schema is unavailable")
		}
		return spec.conditionRoot.Validate(value)
	})
	for index, entry := range entries {
		location := fmt.Sprintf("%s/%d", spec.ItemsPointer, index)
		if _, wrapped := entry.([]any); wrapped && shapeErr == nil {
			shapeErr = &resultEvidenceReferenceShapeError{fmt.Errorf("%s: evidence reference must be an object", location)}
		}
		if err := inspectReferenceListLeaves(entry, 0, func(candidate any) error {
			err := validateResultEvidenceReference(candidate, index, location, spec, ids, results, reads, validateRoot)
			if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](err); shape {
				if shapeErr == nil {
					shapeErr = err
				}
				return nil
			}
			return err
		}); err != nil {
			return err
		}
	}
	return shapeErr
}

// Recursive array wrappers are syntax errors, but they may still contain
// claims at a declared source field. The callback never repairs the payload.
func inspectReferenceListLeaves(value any, depth int, visit func(any) error) error {
	if depth > maxDynamicSchemaDepth {
		return fmt.Errorf("evidence reference source inspection exceeds the nesting limit")
	}
	list, array := value.([]any)
	if !array {
		return visit(value)
	}
	for _, child := range list {
		if err := inspectReferenceListLeaves(child, depth+1, visit); err != nil {
			return err
		}
	}
	return nil
}

func validateResultEvidenceReference(entry any, index int, location string, spec *resultEvidenceReferencesSpec, ids []string, results []TaskResult, reads *[]FileRef, validateRoot func() error) error {
	var shapeErr error
	rememberShape := func(detail string) {
		if shapeErr == nil {
			shapeErr = &resultEvidenceReferenceShapeError{fmt.Errorf("%s: %s", location, detail)}
		}
	}
	if spec.When != nil {
		condition := evidenceString(entry, spec.When.Pointer)
		if condition != spec.When.Value {
			path := append(fixedCollectionPointerTokens(spec.ItemsPointer), strconv.Itoa(index))
			path = append(path, fixedCollectionPointerTokens(spec.When.Pointer)...)
			if condition != "" && conditionPathConforms(validateRoot(), path) {
				return nil
			}
			rememberShape("reference condition must satisfy its schema")
		}
	}
	rawID, _ := resolveJSONPointer(entry, spec.TaskIDPointer)
	if id, ok := rawID.(string); !ok || id == "" {
		rememberShape("reference task must be a non-empty string")
	}
	if err := inspectReferenceListLeaves(rawID, 0, func(candidate any) error {
		id, ok := candidate.(string)
		if !ok || id == "" {
			return nil
		}
		source, err := verifiedResultReferenceSource(entry, location, id, spec, ids, results)
		if err != nil {
			return err
		}
		return validateResultEvidenceReferencePaths(entry, location, spec.ReferencesPointer, source, ids, results, reads, rememberShape)
	}); err != nil {
		return err
	}
	return shapeErr
}

func validateResultEvidenceReferencePaths(entry any, location, pointer string, source TaskResult, ids []string, results []TaskResult, reads *[]FileRef, rememberShape func(string)) error {
	rawRefs, resolveErr := resolveJSONPointer(entry, pointer)
	refs, array := rawRefs.([]any)
	if resolveErr != nil || !array || len(refs) == 0 {
		rememberShape("evidence must be a non-empty array")
		refs = []any{rawRefs}
	}
	for _, raw := range refs {
		if _, wrapped := raw.([]any); wrapped {
			rememberShape("evidence reference must be a non-empty string")
		}
		if err := inspectReferenceListLeaves(raw, 0, func(candidate any) error {
			ref, ok := candidate.(string)
			if !ok || ref == "" {
				rememberShape("evidence reference must be a non-empty string")
				return nil
			}
			return validateReferenceRead(location, ref, source, ids, results, reads)
		}); err != nil {
			return err
		}
	}
	return nil
}

func verifiedResultReferenceSource(entry any, location, id string, spec *resultEvidenceReferencesSpec, ids []string, results []TaskResult) (TaskResult, error) {
	if !slices.Contains(ids, id) || (spec.ExcludeTaskIDPointer != "" && id == evidenceString(entry, spec.ExcludeTaskIDPointer)) {
		return TaskResult{}, fmt.Errorf("%s: reference task %q must be a different accepted evidence_from task", location, id)
	}
	i := slices.IndexFunc(results, func(r TaskResult) bool { return r.TaskID == id })
	if i < 0 || !taskResultStatusIsSuccessful(results[i].Status) || results[i].StructuredPayload == nil {
		return TaskResult{}, fmt.Errorf("reference task %q has no accepted structured result", id)
	}
	source := results[i]
	payload := source.StructuredPayload
	digest := sha256.Sum256(payload.Value)
	if payload.SHA256 != hex.EncodeToString(digest[:]) {
		return TaskResult{}, fmt.Errorf("reference task %q payload hash does not match accepted evidence", id)
	}
	if spec.SourceMatch != nil {
		value, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return TaskResult{}, err
		}
		match, err := resolveJSONPointer(value, spec.SourceMatch.Pointer)
		if err != nil || match != spec.SourceMatch.Value {
			return TaskResult{}, fmt.Errorf("reference task %q does not satisfy the declared source condition", id)
		}
	}
	return source, nil
}

func validateReferenceRead(location, path string, source TaskResult, ids []string, results []TaskResult, reads *[]FileRef) error {
	readPath := func(f FileRef) bool { return f.Path == path }
	if !slices.ContainsFunc(source.FilesRead, readPath) {
		var owners []string
		for _, result := range results {
			if taskResultStatusIsSuccessful(result.Status) && slices.Contains(ids, result.TaskID) && slices.ContainsFunc(result.FilesRead, readPath) {
				owners = append(owners, result.TaskID)
			}
		}
		return fmt.Errorf("%s: evidence %q was not read by task %q; accepted readers: %v", location, path, source.TaskID, owners)
	}
	if reads != nil && !slices.ContainsFunc(*reads, readPath) {
		return fmt.Errorf("%s: evidence %q is missing from this result's files_read", location, path)
	}
	return nil
}

func (c *Coordinator) validateResultReferenceReads(ctx context.Context, todoID string, result *TaskResult) error {
	if err := artifactReadContextError(ctx); err != nil {
		return err
	}
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
	if result != nil && result.StructuredPayload == nil && !item.ResultContract.RequireStructured {
		return nil
	}
	if result == nil || result.StructuredPayload == nil {
		return fmt.Errorf("evidence references require a structured result")
	}
	value, err := decodeResultContractJSON(result.StructuredPayload.Value)
	if err != nil {
		return err
	}
	results := c.dependencyResultsForTaskWithContext(ctx, todoID)
	if err := artifactReadContextError(ctx); err != nil {
		return err
	}
	return validateResultEvidenceReferences(value, compiled.evidenceReferences, item.EvidenceFrom, results, &result.FilesRead)
}
