package team

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Result projections keep semantic claims in the payload exactly once. The
// envelope is a deterministic runtime projection, not a second model answer.
type resultClaimsProjection struct {
	Findings          string `json:"findings,omitempty"`
	Invariants        string `json:"invariant_assessments,omitempty"`
	InvariantSeverity bool   `json:"bind_invariant_severity,omitzero"`
}

func compileResultClaimsProjection(root map[string]any) (*resultClaimsProjection, error) {
	raw, ok := root["x-hufu-result-projections"]
	if !ok {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var spec resultClaimsProjection
	if err = json.Unmarshal(b, &spec, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	if spec.Findings == "" && spec.Invariants == "" {
		return nil, fmt.Errorf("result projections require findings or invariant_assessments")
	}
	if spec.InvariantSeverity && (spec.Findings == "" || spec.Invariants == "") {
		return nil, fmt.Errorf("invariant severity binding requires both projections")
	}
	shape := projectProviderResultSchema(root)
	if shape == nil {
		return nil, fmt.Errorf("result projections require a portable object shape")
	}
	for _, p := range []string{spec.Findings, spec.Invariants} {
		if p == "" {
			continue
		}
		if len(p) > maxTaskResultPointerBytes {
			return nil, fmt.Errorf("projection pointer exceeds limit")
		}
		if err = validateJSONPointer(p); err != nil {
			return nil, err
		}
		node := resultProjectionSchemaAt(root, p)
		if collectionFeedbackSchemaAt(shape, p) == nil || node == nil || node["type"] != "array" {
			return nil, fmt.Errorf("projection %s must select an array", p)
		}
		limit, limitErr := strconv.Atoi(fmt.Sprint(node["maxItems"]))
		if item := resultProjectionSchemaAt(root, p+"/0"); item != nil && item["type"] != "object" {
			return nil, fmt.Errorf("projection %s items must be objects", p)
		}
		maxItems := maxInvariantAssessmentClaims
		if limitErr != nil || limit < 0 || limit > maxItems {
			return nil, fmt.Errorf("projection %s requires maxItems between 0 and %d", p, maxItems)
		}
	}
	if spec.Findings != "" && spec.Invariants != "" && resultPointersOverlap(spec.Findings, spec.Invariants) {
		return nil, fmt.Errorf("result projection sources overlap")
	}
	return &spec, nil
}

func projectClaimsInput(input SubmitResultInput, spec *resultClaimsProjection) (SubmitResultInput, error) {
	if spec == nil || input.projectionBound {
		return input, nil
	}
	if spec.Findings != "" && input.Findings != nil || spec.Invariants != "" && input.InvariantAssessments != nil {
		return input, fmt.Errorf("projected envelope fields are runtime-owned; submit claims only in structured_payload")
	}
	value, err := decodeUniqueJSON(input.StructuredPayload)
	if err != nil {
		return input, err
	}
	if spec.Findings != "" {
		raw, e := resolveJSONPointer(value, spec.Findings)
		if e != nil {
			return input, e
		}
		rows, ok := raw.([]any)
		if !ok {
			return input, fmt.Errorf("projected findings must be an array")
		}
		input.Findings = make([]Finding, 0, len(rows))
		for _, row := range rows {
			source, ok := row.(map[string]any)
			if !ok {
				return input, fmt.Errorf("projected finding must be an object")
			}
			fields := map[string]any{}
			for _, key := range []string{"summary", "detail", "severity", "category"} {
				if v, ok := source[key]; ok {
					fields[key] = v
				}
			}
			b, e := json.Marshal(fields)
			if e != nil {
				return input, e
			}
			var f Finding
			if e = json.Unmarshal(b, &f, json.RejectUnknownMembers(true)); e != nil {
				return input, e
			}
			input.Findings = append(input.Findings, f)
		}
	}
	if spec.Invariants != "" {
		raw, e := resolveJSONPointer(value, spec.Invariants)
		if e != nil {
			return input, e
		}
		b, e := json.Marshal(raw)
		if e != nil {
			return input, e
		}
		var claims []InvariantAssessmentClaim
		if e = json.Unmarshal(b, &claims, json.RejectUnknownMembers(true)); e != nil {
			return input, e
		}
		if claims == nil {
			return input, fmt.Errorf("projected invariant assessments must be an array")
		}
		input.InvariantAssessments = &claims
	}
	input.projectionBound = true
	return input, nil
}

func (c *Coordinator) projectResultClaims(ctx context.Context, id string, input SubmitResultInput) (SubmitResultInput, error) {
	if c == nil {
		return input, nil
	}
	_, compiled, err := c.boundResultContract(id)
	if err != nil {
		return input, err
	}
	if compiled == nil || compiled.claimsProjection == nil || input.projectionBound {
		return input, nil
	}
	spec := compiled.claimsProjection
	// Bind severities before canonical payload validation and hashing. Definitions
	// come from this original work occurrence's verified context manifest.
	if spec.InvariantSeverity {
		probe, err := projectClaimsInput(input, spec)
		if err != nil {
			return input, err
		}
		claims, err := normalizedInvariantAssessmentClaims(probe.InvariantAssessments)
		if err != nil {
			return input, err
		}
		item := c.todoItemByID(id)
		if item == nil {
			return input, fmt.Errorf("invariant projection needs its work occurrence")
		}
		identity, identityErr := submitResultRuntimeIdentityFromContext(ctx, c, id)
		attempt := identity.Attempt
		if identityErr != nil {
			if item.ExecutionReceipt == nil {
				return input, rejectLocalResult(RepairFailureSource, identityErr)
			}
			attempt = item.ExecutionReceipt.Attempt
		}
		metadata, _ := invocationMetadataFromContext(ctx)
		executionID := metadata.ModelExecutionID
		if executionID == "" {
			if item.ExecutionReceipt != nil {
				executionID = item.ExecutionReceipt.ModelExecutionID
			}
		}
		manifest, err := c.invariantContextManifest(item, attempt, executionID)
		if err != nil {
			return input, rejectLocalResult(RepairFailureSource, err)
		}
		included, err := c.includedInvariantAssessments(manifest)
		if err != nil {
			return input, rejectLocalResult(RepairFailureSource, err)
		}
		definitions := map[string]string{}
		for _, entry := range included {
			definitions[entry.definition.ID] = string(entry.definition.Severity)
		}
		value, err := decodeUniqueJSON(input.StructuredPayload)
		if err != nil {
			return input, err
		}
		slices.SortFunc(claims, func(a, b InvariantAssessmentClaim) int { return cmp.Compare(a.InvariantID, b.InvariantID) })
		claimsBytes, err := json.Marshal(claims)
		if err != nil {
			return input, err
		}
		canonicalClaims, err := decodeUniqueJSON(claimsBytes)
		if err != nil {
			return input, err
		}
		if err = setFixedCollectionValue(value, spec.Invariants, canonicalClaims); err != nil {
			return input, err
		}
		raw, err := resolveJSONPointer(value, spec.Findings)
		if err != nil {
			return input, err
		}
		rows, ok := raw.([]any)
		if !ok {
			return input, fmt.Errorf("projected findings must be an array")
		}
		assigned := map[int]string{}
		for _, claim := range claims {
			severity, exists := definitions[claim.InvariantID]
			if !exists {
				return input, rejectLocalResult(RepairFailureContract, fmt.Errorf("invariant %q was not included in the work context", claim.InvariantID))
			}
			if claim.Status != InvariantViolated {
				continue
			}
			i := *claim.FindingIndex
			if i >= len(rows) {
				return input, fmt.Errorf("invariant finding index is out of bounds")
			}
			row, ok := rows[i].(map[string]any)
			if !ok {
				return input, fmt.Errorf("invariant finding must be an object")
			}
			if previous, ok := assigned[i]; ok && previous != severity {
				return input, fmt.Errorf("invariants disagree on finding severity")
			}
			if _, present := row["severity"]; present && assigned[i] == "" {
				return input, &resultProjectionFieldError{Issue: ResultSubmissionIssue{Code: "runtime_owned", Pointer: fmt.Sprintf("/structured_payload%s/%d/severity", spec.Findings, i), Expected: []string{"omit"}}}
			}
			assigned[i] = severity
			row["severity"] = severity
		}
		input.StructuredPayload, err = json.Marshal(value, json.Deterministic(true))
		if err != nil {
			return input, err
		}
	}
	return projectClaimsInput(input, spec)
}

func omitProjectedEnvelope(info map[string]any, required []string, spec *resultClaimsProjection) []string {
	if spec == nil {
		return required
	}
	for name, p := range map[string]string{"findings": spec.Findings, "invariant_assessments": spec.Invariants} {
		if p != "" {
			delete(info, name)
			required = slices.DeleteFunc(required, func(v string) bool { return v == name })
		}
	}
	return required
}

func projectedPayloadSchema(schema map[string]any, spec *resultClaimsProjection) {
	if spec == nil || !spec.InvariantSeverity {
		return
	}
	array := collectionFeedbackSchemaAt(schema, spec.Findings)
	item, _ := array["items"].(map[string]any)
	if item == nil {
		return
	}
	required, _ := item["required"].([]any)
	item["required"] = slices.DeleteFunc(required, func(v any) bool { return v == "severity" })
	item["description"] = "For findings referenced by violated invariant claims, omit severity: runtime binds it from the invariant definition. Other findings require severity."
}

func projectionForContract(compiled *CompiledResultContract) *resultClaimsProjection {
	if compiled == nil {
		return nil
	}
	return compiled.claimsProjection
}

// A typed field diagnostic is representation guidance, never a rewritten claim.
type resultProjectionFieldError struct{ Issue ResultSubmissionIssue }

func (e *resultProjectionFieldError) Error() string {
	return "runtime-owned field " + e.Issue.Pointer + ": omit it; retain the finding and invariant judgment"
}

// Follow local definitions without dropping bounds as the provider shape does.
func resultProjectionSchemaAt(root map[string]any, pointer string) map[string]any {
	node := root
	dereference := func() bool {
		seen := map[string]bool{}
		for i := 0; i < 32; i++ {
			ref, ok := node["$ref"].(string)
			if !ok {
				return node != nil
			}
			if !strings.HasPrefix(ref, "#/") || seen[ref] {
				return false
			}
			seen[ref] = true
			value, err := resolveJSONPointer(root, strings.TrimPrefix(ref, "#"))
			if err != nil {
				return false
			}
			node, _ = value.(map[string]any)
		}
		return false
	}
	for _, token := range fixedCollectionPointerTokens(pointer) {
		if !dereference() {
			return nil
		}
		if node["type"] == "array" {
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 {
				return nil
			}
			node, _ = node["items"].(map[string]any)
		} else {
			properties, _ := node["properties"].(map[string]any)
			node, _ = properties[token].(map[string]any)
		}
	}
	if !dereference() {
		return nil
	}
	return node
}

// Revalidation must also respect the new projection contract without changing
// historical payload bytes or judgments. A divergent envelope needs new work.
func resultProjectionMatchesAccepted(result *TaskResult, compiled *CompiledResultContract) bool {
	if validateResultEnvelope(compiled, result) != nil {
		return false
	}
	spec := projectionForContract(compiled)
	if spec == nil {
		return true
	}
	if result == nil || result.StructuredPayload == nil {
		return false
	}
	projected, err := projectClaimsInput(SubmitResultInput{StructuredPayload: result.StructuredPayload.Value}, spec)
	if err != nil {
		return false
	}
	if spec.Findings != "" && !slices.Equal(result.Findings, projected.Findings) {
		return false
	}
	if spec.Invariants != "" {
		if result.InvariantVerification == nil {
			return false
		}
		claims := make([]InvariantAssessmentClaim, 0, len(result.InvariantVerification.Assessments))
		for _, assessment := range result.InvariantVerification.Assessments {
			claims = append(claims, InvariantAssessmentClaim{InvariantID: assessment.InvariantID, Status: assessment.Status, Summary: assessment.Summary, FindingIndex: assessment.FindingIndex, MissingEvidence: assessment.MissingEvidence})
		}
		return projected.InvariantAssessments != nil && sameCanonicalEvidence(claims, *projected.InvariantAssessments)
	}
	return true
}

func validateResultProjectionModes(session *TeamSession, schemas map[string]*CompiledResultContract, agentRefs map[string]ResultContractRef) error {
	for _, task := range session.ContractTasks {
		ref := task.ResultContract
		if ref == nil {
			if fallback, ok := agentRefs[normalizedName(task.Agent)]; ok {
				ref = &fallback
			}
		}
		if ref == nil {
			continue
		}
		spec := projectionForContract(schemas[ref.ID])
		if spec != nil && spec.Invariants != "" && task.InvariantVerification == "" {
			return fmt.Errorf("contract task %q projects invariant claims but has no invariant-verification mode", task.ID)
		}
	}
	return nil
}

// Projection sources cannot be rewritten by another runtime binding.
func validateResultProjectionTargets(projections *resultClaimsProjection, evidence *resultToolEvidenceSpec, inputs *resultEvidenceInputsSpec, runtimeInputs []resultRuntimeInputSpec, collections []resultFixedCollectionSpec) error {
	if projections != nil {
		for _, pointer := range []string{projections.Findings, projections.Invariants} {
			if pointer == "" {
				continue
			}
			if evidence != nil && resultPointersOverlap(pointer, evidence.GroupsPointer) {
				return fmt.Errorf("projection overlaps tool-evidence rewriting groups")
			}
			for _, binding := range runtimeInputs {
				if resultPointersOverlap(pointer, binding.Target) {
					return fmt.Errorf("projection overlaps runtime input %s", binding.Target)
				}
			}
			if inputs != nil && inputs.runtimeOwned() && resultPointersOverlap(pointer, inputs.ItemsPointer) {
				return fmt.Errorf("projection overlaps runtime evidence input")
			}
			for _, collection := range collections {
				if resultPointersOverlap(pointer, collection.ItemsPointer) {
					return fmt.Errorf("projection overlaps fixed collection")
				}
			}
		}
	}
	return nil
}
