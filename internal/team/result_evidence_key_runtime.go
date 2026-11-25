package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
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
	return c.resultArtifactObservationRefs(ctx, todoID, compiled, false)
}

func (c *Coordinator) resultArtifactObservationRefs(ctx context.Context, todoID string, compiled *CompiledResultContract, includeDecisionReads bool) (map[string]ArtifactRef, error) {
	identity, err := c.resultSubmissionEvidenceIdentity(ctx, todoID)
	if err != nil {
		return nil, err
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok || metadata.RunID != identity.RunID || metadata.TaskID != identity.TaskID || metadata.Attempt != identity.Attempt || metadata.AgentName != identity.Agent {
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
	origin, err := c.resultObservationScope(ctx, todoID, compiled, metadata, events)
	if err != nil {
		return nil, err
	}
	scope := decisionPrimitiveScope(origin.events, origin.branch, metadata)
	refs := make(map[string]ArtifactRef)
	for _, event := range origin.events {
		decisionRead := event.Type == string(EventDecisionArtifactObserved)
		if event.Type != string(EventArtifactEvidenceObserved) && (!includeDecisionReads || !decisionRead) {
			continue
		}
		if event.RunID != identity.RunID || event.TaskID != identity.TaskID || event.Attempt != identity.Attempt || event.Actor != identity.Agent || event.BranchID != origin.branch {
			continue
		}
		if err := validateDecisionObservationEvent(event); err != nil {
			return nil, err
		}
		var observation decisionArtifactObservation
		if err := json.Unmarshal(event.Payload, &observation); err != nil {
			return nil, err
		}
		if observation.Scope != scope || observation.SchemaHash != origin.schema || observation.ModelExecutionID != metadata.ModelExecutionID || observation.ErrorCode != "" {
			continue
		}
		limit := 1 << 20
		if decisionRead {
			limit = resultDecisionContextMax
		}
		if _, err := c.readDecisionArtifact(ctx, observation.Output, limit); err != nil {
			return nil, fmt.Errorf("evidence_key_observation_integrity_invalid: %w", err)
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
	input, pathErr := c.resolveResultWorksetPaths(ctx, todoID, input)
	if pathErr != nil && input == "" {
		return "", pathErr
	}
	resolved, err := c.resolveResultArtifactEvidenceKeys(ctx, todoID, input)
	if err != nil && !isResultEvidenceFormatError(err) {
		return "", err
	}
	return resolved, errors.Join(pathErr, err)
}

// On a typed choice failure the returned JSON is for source inspection only.
func (c *Coordinator) resolveResultArtifactEvidenceKeys(ctx context.Context, todoID, input string) (string, error) {
	_, compiled, err := c.boundResultContract(todoID)
	if err != nil {
		return "", err
	}
	if compiled == nil || compiled.evidenceKeys == nil {
		return input, nil
	}
	_, err = decodeUniqueJSON([]byte(input))
	if err != nil {
		return "", rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	// Use the existing shorthand normalization only after raw duplicate-key
	// validation. This also resolves keys in legacy files_read shorthand.
	normalized, err := normalizeSubmitResultInput([]byte(input))
	if err != nil {
		return "", rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	value, err := decodeUniqueJSON(normalized)
	if err != nil {
		return "", err
	}
	lookup := c.resultEvidenceKeyResolver(ctx, todoID, compiled)
	var choiceErr error
	changed := false
	resolve := func(key, pointer string) (any, error) {
		id, err := lookup(key)
		if err == nil {
			changed = true
			return id, nil
		}
		err = fmt.Errorf("%s: %w", pointer, err)
		if !isResultReferenceChoiceError(err) {
			return nil, err
		}
		if choiceErr == nil {
			choiceErr = err
		}
		// An unknown selector makes no literal artifact claim. Preserve the
		// rejected choice's diagnostic, then inspect the other claims.
		changed = true
		return nil, nil
	}
	for _, pointer := range compiled.evidenceKeys.Pointers {
		_, pathErr := transformEvidenceKeyPath(value, append([]string{"structured_payload"}, fixedCollectionPointerTokens(pointer)...), "", resolve)
		if pathErr != nil {
			return "", pathErr
		}
	}
	root, _ := value.(map[string]any)
	for field, raw := range root {
		if !strings.EqualFold(field, "files_read") {
			continue
		}
		reads, _ := raw.([]any)
		for index, read := range reads {
			next, readErr := transformEvidenceReadKey(read, index, resolve)
			if readErr != nil {
				return "", readErr
			}
			reads[index] = next
		}
	}
	if !changed {
		return input, nil
	}
	raw, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return string(raw), choiceErr
}

// Direct submission has historically accepted FileRef's v1 JSON field aliases.
// Resolve the same members without changing the strict collector's schema.
func transformEvidenceReadKey(read any, index int, resolve func(string, string) (any, error)) (any, error) {
	pointer := "/files_read/" + strconv.Itoa(index)
	object, ok := read.(map[string]any)
	if !ok {
		return transformEvidenceKeyPath(read, nil, pointer, resolve)
	}
	for name, value := range object {
		if strings.EqualFold(name, "path") {
			next, err := transformEvidenceKeyPath(value, nil, pointer+"/path", resolve)
			if err != nil {
				return nil, err
			}
			object[name] = next
		}
	}
	return object, nil
}

func (c *Coordinator) resultEvidenceKeyResolver(ctx context.Context, todoID string, compiled *CompiledResultContract) func(string) (string, error) {
	var refs map[string]ArtifactRef
	return func(key string) (string, error) {
		if refs == nil {
			var readErr error
			if runtimeOwnsFilesRead(compiled) {
				refs, readErr = c.verifiedResultEvidenceKeyRefs(ctx, todoID, compiled)
			} else {
				refs, readErr = c.resultEvidenceKeyRefs(ctx, todoID, compiled)
			}
			if readErr != nil {
				return "", readErr
			}
		}
		source, exists := refs[key]
		if !exists {
			if runtimeOwnsFilesRead(compiled) {
				return "", &resultEvidenceChoiceError{Reference: key, Hint: evidenceKeyCorrectionHint(refs)}
			}
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
			return "", fmt.Errorf("evidence_key_source_unavailable: %w", openErr)
		}
		if verifyErr := store.Verify(ctx, current); verifyErr != nil {
			return "", fmt.Errorf("evidence_key_source_integrity_invalid: %w", verifyErr)
		}
		return current.ID, nil
	}
}

// Check each independently readable source before any normalization action
// or sibling collector can run. The submission decoder owns malformed fields.
func (c *Coordinator) validateSubmittedEvidenceKeyReads(ctx context.Context, todoID string, compiled *CompiledResultContract, raw []byte) error {
	if compiled == nil || compiled.evidenceKeys == nil || runtimeOwnsFilesRead(compiled) || len(raw) == 0 {
		return nil
	}
	if decoded, wrapped, err := decodeSerializedJSONText("files_read", raw); err == nil && wrapped {
		raw = decoded
	}
	entries, err := readSourceEntries(raw)
	if err != nil {
		entries = []jsontext.Value{raw}
	}
	resolve := c.resultEvidenceKeyResolver(ctx, todoID, compiled)
	for index, entry := range entries {
		if decoded, wrapped, err := decodeSerializedJSONText("files_read", entry); err == nil && wrapped {
			entry = decoded
		}
		if err := validateReadableEvidencePath(entry, index, resolve); err != nil {
			return rejectResultSourceRead(err)
		}
	}
	return nil
}

func validateReadableEvidencePath(entry jsontext.Value, index int, resolve func(string) (string, error)) error {
	pointer := "/files_read/" + strconv.Itoa(index)
	check := func(key, path string) (any, error) {
		id, err := resolve(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return id, nil
	}
	var key string
	if json.Unmarshal(entry, &key) == nil {
		_, err := transformEvidenceKeyPath(key, nil, pointer, check)
		return err
	}
	for _, field := range readableSourceMembers(string(entry)) {
		if strings.EqualFold(field.name, "path") && json.Unmarshal(field.raw, &key) == nil {
			if _, err := transformEvidenceKeyPath(key, nil, pointer+"/path", check); err != nil {
				return err
			}
		}
	}
	return nil
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
