package team

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/tools"
)

// Inspect each source-bearing envelope field before decoding ordinary claims.
// A malformed field is left to the submission decoder so it cannot hide a
// well-formed but unauthorised source claim in a different field.
type resultReceiptSourceError struct{ error }

func (e *resultReceiptSourceError) Unwrap() error { return e.error }

func (c *Coordinator) validateSubmissionSourceEnvelope(ctx context.Context, taskID, workspace, raw string, cp *ResultCollectionCheckpoint, compiled *CompiledResultContract) error {
	if cp != nil {
		ctx = context.WithValue(ctx, resultCollectionReadCheckpointKey{}, cp)
		raw = collectionSourceInspectionJSON(raw)
	}
	legacyFields := cp == nil
	if legacyFields {
		raw = c.canonicalSubmissionArguments(taskID, raw)
	}
	fields := readableSourceMembers(raw)
	input := SubmitResultInput{Status: receiptSourceStatus(fields, legacyFields)}
	for _, field := range fields {
		if strings.EqualFold(field.name, "files_read") {
			if err := c.validateSubmittedEvidenceKeyReads(ctx, taskID, compiled, field.raw); err != nil {
				return err
			}
		}
	}
	input.ReceiptIDs, input.receiptIDsSupplied = submittedSourceClaims[string](fields, "receipt_ids", legacyFields)
	input.Artifacts, _ = submittedSourceClaims[SubmitArtifactInput](fields, "artifacts", legacyFields)
	var deferredSchemaError error
	if cp != nil {
		deferredSchemaError = c.bindCollectionReceiptFields(ctx, cp, &input)
		if deferredSchemaError != nil && localResultRejectionReason(deferredSchemaError) != RepairFailureInvalidSchema {
			return deferredSchemaError
		}
		if deferredSchemaError != nil {
			input.receiptIDsSupplied = false
			if err := c.bindCollectionReceiptFields(ctx, cp, &input); err != nil {
				return err
			}
		}
	}
	claims := input.taskResult()
	if cp != nil {
		claims.Attempt = cp.Attempt
	}
	if !input.collectionSourcesBound {
		if err := c.validateTaskResultReceiptClaims(ctx, taskID, &claims); err != nil {
			return &resultReceiptSourceError{err}
		}
	}
	if item := c.todoItemByID(taskID); item != nil && item.Execution.ForbidArtifacts {
		for _, field := range fields {
			if sourceMemberMatches(field.name, "artifacts", legacyFields) {
				err := fmt.Errorf("artifacts are forbidden by this task's execution contract; omit the artifacts field")
				if legacyFields {
					return rejectLocalResult(RepairFailureContract, err)
				}
				// The collector can remove an undeclared field during its bounded
				// schema repair. Still inspect independent source fields below.
				if deferredSchemaError == nil {
					deferredSchemaError = rejectLocalResult(RepairFailureInvalidSchema, err)
				}
				break
			}
		}
	} else {
		if len(claims.Artifacts) > 0 {
			if err := c.validateDirectResultArtifactClaims(ctx, workspace, claims.Artifacts); err != nil {
				return err
			}
		}
		if err := validateMalformedArtifactSources(ctx, workspace, fields, legacyFields); err != nil {
			return err
		}
	}
	if err := c.validateReadableAssumptionSources(ctx, taskID, cp, fields, legacyFields); err != nil {
		return err
	}
	if err := c.validateReadableMemorySources(ctx, taskID, cp, input.collectionSourcesBound, fields, legacyFields); err != nil {
		return err
	}
	return deferredSchemaError
}

// Only an unambiguous legal outcome can relax the receipt requirements for success.
func receiptSourceStatus(fields []submissionSourceField, legacyFields bool) string {
	status := TaskResultStatusSuccess
	seen := false
	for _, field := range fields {
		if sourceMemberMatches(field.name, "status", legacyFields) {
			if seen || json.Unmarshal(field.raw, &status) != nil {
				return TaskResultStatusSuccess
			}
			seen = true
		}
	}
	if legacyFields {
		status = strings.ToLower(strings.TrimSpace(status))
	}
	if validWorkerResultProposalStatus(status) {
		return status
	}
	return TaskResultStatusSuccess
}

func (c *Coordinator) canonicalSubmissionArguments(taskID, raw string) string {
	info := (&submitResultTool{coordinator: c, todoID: taskID}).fullInfo()
	raw, _ = tools.CanonicalToolArguments(raw, info)
	return raw
}

func (c *Coordinator) submissionSourceFields(taskID, raw string) map[string]jsontext.Value {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(c.canonicalSubmissionArguments(taskID, raw)), &fields); err != nil {
		return nil
	}
	return fields
}

type submissionSourceField struct {
	name string
	raw  jsontext.Value
}

