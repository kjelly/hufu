package team

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
)

func runtimeOwnsFilesRead(compiled *CompiledResultContract) bool {
	return compiled != nil && compiled.evidenceKeys != nil && compiled.evidenceKeys.RuntimeOwnedFilesRead
}

// Native view observations are already task/attempt/branch/model scoped and
// hash checked. Project these facts once; never ask the model to repeat them.
func (c *Coordinator) observedResultFilesRead(ctx context.Context, taskID string, compiled *CompiledResultContract, payload *ResultPayload) ([]FileRef, error) {
	refs, err := c.verifiedResultEvidenceKeyRefs(ctx, taskID, compiled)
	if err != nil {
		return nil, err
	}
	observed := make(map[string]ArtifactRef)
	for _, ref := range refs {
		observed[ref.ID] = ref
	}
	if payload != nil {
		value, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return nil, err
		}
		for _, pointer := range compiled.evidenceKeys.Pointers {
			if err := validateObservedReferencePath(value, fixedCollectionPointerTokens(pointer), "/structured_payload", observed); err != nil {
				if choice, ok := errors.AsType[*resultEvidenceChoiceError](err); ok {
					choice.Hint = evidenceKeyCorrectionHint(refs)
				}
				return nil, err
			}
		}
	}
	reads := make([]FileRef, 0, len(observed))
	for id := range observed {
		reads = append(reads, FileRef{Path: id})
	}
	slices.SortFunc(reads, func(a, b FileRef) int { return cmp.Compare(a.Path, b.Path) })
	return reads, nil
}

func validateObservedReferencePath(value any, tokens []string, pointer string, observed map[string]ArtifactRef) error {
	if len(tokens) == 0 {
		id, ok := value.(string)
		if !ok || observed[id].ID != id || id == "" {
			return &resultEvidenceChoiceError{Reference: id, Pointer: pointer}
		}
		return nil
	}
	if tokens[0] == "*" {
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: observed evidence selector requires an array", pointer)
		}
		for index, item := range items {
			if err := validateObservedReferencePath(item, tokens[1:], pointer+"/"+strconv.Itoa(index), observed); err != nil {
				return err
			}
		}
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: observed evidence selector requires an object", pointer)
	}
	return validateObservedReferencePath(object[tokens[0]], tokens[1:], pointer+"/"+escapeJSONPointerToken(tokens[0]), observed)
}

func (a *localResultAdmission) bindObservedFilesRead(ctx context.Context, compiled *CompiledResultContract, input SubmitResultInput, result *TaskResult, contract taskResultSubmissionContract) error {
	if !runtimeOwnsFilesRead(compiled) {
		return nil
	}
	if input.FilesRead != nil {
		return fmt.Errorf("files_read is runtime-owned; omit it entirely")
	}
	if !taskResultStatusIsSuccessful(result.Status) && result.StructuredPayload == nil {
		return nil
	}
	reads, err := a.coordinator.observedResultFilesRead(ctx, a.todoID, compiled, result.StructuredPayload)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	result.FilesRead = reads
	return contract.validateWorkerClaims(result)
}
