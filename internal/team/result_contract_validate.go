package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	// resultPayloadMaxBytes bounds a complete canonical structured payload,
	// including accepted records copied by runtime evidence binding. Aggregate
	// reports need room for those records as well as the worker's decisions.
	resultPayloadMaxBytes = 2 << 20
	// resultPayloadMaxErrors and resultPayloadErrorMaxRunes bound the
	// validation feedback returned to a worker.
	resultPayloadMaxErrors     = 20
	resultPayloadErrorMaxRunes = 200
	// resultPayloadContextMaxBytes bounds the payload shown to the
	// coordinator preview; downstream required evidence uses the full value.
	resultPayloadContextMaxBytes = 16 << 10
	// resultContractPromptSchemaMaxBytes bounds the schema copied into a
	// worker prompt.
	resultContractPromptSchemaMaxBytes = 32 << 10

	resultContractDriftCode      = "result_contract_drift"
	structuredPayloadInvalidCode = "structured_payload_invalid"
	structuredPayloadMissingCode = "structured_payload_missing"
)

// ResultPayload is a validated structured payload. The worker supplies only
// the value; the runtime owns the contract identity and the hash.
type ResultPayload struct {
	Contract ResultContractRef `json:"contract"`
	// Value is the canonical JSON encoding of the payload.
	Value json.RawMessage `json:"value"`
	// SHA256 is the hex SHA-256 of Value.
	SHA256             string `json:"sha256"`
	EvidenceDowngrades int    `json:"evidence_downgrades,omitzero"`
	// CollectionBindings are issued by the runtime after assembly, never
	// accepted from the worker's structured_payload.
	CollectionBindings []ResultCollectionBinding `json:"collection_bindings,omitempty"`
}

// Keep the canonical validator's cause without exposing its unbounded error
// text. Portable provider schemas are guidance, not failure classifiers.
type resultPayloadSchemaError struct {
	contract string
	cause    error
}

func (e *resultPayloadSchemaError) Error() string {
	return fmt.Sprintf("%s: structured_payload does not satisfy result contract %q:\n%s", structuredPayloadInvalidCode, e.contract, formatResultPayloadValidationError(e.cause))
}

func (e *resultPayloadSchemaError) Unwrap() error { return e.cause }

func (p *ResultPayload) clone() *ResultPayload {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Value = append(json.RawMessage(nil), p.Value...)
	clone.CollectionBindings = cloneResultCollectionBindings(p.CollectionBindings)
	return &clone
}

// ResultValidationState records, on an attempt receipt, whether the result
// satisfied its contract.
type ResultValidationState string

const (
	ResultValidationValid       ResultValidationState = "valid"
	ResultValidationInvalid     ResultValidationState = "invalid"
	ResultValidationAbsent      ResultValidationState = "absent"
	ResultValidationNotRequired ResultValidationState = "not_required"
)

