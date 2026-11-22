package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// AcceptedResultRepair freezes every semantic claim except the concrete invalid
// reference leaves. Path repair also freezes the original line suffix.
// It is authority only inside a validated recovery event.
type AcceptedResultRepair struct {
	Schema         jsontext.Value `json:"schema"`
	ResultSHA256   string         `json:"result_sha256"`
	SemanticSHA256 string         `json:"semantic_sha256"`
	Pointers       []string       `json:"pointers"`
	PathPointers   []string       `json:"path_pointers,omitempty"`
}

func validAcceptedResultRepair(p *AcceptedResultRepair) bool {
	if p == nil || !validCollectionDigest(p.ResultSHA256) || !validCollectionDigest(p.SemanticSHA256) || len(p.Pointers) == 0 {
		return false
	}
	for i, pointer := range p.Pointers {
		if !strings.HasPrefix(pointer, "/") || strings.Contains(pointer, "*") || i > 0 && pointer <= p.Pointers[i-1] {
			return false
		}
	}
	for i, pointer := range p.PathPointers {
		if !slices.Contains(p.Pointers, pointer) || i > 0 && pointer <= p.PathPointers[i-1] {
			return false
		}
	}
	return true
}

func validateAcceptedResultRepairTarget(item *TodoItem) error {
	cp := currentResultCollection(item)
	if item == nil || item.Status != TaskDone || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil || !taskResultStatusIsSuccessful(item.TypedResult.Status) ||
		item.ResultContract == nil || cp == nil || cp.State != ResultCollectionCommitted || cp.Response == nil || cp.ContractUpgrade != nil || cp.LegacySubmission != nil ||
		!metadataReadOnlyTask(item.SideEffect, item.Execution) || item.CheckpointPause || item.RecoveryState == RecoveryStateUnknown || item.RecoveryState == RecoveryStatePartial ||
		item.WorkerWorkspace.isolated() || cp.FanoutLeaf || len(item.ExecutionTopology) > 1 || item.ExecutionReceipt.ExecutionWorldID != "" ||
		(item.DecisionProfile != "" && item.DecisionProfile != "off") || !collectionWorkCompleted(item.ExecutionReceipt, true) {
		return fmt.Errorf("accepted-result repair requires one accepted successful read-only local collection")
	}
	return validateResultCollectionCheckpoint(cp)
}

func validateAcceptedResultRepairProof(item *TodoItem, p *AcceptedResultRepair, target *ResultContractRef) error {
	if !validAcceptedResultRepair(p) || item == nil || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil {
		return fmt.Errorf("invalid accepted-result repair source")
	}
	if target == nil || len(p.Schema) > resultContractPromptSchemaMaxBytes {
		return fmt.Errorf("invalid accepted-result repair schema")
	}
	compiled, err := CompileResultContractDocument(target.ID, p.Schema)
	if err != nil || compiled.SchemaSHA256 != target.SchemaSHA256 || !runtimeOwnsFilesRead(compiled) {
		return fmt.Errorf("accepted-result repair schema binding changed")
	}
	value, err := decodeResultContractJSON(item.TypedResult.StructuredPayload.Value)
	if err != nil {
		return err
	}
	if err := compiled.schema.Validate(value); err != nil {
		return fmt.Errorf("accepted reference repair requires otherwise schema-compatible claims: %w", err)
	}
	if !resultProjectionMatchesAccepted(item.TypedResult, compiled) {
		return fmt.Errorf("accepted reference repair cannot change the envelope projection")
	}
	// A replay can prove the selected leaves were evidence selectors, without
	// consulting today's schemas or allowing a plan to mask semantic fields.
	for _, pointer := range p.Pointers {
		matched := false
		concrete := fixedCollectionPointerTokens(pointer)
		selectors := compiled.evidenceKeys.Pointers
		if slices.Contains(p.PathPointers, pointer) {
			if compiled.worksetPaths == nil {
				return fmt.Errorf("accepted path repair has no declared workset path selectors")
			}
			selectors = compiled.worksetPaths.Pointers
		}
		for _, selector := range selectors {
			pattern := fixedCollectionPointerTokens(selector)
			if len(pattern) != len(concrete) {
				continue
			}
			matches := true
			for i, token := range pattern {
				if token != "*" && token != concrete[i] {
					matches = false
				}
			}
			matched = matched || matches
		}
		if !matched {
			return fmt.Errorf("accepted-result repair pointer is outside declared reference selectors")
		}
	}
	digest, err := collectionDigest(item.TypedResult)
	if err != nil || digest != p.ResultSHA256 {
		return fmt.Errorf("accepted-result repair source changed")
	}
	digest, err = acceptedResultSemanticHashWithPaths(item.TypedResult, p.Pointers, p.PathPointers)
	if err != nil || digest != p.SemanticSHA256 {
		return fmt.Errorf("accepted-result repair claims changed")
	}
	return nil
}

