package team

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
)

// canonicalExternalStructuredPayload applies the local submit_result payload
// rules to an external provider's proposal, through the same validator.
func canonicalExternalStructuredPayload(request AttemptRequest, proposal *WorkerResultProposal, filesRead []FileRef) (_ *ResultPayload, rejection error) {
	if runtimeOwnsFilesRead(request.resultContractSchema) {
		return nil, fmt.Errorf("runtime-owned files_read requires native observations from the local runtime")
	}
	if hasResultDecisions(request.resultContractSchema) {
		return nil, fmt.Errorf("bound decisions require the local runtime")
	}
	raw := externalStructuredPayloadJSON(proposal)
	ref := request.Task.ResultContract
	if ref == nil {
		if proposal != nil && proposal.StructuredPayloadJSON != nil {
			return nil, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("%s: structured_payload_json must be null because the task has no result contract", structuredPayloadInvalidCode))
		}
		return nil, nil
	}
	if request.resultContractSchema == nil {
		return nil, fmt.Errorf("%s: result contract is not loaded", resultContractDriftCode)
	}
	if ref.Normalizer != "" {
		return nil, fmt.Errorf("%s: result normalizers require a local worker backend", resultContractUnsupportedCode)
	}
	compiled := request.resultContractSchema
	sources, err := loadExternalResultPayloadSources(request, externalResultSourceContract(compiled, proposal))
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	// Authenticate independently readable references before granting repair,
	// including when an adjacent envelope field is invalid.
	sourceJSON := raw
	projectFixedRows := true
	defer func() {
		if sourceJSON != "" && localResultRejectionReason(rejection) == RepairFailureInvalidSchema {
			if err := authenticateExternalResultClaims(request, sourceJSON, filesRead, sources, projectFixedRows); err != nil {
				rejection = err
			}
		}
	}()
	if err := validateExternalProjectedFields(compiled.claimsProjection, proposal); err != nil {
		return nil, err
	}
	unsuccessful := proposal != nil && runtimeCompletionFailure(compiled, proposal.Status)
	omitted := proposal == nil || proposal.StructuredPayloadJSON == nil
	if omitted && (unsuccessful || !ref.RequireStructured) {
		return nil, validateExternalResultEnvelope(compiled, proposal, nil)
	}
	if raw == "" {
		if !omitted {
			return nil, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("%s: structured_payload_json string must contain a JSON value", structuredPayloadInvalidCode))
		}
		return nil, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("%s: result contract %q requires structured_payload_json", structuredPayloadMissingCode, ref.ID))
	}
	if unsuccessful {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("%s: omit structured_payload_json for an unsuccessful runtime-completion result", structuredPayloadInvalidCode))
	}
	value, err := decodeUniqueJSON([]byte(raw))
	if err != nil {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	raw, pathErr := canonicalExternalWorksetPaths(compiled.worksetPaths, sources.pathSelection, value, raw)
	if pathErr != nil {
		if isResultEvidenceFormatError(pathErr) {
			return nil, rejectLocalResult(RepairFailureInvalidSchema, pathErr)
		}
		return nil, rejectResultSourceRead(pathErr)
	}
	raw, err = projectExternalResultPayload(request, proposal, raw)
	if err != nil {
		return nil, err
	}
	assembled, bindings, err := assembleResultFixedCollections([]byte(raw), sources.plans)
	if err != nil {
		return nil, rejectExternalResultBinding(err)
	}
	sourceJSON, projectFixedRows = string(assembled), false
	if compiled.evidenceInputs.runtimeOwned() {
		assembled, err = bindRuntimeEvidenceEntries(assembled, compiled.evidenceInputs, sources.evidence)
		if err != nil {
			return nil, rejectExternalResultBinding(err)
		}
	}
	sourceJSON = string(assembled)
	assembled, err = assembleResultRuntimeInputs(assembled, compiled.runtimeInputs, sources.runtimeInputs)
	if err != nil {
		return nil, rejectExternalResultBinding(err)
	}
	sourceJSON = string(assembled)
	return validateExternalResultPayload(request, proposal, filesRead, *ref, sources, bindings, assembled)
}

func externalStructuredPayloadJSON(proposal *WorkerResultProposal) string {
	if proposal == nil || proposal.StructuredPayloadJSON == nil {
		return ""
	}
	return strings.TrimSpace(*proposal.StructuredPayloadJSON)
}