// validateStructuredResultPayload is the single trust boundary for
// structured payloads: local submit_result, result-only repair, and external
// provider proposals all go through it.
func validateStructuredResultPayload(compiled *CompiledResultContract, ref ResultContractRef, raw []byte, observations ...[]taskTranscriptRecord) (*ResultPayload, error) {
	if compiled == nil || compiled.schema == nil {
		return nil, fmt.Errorf("%s: result contract %q is not loaded", resultContractDriftCode, ref.ID)
	}
	if len(raw) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("%s: structured_payload is %d bytes, above the %d byte limit", structuredPayloadInvalidCode, len(raw), resultPayloadMaxBytes)
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: structured_payload is not a single JSON value: %w", structuredPayloadInvalidCode, err)
	}
	// Only normalize fields/elements. A null payload itself means absence at
	// the submission boundary and must not satisfy a required payload via a
	// serialized spelling that bypasses that presence check.
	switch value.(type) {
	case map[string]any, []any:
		value = canonicalResultScalars(value, compiled.schema, 0)
	}
	if err := compiled.schema.Validate(value); err != nil {
		return nil, &resultPayloadSchemaError{contract: ref.ID, cause: err}
	}
	var records []taskTranscriptRecord
	if len(observations) > 0 {
		records = observations[0]
	}
	downgrades, err := bindResultToolEvidence(value, compiled.toolEvidence, records)
	if err != nil {
		return nil, fmt.Errorf("%s: bind tool evidence: %w", structuredPayloadInvalidCode, err)
	}
	if err := compiled.schema.Validate(value); err != nil {
		return nil, fmt.Errorf("%s: evidence fallback violates schema: %s", structuredPayloadInvalidCode, formatResultPayloadValidationError(err))
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s: canonicalize structured_payload: %w", structuredPayloadInvalidCode, err)
	}
	if len(canonical) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("%s: evidence-bound payload exceeds the byte limit", structuredPayloadInvalidCode)
	}
	if !redactionStableJSON(canonical) {
		return nil, fmt.Errorf("%s: structured_payload contains secret-like content that durable event redaction would rewrite; remove credentials, tokens, and keys from the payload", structuredPayloadInvalidCode)
	}
	sum := sha256.Sum256(canonical)
	return &ResultPayload{Contract: ref, Value: canonical, SHA256: hex.EncodeToString(sum[:]), EvidenceDowngrades: downgrades}, nil
}

// redactionStableJSON reports whether event redaction leaves canonical JSON
// unchanged, so the persisted payload still matches its hash.
func redactionStableJSON(canonical []byte) bool {
	redacted, err := utils.RedactJSON(canonical)
	if err != nil {
		return false
	}
	value, err := decodeResultContractJSON(redacted)
	if err != nil {
		return false
	}
	recanonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(recanonical, canonical)
}

// formatResultPayloadValidationError turns a schema validation error into a
// bounded list of "location: message" lines the worker can act on.
func formatResultPayloadValidationError(err error) string {
	validation, ok := errors.AsType[*jsonschema.ValidationError](err)
	if !ok {
		return "- " + utils.TruncateRunes(err.Error(), resultPayloadErrorMaxRunes)
	}
	lines := make([]string, 0, resultPayloadMaxErrors)
	total := 0
	seen := make(map[string]bool)
	add := func(location, message string) {
		line := location + ": " + message
		if seen[line] {
			return
		}
		seen[line] = true
		total++
		if len(lines) == resultPayloadMaxErrors {
			return
		}
		if location == "" {
			location = "/"
		}
		lines = append(lines, "- "+utils.TruncateRunes(location+": "+message, resultPayloadErrorMaxRunes))
	}
	// BasicOutput's flattened reference/group wrappers can hide leaf kinds
	// behind "validation failed". Walk Causes directly and budget actionable
	// leaves, not wrappers, so $ref/allOf errors cannot starve missing fields.
	var visit func(*jsonschema.ValidationError)
	visit = func(node *jsonschema.ValidationError) {
		if len(node.Causes) > 0 {
			for _, cause := range node.Causes {
				visit(cause)
			}
			return
		}
		location := ""
		for _, token := range node.InstanceLocation {
			location += "/" + escapeJSONPointerToken(token)
		}
		if required, ok := node.ErrorKind.(*kind.Required); ok {
			for _, property := range required.Missing {
				add(location+"/"+escapeJSONPointerToken(property), "missing required property")
			}
			return
		}
		if output := node.BasicOutput(); output.Error != nil {
			add(location, output.Error.String())
		}
	}
	visit(validation)
	if len(lines) == 0 {
		return "- " + utils.TruncateRunes(validation.Error(), resultPayloadErrorMaxRunes)
	}
	if total > len(lines) {
		lines = append(lines, fmt.Sprintf("- ... and %d more", total-len(lines)))
	}
	return strings.Join(lines, "\n")
}

