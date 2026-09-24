package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kjelly/hufu/internal/utils"
)

const (
	// resultPayloadMaxBytes bounds one submitted structured payload.
	resultPayloadMaxBytes = 256 << 10
	// resultPayloadMaxErrors and resultPayloadErrorMaxRunes bound the
	// validation feedback returned to a worker.
	resultPayloadMaxErrors     = 20
	resultPayloadErrorMaxRunes = 200
	// resultPayloadContextMaxBytes bounds the payload shown to the
	// coordinator and downstream task context; the full value stays in the
	// durable typed result.
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
	SHA256 string `json:"sha256"`
}

func (p *ResultPayload) clone() *ResultPayload {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Value = append(json.RawMessage(nil), p.Value...)
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
func validateStructuredResultPayload(compiled *CompiledResultContract, ref ResultContractRef, raw []byte) (*ResultPayload, error) {
	if compiled == nil || compiled.schema == nil {
		return nil, fmt.Errorf("%s: result contract %q is not loaded", resultContractDriftCode, ref.ID)
	}
	if len(raw) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("%s: structured_payload is %d bytes, above the %d byte limit", structuredPayloadInvalidCode, len(raw), resultPayloadMaxBytes)
	}
	value, err := decodeResultContractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: structured_payload is not a single JSON value: %w", structuredPayloadInvalidCode, err)
	}
	if err := compiled.schema.Validate(value); err != nil {
		return nil, fmt.Errorf("%s: structured_payload does not satisfy result contract %q:\n%s", structuredPayloadInvalidCode, ref.ID, formatResultPayloadValidationError(err))
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%s: canonicalize structured_payload: %w", structuredPayloadInvalidCode, err)
	}
	if !redactionStableJSON(canonical) {
		return nil, fmt.Errorf("%s: structured_payload contains secret-like content that durable event redaction would rewrite; remove credentials, tokens, and keys from the payload", structuredPayloadInvalidCode)
	}
	sum := sha256.Sum256(canonical)
	return &ResultPayload{Contract: ref, Value: canonical, SHA256: hex.EncodeToString(sum[:])}, nil
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
	var validation *jsonschema.ValidationError
	if !errors.As(err, &validation) {
		return "- " + utils.TruncateRunes(err.Error(), resultPayloadErrorMaxRunes)
	}
	output := validation.BasicOutput()
	lines := make([]string, 0, resultPayloadMaxErrors)
	total := 0
	for _, unit := range output.Errors {
		if unit.Error == nil {
			continue
		}
		total++
		if len(lines) == resultPayloadMaxErrors {
			continue
		}
		location := unit.InstanceLocation
		if location == "" {
			location = "/"
		}
		lines = append(lines, "- "+utils.TruncateRunes(location+": "+unit.Error.String(), resultPayloadErrorMaxRunes))
	}
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

// structuredPayloadForSubmission validates a submit_result payload against
// the Todo's contract. The returned message, when non-empty, is the
// worker-facing rejection.
func (c *Coordinator) structuredPayloadForSubmission(todoID string, raw json.RawMessage) (*ResultPayload, string) {
	present := len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	ref, compiled, err := c.boundResultContract(todoID)
	if err != nil {
		return nil, "submit_result rejected: " + err.Error()
	}
	if ref == nil {
		if present {
			return nil, fmt.Sprintf("%s: structured_payload is not accepted for this task because it has no result contract; omit the field", structuredPayloadInvalidCode)
		}
		return nil, ""
	}
	if !present {
		if ref.RequireStructured {
			return nil, fmt.Sprintf("%s: structured_payload is required by result contract %q; submit the result again with a structured_payload that satisfies its schema", structuredPayloadMissingCode, ref.ID)
		}
		return nil, ""
	}
	payload, err := validateStructuredResultPayload(compiled, *ref, raw)
	if err != nil {
		c.recordResultValidationFailure(todoID)
		return nil, err.Error()
	}
	return payload, ""
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
// worker's submit_result tool advertises. A schema that only uses keywords
// every provider accepts is embedded; otherwise the property stays open and
// names the contract. Either way validateStructuredResultPayload is the
// trust boundary, not the provider.
func providerVisibleResultPayloadSchema(ref ResultContractRef, compiled *CompiledResultContract) map[string]any {
	description := fmt.Sprintf("Structured payload that must satisfy result contract %s (sha256 %s).", ref.ID, shortResultContractHash(ref.SchemaSHA256))
	if compiled != nil {
		if value, err := decodeResultContractJSON(compiled.CanonicalSchema); err == nil {
			if schema, ok := value.(map[string]any); ok {
				delete(schema, "$schema")
				if dynamicSchemaEligible(schema) {
					if _, described := schema["description"]; !described {
						schema["description"] = description
					}
					return schema
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
	b.WriteString("Do not put credentials, tokens, or keys in the payload; a payload containing them is rejected.\n")
	if compiled != nil && len(compiled.CanonicalSchema) <= resultContractPromptSchemaMaxBytes {
		fmt.Fprintf(b, "\n```json\n%s\n```\n", compiled.CanonicalSchema)
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
func canonicalExternalStructuredPayload(request AttemptRequest, proposal *WorkerResultProposal) (*ResultPayload, error) {
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
	return validateStructuredResultPayload(request.resultContractSchema, *ref, []byte(raw))
}