func externalResultSourceContract(compiled *CompiledResultContract, proposal *WorkerResultProposal) *CompiledResultContract {
	if proposal != nil && proposal.StructuredPayloadJSON == nil && strings.TrimSpace(proposal.Summary) != "" &&
		runtimeCompletionFailure(compiled, proposal.Status) {
		return withoutRuntimeCompletionInput(compiled)
	}
	return compiled
}

func rejectExternalResultShape(request AttemptRequest, proposal *WorkerResultProposal, reads []FileRef, shapeErr error) error {
	raw := externalStructuredPayloadJSON(proposal)
	compiled := request.resultContractSchema
	if compiled == nil {
		if request.Task.ResultContract != nil {
			return rejectLocalResult(RepairFailureContract, fmt.Errorf("%s: result contract is not loaded", resultContractDriftCode))
		}
		return shapeErr
	}
	sources, err := loadExternalResultPayloadSources(request, externalResultSourceContract(compiled, proposal))
	if err != nil {
		return rejectResultSourceRead(err)
	}
	if raw != "" {
		if err := authenticateExternalResultClaims(request, raw, reads, sources, true); err != nil {
			return err
		}
	}
	return shapeErr
}

func projectExternalResultPayload(request AttemptRequest, proposal *WorkerResultProposal, raw string) (string, error) {
	compiled := request.resultContractSchema
	spec := compiled.claimsProjection
	if spec == nil {
		return raw, nil
	}
	input := SubmitResultInput{StructuredPayload: []byte(raw), Findings: proposal.Findings, InvariantAssessments: proposal.InvariantAssessments}
	var err error
	if request.resultProjectClaims != nil {
		input, err = request.resultProjectClaims(input)
	} else if spec.InvariantSeverity {
		return "", rejectResultSourceRead(fmt.Errorf("invariant projections require verified work context"))
	} else {
		input, err = projectClaimsInput(input, compiled)
	}
	if err != nil {
		return "", rejectProjectedClaims(err)
	}
	return string(input.StructuredPayload), nil
}

func validateExternalProjectedFields(spec *resultClaimsProjection, proposal *WorkerResultProposal) error {
	if spec == nil || proposal == nil {
		return nil
	}
	if _, supplied := proposal.suppliedFields["findings"]; spec.Findings != "" && (supplied || proposal.Findings != nil) {
		return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("projected findings is runtime-owned; omit it entirely"))
	}
	if _, supplied := proposal.suppliedFields["invariant_assessments"]; spec.Invariants != "" && (supplied || proposal.InvariantAssessments != nil) {
		return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("projected invariant_assessments is runtime-owned; omit it entirely"))
	}
	return nil
}