// compiledResultContract returns the loaded schema for an admitted
// reference. A missing schema or a hash mismatch means the team changed
// under an admitted occurrence, which fails closed.
func (c *Coordinator) compiledResultContract(ref *ResultContractRef) (*CompiledResultContract, error) {
	if ref == nil {
		return nil, nil
	}
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("%s: result contract %q has no loaded team", resultContractDriftCode, ref.ID)
	}
	compiled := c.session.ResultContracts[ref.ID]
	if compiled == nil || compiled.SchemaSHA256 != ref.SchemaSHA256 {
		return nil, fmt.Errorf("%s: result contract %q changed after this task was admitted; start a new session to use the new schema", resultContractDriftCode, ref.ID)
	}
	return compiled, nil
}

// boundResultContract returns the contract admitted for a Todo, if any.
func (c *Coordinator) boundResultContract(todoID string) (*ResultContractRef, *CompiledResultContract, error) {
	if c == nil {
		return nil, nil, nil
	}
	item := c.todoItemByID(todoID)
	if item == nil || item.ResultContract == nil {
		return nil, nil, nil
	}
	compiled, err := c.compiledResultContract(item.ResultContract)
	return item.ResultContract.clone(), compiled, err
}

// A blocked bound-decision envelope claims no completed audit or payload. It
// remains unsuccessful and cannot become evidence for a downstream consumer.
// Drift and any supplied payload retain their ordinary validation paths.
func (c *Coordinator) boundDecisionBlockedEnvelope(todoID string, result *TaskResult) bool {
	if result == nil || result.Status != TaskResultStatusBlocked || result.StructuredPayload != nil {
		return false
	}
	_, compiled, err := c.boundResultContract(todoID)
	return err == nil && hasResultDecisions(compiled)
}

// structuredPayloadForSubmission validates a submit_result payload against
// the Todo's contract. The returned message, when non-empty, is the
// worker-facing rejection.
func (c *Coordinator) structuredPayloadForSubmission(todoID string, raw json.RawMessage, observations ...[]taskTranscriptRecord) (*ResultPayload, string) {
	payload, message, _ := c.structuredPayloadForAdmission(todoID, raw, observations...)
	return payload, message
}

func (c *Coordinator) structuredPayloadForAdmission(todoID string, raw json.RawMessage, observations ...[]taskTranscriptRecord) (*ResultPayload, string, bool) {
	return c.validateStructuredPayloadClaims(todoID, raw, true, observations...)
}

