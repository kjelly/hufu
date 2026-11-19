package team

import (
	"context"
	"fmt"
	"slices"
)

// An invalid selection is model-authored data, not a failure of the observed
// source. Construct this only after authenticating the complete observation set.
type resultEvidenceChoiceError struct {
	Reference string
	Hint      string
	Pointer   string
}

func (e *resultEvidenceChoiceError) Error() string {
	if e.Pointer != "" {
		return fmt.Sprintf("%s: evidence_reference_unknown: %q was not observed in this work attempt. %s", e.Pointer, e.Reference, e.Hint)
	}
	return fmt.Sprintf("evidence_reference_unknown: %q was not observed in this work attempt; %s", e.Reference, e.Hint)
}

func (c *Coordinator) verifiedResultEvidenceKeyRefs(ctx context.Context, taskID string, compiled *CompiledResultContract) (map[string]ArtifactRef, error) {
	refs, err := c.resultEvidenceKeyRefs(ctx, taskID, compiled)
	if err != nil || len(refs) == 0 {
		return refs, err
	}
	root := c.artifactStoreRootPath()
	if scope, ok := artifactAccessScopeFromContext(ctx); ok && scope.StoreRoot != "" {
		root = scope.StoreRoot
	}
	store, err := OpenFileArtifactStoreReadOnly(root, c.projectDir)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		current, ok := c.authorizedDecisionEvidence(ctx, ref.ID)
		if !ok || current.SHA256 != ref.SHA256 {
			return nil, fmt.Errorf("observed artifact source is unauthorized")
		}
		if err := store.Verify(ctx, current); err != nil {
			return nil, fmt.Errorf("observed artifact source integrity: %w", err)
		}
	}
	return refs, nil
}

// Finite choices are a model-facing projection only. Canonical storage still
// contains exact full IDs; no approximate matching can create an evidence grant.
func constrainResultEvidenceChoices(schema map[string]any, spec *resultEvidenceKeysSpec, refs map[string]ArtifactRef) {
	if spec == nil || !spec.RuntimeOwnedFilesRead {
		return
	}
	choices := make([]string, 0, 2*len(refs))
	for key, ref := range refs {
		choices = append(choices, key, ref.ID)
	}
	slices.Sort(choices)
	choices = slices.Compact(choices)
	for _, pointer := range spec.Pointers {
		constrainEvidenceChoicePath(schema, fixedCollectionPointerTokens(pointer), choices)
	}
}

func constrainEvidenceChoicePath(schema map[string]any, tokens, choices []string) {
	if schema == nil {
		return
	}
	if len(tokens) == 0 {
		if schema["type"] == "string" {
			if len(choices) == 0 {
				// enum must be nonempty. Empty evidence arrays remain available.
				schema["not"] = map[string]any{}
			} else {
				schema["enum"] = slices.Clone(choices)
			}
		}
		return
	}
	properties, _ := schema["properties"].(map[string]any)
	if tokens[0] == "*" {
		if items, ok := schema["items"].(map[string]any); ok {
			constrainEvidenceChoicePath(items, tokens[1:], choices)
			return
		}
		for _, raw := range properties {
			child, _ := raw.(map[string]any)
			constrainEvidenceChoicePath(child, tokens[1:], choices)
		}
		return
	}
	child, _ := properties[tokens[0]].(map[string]any)
	constrainEvidenceChoicePath(child, tokens[1:], choices)
}

// A bad selection may consume the existing format-repair allowance only after
// independent receipts, bindings and settled decisions have also been checked.
func (c *Coordinator) collectionEvidenceRejection(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, raw string, err error) error {
	return c.resultEvidenceRejection(ctx, cp.TaskID, cp.Attempt, work.Workspace, raw, err)
}

func (c *Coordinator) localEvidenceRejection(ctx context.Context, taskID, raw string, err error) error {
	identity, identityErr := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if identityErr != nil {
		return rejectLocalResult(RepairFailureSource, identityErr)
	}
	return c.resultEvidenceRejection(ctx, taskID, identity.Attempt, c.session.Workspace, raw, err)
}

func (c *Coordinator) resultEvidenceRejection(ctx context.Context, taskID string, attempt int, workspace, raw string, err error) error {
	if !isResultReferenceChoiceError(err) {
		return rejectLocalResult(RepairFailureSource, err)
	}
	_, compiled, sourceErr := c.boundResultContract(taskID)
	if sourceErr != nil {
		return rejectLocalResult(RepairFailureSource, sourceErr)
	}
	check := func(_ string, checkErr error) bool {
		if sourceErr == nil {
			sourceErr = checkErr
		}
		return checkErr == nil
	}
	if !c.validateIndependentResultClaims(ctx, taskID, attempt, workspace, compiled, raw, check) {
		return rejectLocalResult(RepairFailureSource, sourceErr)
	}
	return rejectLocalResult(RepairFailureInvalidSchema, err)
}