func validateExternalResultPayload(request AttemptRequest, proposal *WorkerResultProposal, filesRead []FileRef, ref ResultContractRef, sources externalResultPayloadSources, bindings []ResultCollectionBinding, assembled []byte) (*ResultPayload, error) {
	compiled := request.resultContractSchema
	payload, err := validateStructuredResultPayload(compiled, ref, assembled)
	if err != nil {
		if collectionModelSchemaFailure(compiled, sources.plans, err) {
			return nil, rejectLocalResult(RepairFailureInvalidSchema, err)
		}
		return nil, err
	}
	payload.CollectionBindings = bindings
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return nil, err
	}
	if err := validateResultRuntimeInputs(value, compiled.runtimeInputs, sources.runtimeInputs); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	if err := matchResultWorksetInputs(value, compiled.worksetInputs, sources.worksetInputIDs); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	if err := validateResultWorksetPaths(value, compiled.worksetPaths, sources.pathSelection); err != nil {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := validateResultEvidenceInputs(value, compiled.evidenceInputs, request.Task.EvidenceFrom, request.resultEvidenceInputs); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	if err := validateResultEvidenceReferences(value, compiled.evidenceReferences, request.Task.EvidenceFrom, request.resultEvidenceInputs, &filesRead); err != nil {
		if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](err); shape {
			return nil, rejectLocalResult(RepairFailureInvalidSchema, err)
		}
		return nil, rejectResultSourceRead(err)
	}
	if err := validateExternalResultEnvelope(compiled, proposal, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func validateExternalResultEnvelope(compiled *CompiledResultContract, proposal *WorkerResultProposal, payload *ResultPayload) error {
	result := &TaskResult{StructuredPayload: payload}
	if proposal != nil {
		result.Status, result.Summary = proposal.Status, proposal.Summary
	}
	if err := validateResultEnvelope(compiled, result); err != nil {
		return rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	return nil
}

func rejectExternalResultBinding(err error) error {
	if _, shape := errors.AsType[*resultBindingShapeError](err); shape {
		return rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	return rejectResultSourceRead(err)
}

type externalResultPayloadSources struct {
	plans           []resultFixedCollectionPlan
	evidence        []any
	runtimeInputs   map[string]any
	pathSelection   worksetPathSelection
	worksetInputIDs []string
}

func loadExternalResultPayloadSources(request AttemptRequest, compiled *CompiledResultContract) (sources externalResultPayloadSources, err error) {
	sources.plans, err = buildResultFixedCollections(compiled.fixedCollections, request.Task.EvidenceFrom, request.resultEvidenceInputs)
	if err == nil && compiled.evidenceInputs.runtimeOwned() {
		sources.evidence, err = runtimeBoundEvidenceEntries(compiled.evidenceInputs, request.Task.EvidenceFrom, request.resultEvidenceInputs)
	}
	if err == nil && len(compiled.runtimeInputs) != 0 {
		if request.resultRuntimeInputs == nil {
			return sources, errors.New("runtime input lookup is unavailable")
		}
		sources.runtimeInputs, err = request.resultRuntimeInputs(compiled)
	}
	if err == nil && compiled.worksetPaths != nil {
		if request.resultWorksetPaths == nil {
			return sources, errors.New("workset path lookup is unavailable")
		}
		sources.pathSelection, err = request.resultWorksetPaths()
	}
	if err == nil && compiled.worksetInputs != nil {
		if request.resultWorksetInputIDs == nil {
			return sources, errors.New("workset input lookup is unavailable")
		}
		sources.worksetInputIDs, err = request.resultWorksetInputIDs()
	}
	return sources, err
}

func authenticateExternalResultClaims(request AttemptRequest, raw string, reads []FileRef, sources externalResultPayloadSources, projectFixedRows bool) error {
	compiled := request.resultContractSchema
	verifyInvariants := compiled.claimsProjection != nil && compiled.claimsProjection.InvariantSeverity
	if compiled.evidenceReferences == nil && compiled.evidenceInputs == nil && compiled.worksetInputs == nil && len(compiled.runtimeInputs) == 0 && !verifyInvariants {
		return nil
	}
	inspect := func(value any, view []byte) error {
		if verifyInvariants {
			_, projectionErr := projectExternalResultPayload(request, &WorkerResultProposal{}, string(view))
			if projectionErr != nil && localResultRejectionReason(projectionErr) != RepairFailureInvalidSchema {
				return projectionErr
			}
		}
		if len(compiled.runtimeInputs) != 0 {
			if _, object := value.(map[string]any); !object {
				return rejectResultSourceRead(errors.New("structured payload must be an object to verify its runtime input bindings"))
			}
		}
		if projectFixedRows {
			if err := projectFixedCollectionSourceRows(value, sources.plans); err != nil {
				return rejectResultSourceRead(err)
			}
		}
		value = canonicalResultScalars(value, compiled.schema, 0)
		if err := matchResultWorksetInputs(value, compiled.worksetInputs, sources.worksetInputIDs); err != nil {
			return rejectResultSourceRead(err)
		}
		if !compiled.evidenceInputs.runtimeOwned() {
			if err := validateResultEvidenceInputs(value, compiled.evidenceInputs, request.Task.EvidenceFrom, request.resultEvidenceInputs); err != nil {
				return rejectResultSourceRead(err)
			}
		}
		err := validateResultEvidenceReferences(value, compiled.evidenceReferences, request.Task.EvidenceFrom, request.resultEvidenceInputs, &reads)
		if _, shape := errors.AsType[*resultEvidenceReferenceShapeError](err); err == nil || shape {
			return nil
		}
		return rejectResultSourceRead(err)
	}
	inspectView := func(view []byte) error {
		return inspectProviderPayloadObjects(view, 0, inspect)
	}
	found, err := inspectProviderResultJSONFrames(raw, resultPayloadMaxBytes, func(frame string) error {
		return inspectReadablePayloadViews(jsontext.Value(frame), inspectView)
	})
	if err != nil {
		return rejectResultSourceRead(err)
	}
	if !found {
		return inspectReadablePayloadViews(jsontext.Value(raw), inspectView)
	}
	return nil
}