// validateStructuredPayloadClaims shares canonical validation without recording
// rejected submissions during read-only recovery assessment.
func (c *Coordinator) validateStructuredPayloadClaims(todoID string, raw json.RawMessage, recordFailure bool, observations ...[]taskTranscriptRecord) (*ResultPayload, string, bool) {
	present := len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	ref, compiled, err := c.boundResultContract(todoID)
	if err != nil {
		return nil, "submit_result rejected: " + err.Error(), false
	}
	if ref == nil {
		if present {
			return nil, fmt.Sprintf("%s: structured_payload is not accepted for this task because it has no result contract; omit the field", structuredPayloadInvalidCode), false
		}
		return nil, "", false
	}
	if !present {
		if ref.RequireStructured {
			return nil, fmt.Sprintf("%s: structured_payload is required by result contract %q; submit the result again with a structured_payload that satisfies its schema", structuredPayloadMissingCode, ref.ID), false
		}
		return nil, "", false
	}
	plans, err := c.fixedCollectionPlans(todoID)
	if err == nil {
		item := c.todoItemByID(todoID)
		results := c.dependencyResultsForTask(todoID)
		if compiled.evidenceInputs != nil && compiled.evidenceInputs.runtimeOwned() {
			err = c.validateDependencyPayloads(results)
			if err == nil {
				raw, err = assembleRuntimeEvidenceInputs(raw, compiled.evidenceInputs, item.EvidenceFrom, results)
			}
		}
	}
	if err == nil {
		plans, err = c.resolveDecisionCollectionPlans(context.Background(), todoID, plans)
	}
	var bindings []ResultCollectionBinding
	var assembled []byte
	if err == nil {
		assembled, bindings, err = assembleResultFixedCollections(raw, plans)
	}
	if err == nil {
		var values map[string]any
		values, err = c.resultRuntimeInputValues(compiled)
		if err == nil {
			assembled, err = assembleResultRuntimeInputs(assembled, compiled.runtimeInputs, values)
		}
	}
	if err != nil {
		err = fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	// Source checks are independent of model formatting. Run them even when
	// canonical schema validation would fail first, so a format error cannot
	// conceal an invalid receipt, source reference, or settled decision.
	if err == nil {
		if bindingErr := c.validateAssembledEvidenceInputs(todoID, compiled, assembled, bindings); bindingErr != nil {
			err = fmt.Errorf("%s: bind evidence inputs: %w", structuredPayloadInvalidCode, bindingErr)
		}
	}
	repairable := false
	var payload *ResultPayload
	if err == nil {
		payload, err = validateStructuredResultPayload(compiled, *ref, assembled, observations...)
		if err != nil {
			repairable = collectionModelSchemaFailure(compiled, plans, err)
		}
	}
	if err == nil {
		payload.CollectionBindings = bindings
	}
	if err == nil {
		if bindingErr := c.validateSubmittedEvidenceInputs(todoID, payload); bindingErr != nil {
			err = fmt.Errorf("%s: bind evidence inputs: %w", structuredPayloadInvalidCode, bindingErr)
		}
	}
	if err != nil {
		if recordFailure {
			c.recordResultValidationFailure(todoID)
		}
		return nil, err.Error(), repairable
	}
	return payload, "", false
}

func (c *Coordinator) recordResultValidationFailure(todoID string) {
	if c == nil || todoID == "" {
		return
	}
	c.resultValidationMu.Lock()
	defer c.resultValidationMu.Unlock()
	if c.resultValidationFailures == nil {
		c.resultValidationFailures = make(map[string]int)
	}
	c.resultValidationFailures[todoID]++
}

// takeResultValidationFailures returns and clears the rejected payload
// count since the Todo's previous receipt.
func (c *Coordinator) takeResultValidationFailures(todoID string) int {
	if c == nil {
		return 0
	}
	c.resultValidationMu.Lock()
	defer c.resultValidationMu.Unlock()
	count := c.resultValidationFailures[todoID]
	delete(c.resultValidationFailures, todoID)
	return count
}

// applyResultContractReceipt records the attempt's result contract outcome
// on its receipt.
func (c *Coordinator) applyResultContractReceipt(todoID string, result *TaskResult, receipt *ExecutionReceipt) {
	if receipt == nil {
		return
	}
	// Accumulate: a result-only repair re-applies the outcome to the same
	// attempt's receipt after the worker turn already recorded its own.
	receipt.ResultValidationFailures += c.takeResultValidationFailures(todoID)
	var ref *ResultContractRef
	if item := c.todoItemByID(todoID); item != nil {
		ref = item.ResultContract
	}
	if ref == nil {
		return
	}
	receipt.ResultContractID = ref.ID
	receipt.ResultContractSHA256 = ref.SchemaSHA256
	switch {
	case result != nil && result.StructuredPayload != nil:
		receipt.ResultPayloadSHA256 = result.StructuredPayload.SHA256
		receipt.ResultValidation = ResultValidationValid
	case receipt.ResultValidationFailures > 0:
		receipt.ResultValidation = ResultValidationInvalid
	default:
		receipt.ResultValidation = ResultValidationAbsent
	}
}

// providerVisibleResultPayloadSchema is the structured_payload property the
// worker's submit_result tool advertises. Portable schemas are embedded;
// complex schemas retain a portable projection when possible, otherwise the
// property stays open and names the contract. validateStructuredResultPayload is the
// trust boundary, not the provider.
func providerVisibleResultPayloadSchema(ref ResultContractRef, compiled *CompiledResultContract) map[string]any {
	description := fmt.Sprintf("Structured payload that must satisfy result contract %s (sha256 %s).", ref.ID, shortResultContractHash(ref.SchemaSHA256))
	if compiled != nil {
		if value, err := decodeResultContractJSON(compiled.CanonicalSchema); err == nil {
			if schema, ok := value.(map[string]any); ok {
				omitRuntimeEvidenceInputs(schema, compiled.evidenceInputs)
				omitRuntimeResultInputs(schema, compiled.runtimeInputs)
				delete(schema, "$schema")
				if dynamicSchemaEligible(schema) {
					if _, described := schema["description"]; !described {
						schema["description"] = description
					}
					return schema
				}
				if projected := projectProviderResultSchema(schema); projected != nil {
					if _, described := projected["description"]; !described {
						projected["description"] = description
					}
					return projected
				}
				open := map[string]any{"description": description}
				if typeName, ok := schema["type"].(string); ok {
					open["type"] = typeName
				}
				return open
			}
		}
	}
	return map[string]any{"description": description}
}

func shortResultContractHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// resultContractPromptSection tells the worker which contract its result
// must satisfy and how to submit the payload on its protocol.
func resultContractPromptSection(ref *ResultContractRef, compiled *CompiledResultContract, external bool) string {
	if ref == nil {
		return ""
	}
	b := &strings.Builder{}
	b.WriteString("\n\n## Result Contract\n\n")
	fmt.Fprintf(b, "This task's result is bound to result contract `%s` (sha256 %s).", ref.ID, shortResultContractHash(ref.SchemaSHA256))
	field := "`structured_payload` (a JSON value)"
	if external {
		field = "`structured_payload_json` (the JSON value encoded as a string)"
	}
	if ref.RequireStructured {
		fmt.Fprintf(b, " A structured payload is required: the task cannot complete without %s that satisfies the schema below.\n", field)
	} else {
		fmt.Fprintf(b, " Include %s that satisfies the schema below when you can.\n", field)
	}
	b.WriteString("Do not put credentials, authentication tokens, or secret API keys in the payload; a payload containing them is rejected.\n")
	if runtimeOwnsFilesRead(compiled) {
		b.WriteString("The runtime supplies outer files_read from successful native view observations in this work attempt. Omit files_read entirely, even when empty or null. Evidence references at the declared x-hufu-evidence-keys paths must name artifacts actually observed by this task, not merely authorized dependencies. This mode requires the local native runtime.\n")
	} else if compiled != nil && compiled.evidenceKeys != nil && !external {
		b.WriteString("Successful runtime view calls issue short ev_ evidence keys. Use these keys only in files_read.path (or string entries) and the x-hufu-evidence-keys pointers below. The runtime resolves them to full artifact IDs before all canonical validation and hashing. Keys are task/attempt/branch/model-execution scoped; never invent or borrow keys. They attest a successful bounded read, not full artifact coverage or support for a semantic claim.\n")
	}
	if compiled != nil && len(compiled.fixedCollections) > 0 {
		b.WriteString("This schema describes the canonical STORED result. For fields declared in Runtime Fixed Collections, submit keyed decision objects according to that manifest instead of arrays. The runtime supplies the final array and its fixed fields before validating this schema.\n")
	}
	if hasResultDecisions(compiled) && !external {
		b.WriteString("If required bound decisions cannot settle, submit status=blocked with a summary and OMIT structured_payload. Do not fabricate a decision or keep resubmitting an incomplete decision collection. A blocked envelope is not a successful audit; successful results still require the full result contract.\n")
	}
	if compiled != nil {
		schema := compiled.CanonicalSchema
		runtimeOwned := len(compiled.runtimeInputs) > 0
		for _, binding := range compiled.runtimeInputs {
			fmt.Fprintf(b, "The runtime supplies `%s` from receipt-verified native task `%s` output `%s`. Omit this field entirely; never copy or summarize its value in the submission.\n", binding.Target, binding.SourceTask, binding.Output)
		}
		if spec := compiled.evidenceInputs; spec != nil && spec.runtimeOwned() {
			fmt.Fprintf(b, "The runtime supplies `%s` from every accepted evidence_from source. Omit this field entirely; the stored result retains all complete source records and hashes.\n", spec.ItemsPointer)
			runtimeOwned = true
		}
		if runtimeOwned {
			schema, _ = json.Marshal(providerVisibleResultPayloadSchema(*ref, compiled))
		}
		if len(schema) <= resultContractPromptSchemaMaxBytes {
			fmt.Fprintf(b, "\n```json\n%s\n```\n", schema)
		}
	}
	return b.String()
}

// formatForContext renders a validated payload for the coordinator and
// downstream task context, bounded to keep context budgets predictable.
func (p *ResultPayload) formatForContext() string {
	if p == nil {
		return ""
	}
	header := fmt.Sprintf("Structured Payload (contract %s, sha256 %s):\n", p.Contract.ID, p.SHA256)
	if len(p.Value) <= resultPayloadContextMaxBytes {
		return header + string(p.Value) + "\n"
	}
	return header + utils.TruncateString(string(p.Value), resultPayloadContextMaxBytes) +
		fmt.Sprintf("\n[structured payload truncated at %d bytes; the complete value is stored in the durable typed result]\n", resultPayloadContextMaxBytes)
}

// accumulateResultContractMetrics projects rejected structured payloads from
// the run's receipts.
func accumulateResultContractMetrics(metrics *RunMetrics, receipt ExecutionReceipt, runID string) {
	if metrics == nil || (runID != "" && receipt.RunID != runID) {
		return
	}
	metrics.StructuredResultValidationFailures += receipt.ResultValidationFailures
}

// attemptResultContractSchema resolves the loaded schema for an attempt's
// admitted contract. Drift is reported where the payload is validated.
func (c *Coordinator) attemptResultContractSchema(task TaskDef) *CompiledResultContract {
	compiled, _ := c.compiledResultContract(task.ResultContract)
	return compiled
}

// canonicalExternalStructuredPayload applies the local submit_result payload
// rules to an external provider's proposal, through the same validator.
func canonicalExternalStructuredPayload(request AttemptRequest, proposal *WorkerResultProposal, filesRead []FileRef) (*ResultPayload, error) {
	if runtimeOwnsFilesRead(request.resultContractSchema) {
		return nil, fmt.Errorf("runtime-owned files_read requires native observations from the local runtime")
	}
	if hasResultDecisions(request.resultContractSchema) {
		return nil, fmt.Errorf("bound decisions require the local runtime")
	}
	raw := ""
	if proposal != nil && proposal.StructuredPayloadJSON != nil {
		raw = strings.TrimSpace(*proposal.StructuredPayloadJSON)
	}
	ref := request.Task.ResultContract
	if ref == nil {
		if raw != "" {
			return nil, fmt.Errorf("%s: structured_payload_json must be null because the task has no result contract", structuredPayloadInvalidCode)
		}
		return nil, nil
	}
	if raw == "" {
		if ref.RequireStructured {
			return nil, fmt.Errorf("%s: result contract %q requires structured_payload_json", structuredPayloadMissingCode, ref.ID)
		}
		return nil, nil
	}
	if request.resultContractSchema == nil {
		return nil, fmt.Errorf("%s: result contract is not loaded", resultContractDriftCode)
	}
	plans, err := buildResultFixedCollections(request.resultContractSchema.fixedCollections, request.Task.EvidenceFrom, request.resultEvidenceInputs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	assembled, bindings, err := assembleResultFixedCollections([]byte(raw), plans)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	assembled, err = assembleRuntimeEvidenceInputs(assembled, request.resultContractSchema.evidenceInputs, request.Task.EvidenceFrom, request.resultEvidenceInputs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	var runtimeInputs map[string]any
	if len(request.resultContractSchema.runtimeInputs) > 0 {
		if request.resultRuntimeInputs == nil {
			return nil, fmt.Errorf("%s: runtime input lookup is unavailable", structuredPayloadInvalidCode)
		}
		runtimeInputs, err = request.resultRuntimeInputs()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
		}
	}
	assembled, err = assembleResultRuntimeInputs(assembled, request.resultContractSchema.runtimeInputs, runtimeInputs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	payload, err := validateStructuredResultPayload(request.resultContractSchema, *ref, assembled)
	if err != nil {
		return nil, err
	}
	payload.CollectionBindings = bindings
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return nil, err
	}
	if err := validateResultRuntimeInputs(value, request.resultContractSchema.runtimeInputs, runtimeInputs); err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	if err := validateResultWorksetInputs(value, request.resultContractSchema.worksetInputs, request.Task.WorksetBinding); err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	if err := validateResultEvidenceInputs(value, request.resultContractSchema.evidenceInputs, request.Task.EvidenceFrom, request.resultEvidenceInputs); err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	if err := validateResultEvidenceReferences(value, request.resultContractSchema.evidenceReferences, request.Task.EvidenceFrom, request.resultEvidenceInputs, &filesRead); err != nil {
		return nil, fmt.Errorf("%s: %w", structuredPayloadInvalidCode, err)
	}
	return payload, nil
}

// collectionModelSchemaFailure runs only after source assembly and independent
// provenance validation. Canonical JSON Schema errors in model-owned fields
// are repairable even when portability projection omitted their constraints.
// Runtime-owned values and parent-level constraints involving them stay blocked.
func collectionModelSchemaFailure(compiled *CompiledResultContract, plans []resultFixedCollectionPlan, err error) bool {
	failure, ok := errors.AsType[*resultPayloadSchemaError](err)
	if !ok || compiled == nil {
		return false
	}
	validation, ok := errors.AsType[*jsonschema.ValidationError](failure.cause)
	if !ok {
		return false
	}
	var protected []string
	for _, input := range compiled.runtimeInputs {
		protected = append(protected, input.Target)
	}
	if compiled.evidenceInputs.runtimeOwned() {
		protected = append(protected, compiled.evidenceInputs.ItemsPointer)
	}
	for _, plan := range plans {
		fields := plan.spec.fixedFields()
		fields["key"] = true
		for _, group := range plan.spec.DecisionGroups {
			for _, field := range group.Outputs {
				fields[field] = true
			}
		}
		for index := range plan.rows {
			row := plan.spec.ItemsPointer + "/" + strconv.Itoa(index)
			for field := range fields {
				protected = append(protected, row+"/"+escapeJSONPointerToken(field))
			}
		}
	}
	var visit func(*jsonschema.ValidationError) bool
	visit = func(node *jsonschema.ValidationError) bool {
		if len(node.Causes) > 0 {
			for _, cause := range node.Causes {
				if !visit(cause) {
					return false
				}
			}
			return true
		}
		pointer := ""
		for _, token := range node.InstanceLocation {
			pointer += "/" + escapeJSONPointerToken(token)
		}
		for _, owned := range protected {
			if resultPointersOverlap(pointer, owned) {
				return false
			}
		}
		return true
	}
	return visit(validation)
}

// Source checks must see the same scalar normalization as canonical admission
// (for example a schema-declared boolean submitted as "true"). This does not
// perform schema repair or change any runtime-owned binding.
func (c *Coordinator) validateAssembledEvidenceInputs(todoID string, compiled *CompiledResultContract, raw []byte, bindings []ResultCollectionBinding) error {
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return err
	}
	switch value.(type) {
	case map[string]any, []any:
		value = canonicalResultScalars(value, compiled.schema, 0)
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.validateSubmittedEvidenceInputs(todoID, &ResultPayload{Value: normalized, CollectionBindings: bindings})
}