func readableSourceMembers(raw string) []submissionSourceField {
	// Preserve each member, including repeated ones. A duplicate in the draft
	// cannot hide a separately readable source. Admission still decodes strictly.
	fields, incompleteValue := scanSourceMembers(raw)
	if incompleteValue {
		fields, _ = scanSourceMembers(readableSourcePrefix(raw))
	}
	return fields
}

func scanSourceMembers(raw string) (fields []submissionSourceField, incompleteValue bool) {
	decoder := jsontext.NewDecoder(strings.NewReader(raw), jsontext.AllowDuplicateNames(true))
	first, err := decoder.ReadToken()
	if err != nil || first.Kind() != '{' {
		return nil, false
	}
	for decoder.PeekKind() == '"' {
		field, err := decoder.ReadToken()
		if err != nil {
			break
		}
		name := field.String()
		value, err := decoder.ReadValue()
		if err != nil {
			return fields, true
		}
		fields = append(fields, submissionSourceField{name: name, raw: value.Clone()})
	}
	return fields, false
}

// Make complete tokens in a truncated object or array available to source
// checks. The original bytes still govern schema validation and persistence.
func readableSourcePrefix(raw string) string {
	prefix, _ := readableSourceContainerPrefix(raw)
	return prefix
}

func readableSourceContainerPrefix(raw string) (string, int) {
	decoder := jsontext.NewDecoder(strings.NewReader(raw), jsontext.AllowDuplicateNames(true))
	var lastValueEnd int64
	for {
		if _, err := decoder.ReadToken(); err != nil {
			break
		}
		depth := decoder.StackDepth()
		if depth == 0 {
			return raw[:decoder.InputOffset()], int(decoder.InputOffset())
		}
		kind, count := decoder.StackIndex(depth)
		if kind != '{' || count%2 == 0 {
			lastValueEnd = decoder.InputOffset()
		}
	}
	if lastValueEnd == 0 {
		return "", int(decoder.InputOffset())
	}
	var prefix strings.Builder
	prefix.WriteString(raw[:lastValueEnd])
	for depth := decoder.StackDepth(); depth > 0; depth-- {
		if kind, _ := decoder.StackIndex(depth); kind == '{' {
			prefix.WriteByte('}')
		} else {
			prefix.WriteByte(']')
		}
	}
	return prefix.String(), int(decoder.InputOffset())
}

func submittedSourceClaims[T any](fields []submissionSourceField, name string, legacyFields bool) ([]T, bool) {
	var claims []T
	supplied := false
	options := submissionSourceOptions(legacyFields)
	for _, field := range fields {
		if !sourceMemberMatches(field.name, name, legacyFields) {
			continue
		}
		entries, err := readSourceEntries(field.raw)
		if err != nil {
			continue
		}
		supplied = supplied || len(entries) == 0
		for _, entry := range entries {
			var claim T
			if json.Unmarshal(entry, &claim, options) == nil {
				claims = append(claims, claim)
			}
		}
	}
	return claims, supplied || len(claims) > 0
}

func sourceMemberMatches(actual, field string, legacyFields bool) bool {
	return actual == field || legacyFields && strings.EqualFold(actual, field)
}

func readSourceEntries(raw []byte) ([]jsontext.Value, error) {
	var entries []jsontext.Value
	err := json.Unmarshal(raw, &entries, jsontext.AllowDuplicateNames(true))
	return entries, err
}

func forEachReadableSourceEntry(fields []submissionSourceField, name string, legacyFields bool, inspect func([]submissionSourceField)) (emptyArray bool) {
	for _, field := range fields {
		if !sourceMemberMatches(field.name, name, legacyFields) {
			continue
		}
		entries, err := readSourceEntries(field.raw)
		if err != nil {
			continue
		}
		emptyArray = emptyArray || len(entries) == 0
		for _, entry := range entries {
			inspect(readableSourceMembers(string(entry)))
		}
	}
	return emptyArray
}

func readableSourceStrings(fields []submissionSourceField, name string, legacyFields bool) []string {
	var values []string
	for _, field := range fields {
		var value string
		if sourceMemberMatches(field.name, name, legacyFields) && json.Unmarshal(field.raw, &value) == nil && value != "" {
			values = append(values, value)
		}
	}
	return values
}

func (c *Coordinator) validateReadableAssumptionSources(ctx context.Context, taskID string, cp *ResultCollectionCheckpoint, fields []submissionSourceField, legacyFields bool) error {
	ids, evidence := readableAssumptionSources(fields, legacyFields)
	if len(ids) == 0 && len(evidence) == 0 {
		return nil
	}
	assumptions, governed, err := c.readSubmissionAssumptions(ctx, taskID, cp)
	if err != nil || !governed {
		return err
	}
	for _, id := range ids {
		if !declaredAssumption(assumptions, strings.TrimSpace(id)) {
			return fmt.Errorf("assumption %q is not declared on this decision", id)
		}
	}
	if len(evidence) > 0 {
		return c.validateAssumptionEvidence(ctx, evidence)
	}
	return nil
}

