package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// This extension binds schema-declared review entries to accepted evidence_from
// payloads. It interprets no prose and has no consumer-specific vocabulary.
type resultEvidenceInputsSpec struct {
	RuntimeOwned    bool              `json:"runtime_owned,omitempty"`
	RuntimeBind     bool              `json:"runtime_bind,omitzero"`
	RuntimeComplete bool              `json:"runtime_complete,omitzero"`
	ItemsPointer    string            `json:"items_pointer"`
	TaskIDPointer   string            `json:"task_id_pointer"`
	HashPointer     string            `json:"hash_pointer"`
	CompletePointer string            `json:"complete_pointer"`
	ValueBindings   map[string]string `json:"value_bindings,omitempty"`
	arraySchema     *jsonschema.Schema
}

// Both spellings exist in pinned contracts and select the same runtime owner.
func (s *resultEvidenceInputsSpec) runtimeOwned() bool {
	return s != nil && (s.RuntimeOwned || s.RuntimeBind)
}

func (s *resultEvidenceInputsSpec) ownsTarget(pointer string) bool {
	return s.runtimeOwned() && (resultPointersOverlap(pointer, s.ItemsPointer) ||
		s.RuntimeComplete && resultPointersOverlap(pointer, s.CompletePointer))
}

func compileResultEvidenceInputs(root map[string]any) (*resultEvidenceInputsSpec, error) {
	raw, present := root["x-hufu-evidence-inputs"]
	if !present {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	spec := new(resultEvidenceInputsSpec)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(spec); err != nil {
		return nil, fmt.Errorf("x-hufu-evidence-inputs: %w", err)
	}
	for _, pointer := range []string{spec.ItemsPointer, spec.TaskIDPointer, spec.HashPointer, spec.CompletePointer} {
		if pointer == "" {
			return nil, fmt.Errorf("x-hufu-evidence-inputs requires all pointer fields")
		}
		if err := validateJSONPointer(pointer); err != nil {
			return nil, err
		}
	}
	for target, source := range spec.ValueBindings {
		if target == "" || source == "" {
			return nil, fmt.Errorf("input value bindings require non-empty pointers")
		}
		if err := validateJSONPointer(target); err != nil {
			return nil, err
		}
		if err := validateJSONPointer(source); err != nil {
			return nil, err
		}
	}
	if spec.runtimeOwned() {
		if err := validateRuntimeEvidenceBinding(root, spec); err != nil {
			return nil, err
		}
	}
	if spec.RuntimeComplete {
		complete, err := fixedCollectionSchemaAt(root, spec.CompletePointer)
		if !spec.runtimeOwned() || err != nil || complete["type"] != "boolean" || complete["const"] != true {
			return nil, fmt.Errorf("runtime_complete requires runtime-owned evidence inputs and a complete_pointer declared as boolean const true")
		}
	}
	if spec.RuntimeOwned {
		if err := validateRuntimeEvidenceInputSchema(root, spec); err != nil {
			return nil, err
		}
	}
	return spec, nil
}

func validateResultEvidenceInputs(value any, spec *resultEvidenceInputsSpec, ids []string, results []TaskResult) error {
	if spec == nil {
		return nil
	}
	completeValue, err := resolveJSONPointer(value, spec.CompletePointer)
	if err != nil {
		return err
	}
	complete, ok := completeValue.(bool)
	if !ok {
		return fmt.Errorf("input review completeness must be a boolean")
	}
	if spec.RuntimeComplete && !complete {
		return fmt.Errorf("runtime-owned completion %s must be true", spec.CompletePointer)
	}
	raw, err := resolveJSONPointer(value, spec.ItemsPointer)
	if err != nil {
		return err
	}
	entries, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("input review must be an array")
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		id := evidenceString(entry, spec.TaskIDPointer)
		if !slices.Contains(ids, id) || seen[id] {
			return fmt.Errorf("input review task %q is not a unique evidence_from ID", id)
		}
		seen[id] = true
		index := slices.IndexFunc(results, func(result TaskResult) bool { return result.TaskID == id })
		if index < 0 || !taskResultStatusIsSuccessful(results[index].Status) || results[index].StructuredPayload == nil {
			return fmt.Errorf("input review task %q has no accepted structured payload", id)
		}
		payload := results[index].StructuredPayload
		sum := sha256.Sum256(payload.Value)
		if payload.SHA256 != hex.EncodeToString(sum[:]) || evidenceString(entry, spec.HashPointer) != payload.SHA256 {
			return fmt.Errorf("input review task %q payload hash does not match accepted evidence", id)
		}
		source, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return err
		}
		for targetPointer, sourcePointer := range spec.ValueBindings {
			targetValue, targetErr := resolveJSONPointer(entry, targetPointer)
			sourceValue, sourceErr := resolveJSONPointer(source, sourcePointer)
			if targetErr != nil || sourceErr != nil || !reflect.DeepEqual(targetValue, sourceValue) {
				return fmt.Errorf("input review task %q field %s does not match accepted evidence %s", id, targetPointer, sourcePointer)
			}
		}
	}
	if complete && (len(ids) == 0 || len(seen) != len(ids)) {
		return fmt.Errorf("complete input review must bind every evidence_from task")
	}
	return nil
}

func (c *Coordinator) validateSubmittedEvidenceInputs(ctx context.Context, todoID string, payload *ResultPayload) error {
	return c.validateEvidenceInputBindings(ctx, todoID, payload, false)
}