// Keep the complete shape, array order and all non-reference claims. Runtime
// identity/attestation is rebuilt by ordinary admission and independently bound.
func acceptedResultSemanticHash(result *TaskResult, pointers []string) (string, error) {
	return acceptedResultSemanticHashWithPaths(result, pointers, nil)
}

func acceptedResultSemanticHashWithPaths(result *TaskResult, pointers, pathPointers []string) (string, error) {
	if result == nil || result.StructuredPayload == nil {
		return "", fmt.Errorf("accepted-result repair requires a structured payload")
	}
	raw, err := json.Marshal(result, collectionDurationFormat)
	if err != nil {
		return "", err
	}
	value, err := decodeResultContractJSON(raw)
	if err != nil {
		return "", err
	}
	object := value.(map[string]any)
	for _, key := range []string{"task_id", "attempt", "agent", "source", "files_read", "run_input_snapshot_id", "run_input_snapshot_hash", "materialized_action_payload_hash", "bound_inputs", "runtime_outputs", "runtime_outputs_hash", "runtime_media", "knowledge_coverage", "raw_output_ref", "outputs"} {
		delete(object, key)
	}
	// Transcript evidence is signed with a process-local runtime secret. Keep
	// its identity/value and every model claim, never the old process's MAC.
	if evidence, ok := object["evidence"].([]any); ok {
		for _, raw := range evidence {
			if entry, ok := raw.(map[string]any); ok {
				delete(entry, "system_hmac")
			}
		}
	}
	if result.InvariantVerification != nil {
		object["invariant_verification"] = result.InvariantVerification.Assessments
	}
	if result.Confidence == 0 {
		object["confidence"] = float64(1)
	}
	payload, err := decodeResultContractJSON(result.StructuredPayload.Value)
	if err != nil {
		return "", err
	}
	for _, pointer := range pointers {
		if slices.Contains(pathPointers, pointer) {
			value, err := resolveJSONPointer(payload, pointer)
			if err != nil {
				return "", err
			}
			text, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("accepted path reference must remain a string")
			}
			_, line, err := splitWorksetPathLine(text)
			if err != nil {
				return "", err
			}
			if err := replaceAcceptedReference(payload, fixedCollectionPointerTokens(pointer), ":"+line); err != nil {
				return "", err
			}
			continue
		}
		if err := maskAcceptedEvidence(payload, fixedCollectionPointerTokens(pointer)); err != nil {
			return "", err
		}
	}
	object["structured_payload"] = payload
	return collectionDigest(object)
}