func readableAssumptionSources(fields []submissionSourceField, legacyFields bool) (ids []string, evidence []AssumptionCheck) {
	forEachReadableSourceEntry(fields, "assumption_checks", legacyFields, func(entry []submissionSourceField) {
		entryIDs := readableSourceStrings(entry, "assumption_id", legacyFields)
		ids = append(ids, entryIDs...)
		var check AssumptionCheck
		if len(entryIDs) > 0 {
			check.AssumptionID = entryIDs[0]
		}
		for _, field := range entry {
			if !sourceMemberMatches(field.name, "evidence", legacyFields) {
				continue
			}
			refs, _ := readSourceEntries(field.raw)
			for _, raw := range refs {
				var id string
				if json.Unmarshal(raw, &id) == nil && strings.TrimSpace(id) != "" {
					check.Evidence = append(check.Evidence, id)
				}
			}
		}
		if len(check.Evidence) > 0 {
			evidence = append(evidence, check)
		}
	})
	return ids, evidence
}

func (c *Coordinator) validateReadableMemorySources(ctx context.Context, taskID string, cp *ResultCollectionCheckpoint, bound bool, fields []submissionSourceField, legacyFields bool) error {
	retrievalIDs, itemIDs, empty := readableMemorySourceIDs(fields, legacyFields)
	if len(retrievalIDs) == 0 && len(itemIDs) == 0 {
		if bound && empty {
			return c.validateCollectedMemoryInput(ctx, cp, SubmitResultInput{memoryUsesSupplied: true}, &TaskResult{})
		}
		return nil
	}
	if bound && len(retrievalIDs) > 0 {
		return fmt.Errorf("memory retrieval_id is runtime-owned; omit it")
	}
	if c.session != nil && !memoryLearningEnabled(c.session.Config.MemoryLearning) && !bound {
		return nil // Keep the historical behavior for disabled direct attribution.
	}
	if cp == nil {
		cp, _ = ctx.Value(resultCollectionReadCheckpointKey{}).(*ResultCollectionCheckpoint)
	}
	var manifest *MemoryInjectionManifest
	var err error
	if cp != nil && cp.TaskID == taskID {
		manifest, err = c.collectionMemoryManifest(cp)
		if err == nil && (manifest == nil || len(manifest.Items) == 0 || !memoryLearningEnabled(c.session.Config.MemoryLearning)) {
			return fmt.Errorf("memory_uses requires attributable memory from the original work attempt")
		}
	} else {
		var retrievalID string
		if len(retrievalIDs) > 0 {
			retrievalID = retrievalIDs[0]
		}
		manifest, err = c.directMemoryAttributionManifest(ctx, taskID, 0, retrievalID)
	}
	if err != nil {
		return err
	}
	for _, id := range retrievalIDs {
		if id != manifest.RetrievalID {
			return fmt.Errorf("memory retrieval_id is not valid for task %q attempt %d", taskID, manifest.Attempt)
		}
	}
	allowed := memoryUseManifestItems(manifest)
	for i, id := range itemIDs {
		itemIDs[i] = memoryContextItemID(id)
		if _, ok := allowed[itemIDs[i]]; !ok {
			return fmt.Errorf("memory context item %q was not injected", id)
		}
	}
	if len(itemIDs) == 0 {
		return nil
	}
	return c.validateMemoryUseRecords(ctx, manifest.Agent, itemIDs, allowed)
}

func readableMemorySourceIDs(fields []submissionSourceField, legacyFields bool) (retrievalIDs, itemIDs []string, empty bool) {
	empty = forEachReadableSourceEntry(fields, "memory_uses", legacyFields, func(entry []submissionSourceField) {
		retrievalIDs = append(retrievalIDs, readableSourceStrings(entry, "retrieval_id", legacyFields)...)
		itemIDs = append(itemIDs, readableSourceStrings(entry, "context_item_id", legacyFields)...)
	})
	return retrievalIDs, itemIDs, empty
}

func submissionSourceOptions(legacyFields bool) json.Options {
	options := json.RejectUnknownMembers(true)
	if legacyFields {
		// Match the direct submission decoder's v1 case folding, including
		// names inside typed source claims. Collection claims are strict.
		options = json.JoinOptions(jsonv1.DefaultOptionsV1(), options)
	}
	return options
}

// A bad description, unknown field, or duplicate path leaves readable paths
// subject to the same source policy. The typed decoder still rejects the draft.
func validateMalformedArtifactSources(ctx context.Context, workspace string, fields []submissionSourceField, legacyFields bool) error {
	var paths []ArtifactRef
	for _, field := range fields {
		if !sourceMemberMatches(field.name, "artifacts", legacyFields) {
			continue
		}
		entries, _ := readSourceEntries(field.raw)
		for _, entry := range entries {
			paths = append(paths, malformedArtifactPaths(entry, legacyFields)...)
		}
	}
	return validateSubmittedArtifactClaims(ctx, workspace, paths)
}

