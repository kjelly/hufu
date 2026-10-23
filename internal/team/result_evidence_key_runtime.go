package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// This is a projection of the existing hash-chained observation, not a second
// registry. Include the task occurrence, branch and model execution so a short
// key cannot grant another attempt/world access to an artifact.
func artifactEvidenceKey(event RunEvent, observation decisionArtifactObservation, source ArtifactRef) string {
	digest, _ := decisionObjectHash([]any{"artifact-evidence-key-v1", event.BranchID, event.RunID, event.TaskID, event.Attempt, event.Actor, observation.ModelExecutionID, observation.Scope, observation.SchemaHash, source.ID, source.SHA256})
	return evidenceKeyPrefix + digest[:12]
}

func rememberArtifactEvidenceKey(refs map[string]ArtifactRef, key string, source ArtifactRef) error {
	if previous, exists := refs[key]; exists && (previous.ID != source.ID || previous.SHA256 != source.SHA256) {
		return fmt.Errorf("evidence_key_collision")
	}
	refs[key] = source
	return nil
}

func (c *Coordinator) resultEvidenceKeyRefs(ctx context.Context, todoID string, compiled *CompiledResultContract) (map[string]ArtifactRef, error) {
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, todoID)
	if err != nil {
		return nil, err
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok || metadata.TaskID != identity.TaskID || metadata.Attempt != identity.Attempt || metadata.AgentName != identity.Agent {
		return nil, fmt.Errorf("evidence_key_identity_invalid")
	}
	if !c.hasDurableEventJournal() {
		return nil, fmt.Errorf("evidence_key_journal_unavailable")
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return nil, err
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	scope := decisionPrimitiveScope(events, c.activeBranchID(), metadata)
	refs := make(map[string]ArtifactRef)
	for _, event := range events {
		if event.Type != string(EventArtifactEvidenceObserved) || event.RunID != identity.RunID || event.TaskID != identity.TaskID || event.Attempt != identity.Attempt || event.Actor != identity.Agent || event.BranchID != c.activeBranchID() {
			continue
		}
		if err := validateDecisionObservationEvent(event); err != nil {
			return nil, err
		}
		var observation decisionArtifactObservation
		if err := json.Unmarshal(event.Payload, &observation); err != nil {
			return nil, err
		}
		if observation.Scope != scope || observation.SchemaHash != compiled.SchemaSHA256 || observation.ModelExecutionID != metadata.ModelExecutionID || observation.ErrorCode != "" {
			continue
		}
		if _, err := c.readDecisionArtifact(ctx, observation.Output, 1<<20); err != nil {
			return nil, fmt.Errorf("evidence_key_observation_integrity_invalid")
		}
		for _, source := range observation.Sources {
			key := artifactEvidenceKey(event, observation, source)
			if err := rememberArtifactEvidenceKey(refs, key, source); err != nil {
				return nil, err
			}
		}
	}
	return refs, nil
}

func (c *Coordinator) resolveResultEvidenceKeys(ctx context.Context, todoID, input string) (string, error) {
	if !strings.Contains(input, evidenceKeyPrefix) {
		return input, nil
	}
	_, compiled, err := c.boundResultContract(todoID)
	if err != nil {
		return "", err
	}
	if compiled == nil || compiled.evidenceKeys == nil {
		return input, nil
	}
	_, err = decodeUniqueJSON([]byte(input))
	if err != nil {
		return "", err
	}
	// Use the existing shorthand normalization only after raw duplicate-key
	// validation. This also resolves keys in legacy files_read shorthand.
	normalized, err := normalizeSubmitResultInput([]byte(input))
	if err != nil {
		return "", err
	}
	value, err := decodeUniqueJSON(normalized)
	if err != nil {
		return "", err
	}
	var refs map[string]ArtifactRef
	resolve := func(key string) (string, error) {
		if refs == nil {
			var readErr error
			refs, readErr = c.resultEvidenceKeyRefs(ctx, todoID, compiled)
			if readErr != nil {
				return "", readErr
			}
		}
		source, exists := refs[key]
		if !exists {
			return "", fmt.Errorf("evidence_key_unknown_or_out_of_scope; %s", evidenceKeyCorrectionHint(refs))
		}
		current, authorized := c.authorizedDecisionEvidence(ctx, source.ID)
		if !authorized || current.SHA256 != source.SHA256 {
			return "", fmt.Errorf("evidence_key_source_unauthorized")
		}
		storeRoot := c.artifactStoreRootPath()
		if scope, ok := artifactAccessScopeFromContext(ctx); ok && scope.StoreRoot != "" {
			storeRoot = scope.StoreRoot
		}
		store, openErr := OpenFileArtifactStoreReadOnly(storeRoot, c.projectDir)
		if openErr != nil {
			return "", fmt.Errorf("evidence_key_source_unavailable")
		}
		if verifyErr := store.Verify(ctx, current); verifyErr != nil {
			return "", fmt.Errorf("evidence_key_source_integrity_invalid")
		}
		return current.ID, nil
	}
	changed := false
	for _, pointer := range compiled.evidenceKeys.Pointers {
		_, didChange, pathErr := transformEvidenceKeyPath(value, append([]string{"structured_payload"}, fixedCollectionPointerTokens(pointer)...), resolve)
		if pathErr != nil {
			return "", pathErr
		}
		changed = changed || didChange
	}
	// files_read is the existing canonical read projection, not a new evidence
	// authority. Support its documented string shorthand as well as path objects.
	root, _ := value.(map[string]any)
	reads, _ := root["files_read"].([]any)
	for index, read := range reads {
		tokens := []string(nil)
		if _, object := read.(map[string]any); object {
			tokens = []string{"path"}
		}
		next, didChange, readErr := transformEvidenceKeyPath(read, tokens, resolve)
		if readErr != nil {
			return "", readErr
		}
		reads[index] = next
		changed = changed || didChange
	}
	if !changed {
		return input, nil
	}
	raw, err := json.Marshal(value, json.Deterministic(true))
	return string(raw), err
}

// The bounded runtime-owned map reaches the existing repair diagnostic. Never
// truncate a map into apparently complete authority or guess a replacement.
func evidenceKeyCorrectionHint(refs map[string]ArtifactRef) string {
	if len(refs) == 0 {
		return "No keys were issued in this scope; do not guess evidence."
	}
	if len(refs) > 16 {
		return "Issued key map exceeds the diagnostic limit; consult the successful view responses."
	}
	ids := make(map[string]string, len(refs))
	for key, ref := range refs {
		ids[key] = ref.ID
	}
	raw, err := json.Marshal(ids, json.Deterministic(true))
	if err != nil || len(raw) > 1024 {
		return "Issued key map exceeds the diagnostic limit; consult the successful view responses."
	}
	return "Runtime-issued keys (source authorization/integrity still required): " + string(raw)
}