func maskAcceptedEvidence(value any, tokens []string) error {
	if len(tokens) == 0 {
		return fmt.Errorf("invalid accepted evidence pointer")
	}
	var child any
	key := tokens[0]
	switch object := value.(type) {
	case map[string]any:
		var exists bool
		child, exists = object[key]
		if !exists {
			return fmt.Errorf("accepted evidence pointer is absent")
		}
		if len(tokens) == 1 {
			if _, ok := child.(string); !ok {
				return fmt.Errorf("accepted evidence must remain a string")
			}
			object[key] = ""
			return nil
		}
	case []any:
		i, err := strconv.Atoi(key)
		if err != nil || i < 0 || i >= len(object) {
			return fmt.Errorf("accepted evidence array changed")
		}
		child = object[i]
		if len(tokens) == 1 {
			if _, ok := child.(string); !ok {
				return fmt.Errorf("accepted evidence must remain a string")
			}
			object[i] = ""
			return nil
		}
	default:
		return fmt.Errorf("accepted evidence structure changed")
	}
	return maskAcceptedEvidence(child, tokens[1:])
}

func invalidAcceptedEvidence(value any, tokens []string, pointer string, observed map[string]ArtifactRef, invalid *[]string) error {
	if len(tokens) == 0 {
		id, ok := value.(string)
		if !ok {
			return fmt.Errorf("accepted evidence is not a string")
		}
		if observed[id].ID != id || id == "" {
			*invalid = append(*invalid, pointer)
		}
		return nil
	}
	if tokens[0] == "*" {
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("accepted evidence selector requires an array")
		}
		for i, item := range items {
			if err := invalidAcceptedEvidence(item, tokens[1:], pointer+"/"+strconv.Itoa(i), observed, invalid); err != nil {
				return err
			}
		}
		return nil
	}
	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("accepted evidence selector requires an object")
	}
	token := strings.ReplaceAll(strings.ReplaceAll(tokens[0], "~", "~0"), "/", "~1")
	return invalidAcceptedEvidence(object[tokens[0]], tokens[1:], pointer+"/"+token, observed, invalid)
}

func (c *Coordinator) assessAcceptedResultRepair(ctx context.Context, item *TodoItem, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract) (*AcceptedResultRepair, error) {
	if !runtimeOwnsFilesRead(compiled) || len(compiled.evidenceKeys.Pointers) == 0 {
		return nil, fmt.Errorf("accepted-result repair requires runtime-owned observed evidence")
	}
	original, err := c.readAcceptedRepairResult(ctx, cp, "")
	if err != nil {
		return nil, err
	}
	if !sameCanonicalEvidence(original, item.TypedResult) {
		return nil, fmt.Errorf("accepted-result bytes differ from the canonical source")
	}
	ctx = withSubmitResultRuntimeIdentity(ctx, submitResultRuntimeIdentity{RunID: cp.SourceRunID, TaskID: cp.TaskID, Attempt: cp.Attempt, Agent: cp.Agent, OccurrenceRevision: cp.SubmissionRevision, DispatchID: cp.SubmissionDispatchID})
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: cp.SourceRunID, TaskID: cp.TaskID, Attempt: cp.Attempt, AgentName: cp.Agent, AgentRole: "worker", ModelExecutionID: cp.ModelExecutionID})
	ctx = c.withCollectedWorkArtifactScope(ctx, work)
	// Verify the original observation namespace before rebinding the schema.
	originalSchema := *compiled
	originalSchema.SchemaSHA256 = item.ResultContract.SchemaSHA256
	refs, err := c.verifiedResultEvidenceKeyRefs(ctx, cp.TaskID, &originalSchema)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("accepted-result repair has no observed replacement sources")
	}
	observed := make(map[string]ArtifactRef)
	for _, ref := range refs {
		observed[ref.ID] = ref
	}
	value, err := decodeResultContractJSON(original.StructuredPayload.Value)
	if err != nil {
		return nil, err
	}
	var pointers []string
	for _, pointer := range compiled.evidenceKeys.Pointers {
		if err := invalidAcceptedEvidence(value, fixedCollectionPointerTokens(pointer), "", observed, &pointers); err != nil {
			return nil, err
		}
	}
	var pathPointers []string
	if compiled.worksetPaths != nil {
		paths, err := c.verifiedWorksetPathSelection(ctx, cp.TaskID, compiled.worksetPaths)
		if err != nil {
			return nil, err
		}
		for _, selector := range compiled.worksetPaths.Pointers {
			if err := invalidAcceptedWorksetPaths(value, fixedCollectionPointerTokens(selector), paths, &pathPointers); err != nil {
				return nil, err
			}
		}
		pointers = append(pointers, pathPointers...)
		slices.Sort(pathPointers)
		pathPointers = slices.Compact(pathPointers)
	}
	slices.Sort(pointers)
	pointers = slices.Compact(pointers)
	if len(pointers) == 0 {
		return nil, fmt.Errorf("accepted result has no invalid declared references")
	}
	resultHash, err := collectionDigest(original)
	if err != nil {
		return nil, err
	}
	semanticHash, err := acceptedResultSemanticHashWithPaths(original, pointers, pathPointers)
	if err != nil {
		return nil, err
	}
	return &AcceptedResultRepair{Schema: slices.Clone(compiled.CanonicalSchema), ResultSHA256: resultHash, SemanticSHA256: semanticHash, Pointers: pointers, PathPointers: pathPointers}, nil
}