func malformedArtifactPaths(entry jsontext.Value, legacyFields bool) []ArtifactRef {
	var paths []ArtifactRef
	for _, field := range readableSourceMembers(string(entry)) {
		var path string
		if sourceMemberMatches(field.name, "path", legacyFields) && json.Unmarshal(field.raw, &path) == nil && strings.TrimSpace(path) != "" {
			paths = append(paths, ArtifactRef{Path: path})
		}
	}
	var claim SubmitArtifactInput
	if len(paths) <= 1 && json.Unmarshal(entry, &claim, submissionSourceOptions(legacyFields)) == nil {
		return nil
	}
	return paths
}

// A valid unsuccessful envelope does not claim that a successful native
// action or a settled decision exists. Other source checks still apply.
type resultSourceOmissions struct {
	runtimeCompletion  bool
	unsettledDecisions bool
}

func submissionSourceOmissions(compiled *CompiledResultContract, raw string, direct bool) resultSourceOmissions {
	if !hasRuntimeCompletion(compiled) && !hasResultDecisions(compiled) {
		return resultSourceOmissions{}
	}
	var envelope struct {
		Status            string         `json:"status"`
		Summary           string         `json:"summary"`
		StructuredPayload jsontext.Value `json:"structured_payload"`
	}
	var err error
	if direct && len(compiled.submissionFields) == 0 {
		if !jsontext.Value(raw).IsValid() {
			return resultSourceOmissions{}
		}
		var normalized []byte
		normalized, err = normalizeSubmitResultInput([]byte(raw))
		if err == nil {
			err = jsonv1.Unmarshal(normalized, &envelope)
		}
	} else {
		err = json.Unmarshal([]byte(raw), &envelope)
	}
	if err != nil || len(bytes.TrimSpace(envelope.StructuredPayload)) != 0 {
		return resultSourceOmissions{}
	}
	result := TaskResult{Status: envelope.Status, Summary: strings.TrimSpace(envelope.Summary)}
	if validateResultSummaryAndStatus(&result) != nil {
		return resultSourceOmissions{}
	}
	return resultSourceOmissions{
		runtimeCompletion:  runtimeCompletionFailure(compiled, result.Status),
		unsettledDecisions: allowsUnsettledDecisions(compiled, result.Status, envelope.StructuredPayload),
	}
}

func allowsUnsettledDecisions(compiled *CompiledResultContract, status string, payload []byte) bool {
	return hasResultDecisions(compiled) && status == TaskResultStatusBlocked && len(bytes.TrimSpace(payload)) == 0
}

// Validate sources whose identity and values are independent of model claims.
func (c *Coordinator) validateIndependentSubmissionSources(ctx context.Context, taskID string, compiled *CompiledResultContract, omissions resultSourceOmissions) error {
	if err := c.validateRequiredArtifactReadSubmission(ctx, taskID); err != nil {
		return err
	}
	return c.validateIndependentResultSources(ctx, taskID, compiled, omissions)
}

// A durable envelope uses recorded read evidence. Its other independent
// bindings must still verify against their sources.
func (c *Coordinator) validateIndependentResultSources(ctx context.Context, taskID string, compiled *CompiledResultContract, omissions resultSourceOmissions) error {
	if compiled == nil {
		return nil
	}
	if compiled.worksetInputs != nil {
		if _, err := c.verifiedResultWorksetInputIDs(ctx, taskID, compiled.worksetInputs); err != nil {
			return err
		}
	}
	if err := c.validateSubmissionRuntimeInputs(ctx, taskID, compiled, omissions); err != nil {
		return err
	}
	if compiled.evidenceInputs.runtimeOwned() {
		if err := c.validateDependencyPayloads(ctx, taskID, c.dependencyResultsForBinding(taskID)); err != nil {
			return err
		}
	}
	plans, err := c.fixedCollectionPlans(taskID)
	if err != nil {
		return err
	}
	if _, err := c.resolveDecisionPlans(ctx, taskID, plans, nil, omissions.unsettledDecisions); err != nil {
		return err
	}
	if compiled.worksetPaths != nil {
		if _, err := c.verifiedWorksetPathSelection(ctx, taskID, compiled.worksetPaths); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) validateSubmissionRuntimeInputs(ctx context.Context, taskID string, compiled *CompiledResultContract, omissions resultSourceOmissions) error {
	if omissions.runtimeCompletion {
		compiled = withoutRuntimeCompletionInput(compiled)
	}
	_, err := c.resultRuntimeInputValuesWithContext(ctx, compiled, taskID)
	return err
}