// Accepted results retain their recorded decision provenance across forks.
// New submissions must still resolve decisions only in the current scope.
func (c *Coordinator) validateAcceptedEvidenceInputs(ctx context.Context, todoID string, payload *ResultPayload) error {
	return c.validateEvidenceInputBindings(ctx, todoID, payload, true)
}

func (c *Coordinator) validateEvidenceInputBindings(ctx context.Context, todoID string, payload *ResultPayload, accepted bool) error {
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
	if !hasResultSourceBindings(compiled) {
		return nil
	}
	if payload == nil {
		if item.ResultContract.RequireStructured {
			return fmt.Errorf("input review requires a structured payload")
		}
		return c.validateIndependentResultSources(ctx, todoID, compiled, resultSourceOmissions{})
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return err
	}
	return c.validateResultSourceBindings(ctx, item, compiled, payload, value, nil, accepted)
}

func hasResultSourceBindings(compiled *CompiledResultContract) bool {
	return compiled.evidenceInputs != nil || compiled.evidenceReferences != nil || compiled.worksetInputs != nil || compiled.worksetPaths != nil ||
		len(compiled.fixedCollections) != 0 || len(compiled.runtimeInputs) != 0
}

// Validate stored values against authoritative sources without assembling a new
// payload. Accepted collections resolve their recorded decision events.
func (c *Coordinator) validateResultSourceBindings(ctx context.Context, item *TodoItem, compiled *CompiledResultContract, payload *ResultPayload, value any, reads *[]FileRef, accepted bool) error {
	todoID := item.ID
	if compiled.worksetInputs != nil {
		ids, err := c.verifiedResultWorksetInputIDs(ctx, todoID, compiled.worksetInputs)
		if err != nil {
			return err
		}
		if err := matchResultWorksetInputs(value, compiled.worksetInputs, ids); err != nil {
			return err
		}
	}
	runtimeInputs, err := c.resultRuntimeInputValuesWithContext(ctx, compiled, todoID)
	if err != nil {
		return err
	}
	if err := validateResultRuntimeInputs(value, compiled.runtimeInputs, runtimeInputs); err != nil {
		return err
	}
	results := c.dependencyResultsForBinding(todoID)
	if err := c.validateDependencyPayloads(ctx, todoID, results); err != nil {
		return err
	}
	plans, err := buildResultFixedCollections(compiled.fixedCollections, item.EvidenceFrom, results)
	if err != nil {
		return err
	}
	if accepted {
		plans, err = c.resolveDecisionPlans(ctx, todoID, plans, payload.CollectionBindings, false)
	} else {
		plans, err = c.resolveDecisionCollectionPlans(ctx, todoID, plans)
	}
	if err != nil {
		return err
	}
	if err := validateResultFixedCollections(value, payload.CollectionBindings, plans); err != nil {
		return err
	}
	if err := validateResultEvidenceInputs(value, compiled.evidenceInputs, item.EvidenceFrom, results); err != nil {
		return err
	}
	return c.validatePayloadReferenceSources(ctx, todoID, compiled, value, item.EvidenceFrom, results, reads)
}

// A rejected model representation can still carry independent source claims.
// At this point the runtime has loaded the original values and fixed plans.
func (c *Coordinator) validatePayloadSourcesAfterBindingShape(ctx context.Context, todoID string, compiled *CompiledResultContract, raw []byte, plans []resultFixedCollectionPlan) error {
	item := c.todoItemByID(todoID)
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return err
	}
	if err := projectFixedCollectionSourceRows(value, plans); err != nil {
		return err
	}
	value = canonicalResultScalars(value, compiled.schema, 0)
	results := c.dependencyResultsForBinding(todoID)
	if err := c.validateDependencyPayloads(ctx, todoID, results); err != nil {
		return err
	}
	if err := validateResultWorksetInputs(value, compiled.worksetInputs, item.WorksetBinding); err != nil {
		return err
	}
	// Runtime-owned rows have already been authenticated from the dependency
	// payloads. A rejected copy cannot take the place of that original source.
	if !compiled.evidenceInputs.runtimeOwned() {
		if err := validateResultEvidenceInputs(value, compiled.evidenceInputs, item.EvidenceFrom, results); err != nil {
			return err
		}
	}
	err = c.validatePayloadReferenceSources(ctx, todoID, compiled, value, item.EvidenceFrom, results, nil)
	if isResultEvidenceFormatError(err) {
		return nil
	}
	if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](err); shape {
		return nil
	}
	return err
}

func (c *Coordinator) validatePayloadReferenceSources(ctx context.Context, todoID string, compiled *CompiledResultContract, value any, evidenceFrom []string, results []TaskResult, reads *[]FileRef) error {
	if compiled.evidenceReferences != nil {
		results = c.dependencyResultsForTaskWithContext(ctx, todoID)
		if err := artifactReadContextError(ctx); err != nil {
			return err
		}
	}
	referenceErr := validateResultEvidenceReferences(value, compiled.evidenceReferences, evidenceFrom, results, reads)
	if referenceErr != nil {
		if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](referenceErr); !shape {
			return referenceErr
		}
	}
	if compiled.worksetPaths != nil {
		selection, err := c.verifiedWorksetPathSelection(ctx, todoID, compiled.worksetPaths)
		if err != nil {
			return err
		}
		if err := validateResultWorksetPaths(value, compiled.worksetPaths, selection); err != nil {
			return err
		}
	}
	return referenceErr
}