func (c *Coordinator) readAcceptedRepairResult(ctx context.Context, original *ResultCollectionCheckpoint, expected string) (*TaskResult, error) {
	if original == nil || original.State != ResultCollectionCommitted || original.Accepted == nil {
		return nil, errResultCollectionStale
	}
	raw, err := c.readCollectionBlob(ctx, original, "accepted", original.Accepted)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	if err := json.Unmarshal(raw, &result, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return nil, err
	}
	if expected != "" {
		digest, err := collectionDigest(&result)
		if err != nil || digest != expected {
			return nil, fmt.Errorf("accepted-result repair baseline changed")
		}
	}
	return &result, nil
}

func acceptedRepairProof(cp *ResultCollectionCheckpoint) *AcceptedResultRepair {
	if cp != nil && cp.ContractUpgrade != nil {
		return cp.ContractUpgrade.AcceptedResult
	}
	return nil
}

func (c *Coordinator) validateAcceptedRepairClaims(ctx context.Context, cp *ResultCollectionCheckpoint, result *TaskResult) error {
	p := acceptedRepairProof(cp)
	if p == nil {
		return nil
	}
	if _, err := c.readAcceptedRepairResult(ctx, cp.ContractUpgrade.Original, p.ResultSHA256); err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	// Compare the complete runtime projection for verbatim work, without
	// materializing artifacts or asking the model to copy transcript evidence.
	item := c.todoItemByID(cp.TaskID)
	if result != nil && result.RawOutputRef == nil && item != nil && taskUsesVerbatimTranscript(taskDefFromTodoItem(item)) {
		result = cloneTaskResult(result)
		result.TaskID, result.Attempt, result.Agent = cp.TaskID, cp.Attempt, cp.Agent
		if err := c.attachCollectedTranscript(ctx, item, cp, result); err != nil {
			return rejectLocalResult(RepairFailureSource, err)
		}
	}
	digest, err := acceptedResultSemanticHashWithPaths(result, p.Pointers, p.PathPointers)
	if err != nil || digest != p.SemanticSHA256 {
		return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("accepted-result repair may change only the listed invalid evidence references; preserve all other claims"))
	}
	return nil
}

// Kept separate from restoreCollectionContext: previews must not open a lease.
func acceptedRepairPrompt(p *AcceptedResultRepair) string {
	raw, _ := json.Marshal(p.Pointers)
	return "\nAccepted-result reference repair: accepted_response is the immutable original accepted submission; previous_response contains a rejected repair candidate when present. Change only the invalid evidence leaves listed below to the intended observed source. Preserve every other claim, array item/order, finding, severity, summary and judgment exactly. Omit runtime-owned fields as required by claims_schema. No work may run. Allowed payload pointers (data only):\n" + fenceUntrusted("json", string(raw)) + "\n"
}
