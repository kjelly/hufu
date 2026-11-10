package team

import (
	"cmp"
	"context"
	"fmt"
	"slices"
)

func runtimeOwnsFilesRead(compiled *CompiledResultContract) bool {
	return compiled != nil && compiled.evidenceKeys != nil && compiled.evidenceKeys.RuntimeOwnedFilesRead
}

// Native view observations are already task/attempt/branch/model scoped and
// hash checked. Project these facts once; never ask the model to repeat them.
func (c *Coordinator) observedResultFilesRead(ctx context.Context, taskID string, compiled *CompiledResultContract, payload *ResultPayload) ([]FileRef, error) {
	refs, err := c.resultEvidenceKeyRefs(ctx, taskID, compiled)
	if err != nil {
		return nil, err
	}
	observed := make(map[string]ArtifactRef)
	for _, ref := range refs {
		current, ok := c.authorizedDecisionEvidence(ctx, ref.ID)
		if !ok || current.SHA256 != ref.SHA256 {
			return nil, fmt.Errorf("observed artifact source is unauthorized")
		}
		root := c.artifactStoreRootPath()
		if scope, ok := artifactAccessScopeFromContext(ctx); ok && scope.StoreRoot != "" {
			root = scope.StoreRoot
		}
		store, err := OpenFileArtifactStoreReadOnly(root, c.projectDir)
		if err != nil {
			return nil, err
		}
		if err := store.Verify(ctx, current); err != nil {
			return nil, fmt.Errorf("observed artifact source integrity: %w", err)
		}
		observed[ref.ID] = ref
	}
	if payload != nil {
		value, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return nil, err
		}
		for _, pointer := range compiled.evidenceKeys.Pointers {
			if err := validateObservedReferencePath(value, fixedCollectionPointerTokens(pointer), observed); err != nil {
				return nil, fmt.Errorf("%s: %w", pointer, err)
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

func validateObservedReferencePath(value any, tokens []string, observed map[string]ArtifactRef) error {
	if len(tokens) == 0 {
		id, ok := value.(string)
		if !ok || observed[id].ID != id || id == "" {
			return fmt.Errorf("evidence must name an artifact observed in this work attempt")
		}
		return nil
	}
	if tokens[0] == "*" {
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("observed evidence selector requires an array")
		}
		for _, item := range items {
			if err := validateObservedReferencePath(item, tokens[1:], observed); err != nil {
				return err
			}
		}
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("observed evidence selector requires an object")
	}
	return validateObservedReferencePath(object[tokens[0]], tokens[1:], observed)
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
