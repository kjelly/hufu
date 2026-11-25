package team

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

func (c *Coordinator) validateRecoveryCanonicalClaims(ctx context.Context, cp *ResultCollectionCheckpoint, claims []byte, issue *agent.StructuredResponseIssue, stage *resultNormalization, draft json.RawMessage) error {
	input, err := c.decodeCollectionClaims(cp.TaskID, claims)
	if issue != nil {
		err = rejectLocalResult(RepairFailureInvalidSchema, errors.New(issue.Diagnostic()))
	}
	if err == nil {
		err = c.bindCollectionReceiptFields(ctx, cp, &input)
	}
	if draft != nil {
		input.normalizerDraft = draft
	}
	input.resultNormalization = stage
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, cp)
	if err == nil {
		input, err = c.bindStagedNormalization(ctx, cp.TaskID, input)
	}
	if err == nil {
		input, err = c.projectResultClaims(ctx, cp.TaskID, input)
		if err != nil {
			err = rejectProjectedClaims(err)
		}
	}
	if err == nil {
		admission := localResultAdmission{coordinator: c, todoID: cp.TaskID}
		var res *TaskResult
		res, err = admission.validateClaims(ctx, input, localResultSubmissionContract(c, cp.TaskID), func() RepairFailureReason { return RepairFailureInvalidSchema }, false)
		if err == nil && input.collectionSourcesBound {
			err = c.validateCollectedMemoryInput(ctx, cp, input, res)
		}
		if err == nil {
			err = c.attestInvariantClaims(cp.TaskID, cp.Attempt, cp.ModelExecutionID, input.InvariantAssessments, res)
			if _, mismatch := errors.AsType[*invariantFindingSeverityMismatch](err); mismatch {
				err = rejectLocalResult(RepairFailureInvalidSchema, err)
			}
		}
		if err == nil {
			err = c.validateAcceptedRepairClaims(ctx, cp, res)
		}
		if err == nil {
			contract := localResultSubmissionContract(c, cp.TaskID)
			err = contract.validateFinalizableResult(res)
			if err != nil && collectionProjectionFormatFailure(contract, res) {
				err = rejectLocalResult(RepairFailureInvalidSchema, err)
			}
		}
	}
	return err
}

func (c *Coordinator) assessRecoveryBlobIntegrity(ctx context.Context, cp *ResultCollectionCheckpoint, compiled *CompiledResultContract, check func(string, error) bool) (resultSourceOmissions, bool) {
	var omissions resultSourceOmissions
	for _, blob := range []struct {
		kind string
		ref  *ResultCollectionRef
	}{{"request", cp.Request}, {"response", cp.Response}, {"claims", cp.Prepared}, {"accepted", cp.Accepted}} {
		if blob.ref == nil {
			continue
		}
		record, err := c.readCollectionBlobRecord(ctx, cp, blob.kind, blob.ref)
		if !check(blob.kind+"_integrity", err) {
			return omissions, false
		}
		if (blob.kind == "response" || blob.kind == "claims") && record.ResponseIssue == nil {
			raw, _ := collectionResponseJSON(record.Data)
			omissions = submissionSourceOmissions(compiled, string(raw), false)
		}
	}
	return omissions, true
}

// Normalizer drafts do not use the final schema's representation. Validate
// independent envelope claims first, then revalidate an authenticated output.
func (c *Coordinator) assessNormalizedRecoveryClaims(ctx context.Context, a *ResultCollectionRecoveryAssessment, work *resultCollectionWork, compiled *CompiledResultContract, ref ResultContractRef, blob *resultCollectionBlob, probe, sourceFieldDiagnostic string, check func(string, error) bool) {
	cp := a.Checkpoint
	var envelope map[string]json.RawMessage
	raw, _ := collectionResponseJSON(blob.Data)
	draft, draftErr := submittedNormalizerDraft(raw, false)
	if err := json.Unmarshal([]byte(probe), &envelope); err != nil || envelope == nil || draftErr != nil {
		if !c.validateRecoveryIndependentClaims(ctx, cp, work, compiled, probe, check) {
			return
		}
		shapeErr := errors.New("collector result must be one JSON object")
		if cp.Prepared != nil {
			check("normalization_stage", shapeErr)
		} else {
			check("canonical_claims", shapeErr)
			a.Disposition = RecoveryRepairClaims
		}
		return
	}
	delete(envelope, "structured_payload")
	var forbiddenFilesRead json.RawMessage
	if runtimeOwnsFilesRead(compiled) {
		forbiddenFilesRead = envelope["files_read"]
		delete(envelope, "files_read")
	}
	body, err := json.Marshal(envelope)
	if !check("independent_claims", err) {
		return
	}
	resolved, err := c.resolveResultEvidenceKeys(ctx, cp.TaskID, string(body))
	if !check("evidence_keys", err) || !c.validateRecoveryIndependentClaims(ctx, cp, work, compiled, resolved, check) {
		return
	}
	stage, err := c.readCollectionNormalization(&ref, cp, work.Workspace, draft, blob.Normalization, cp.Prepared != nil)
	if !check("normalization_stage", err) {
		return
	}
	if sourceFieldDiagnostic != "" {
		assessCollectionSourceFieldRecollection(a, sourceFieldDiagnostic, check)
		return
	}
	if blob.ResponseIssue != nil {
		check("canonical_claims", errors.New(blob.ResponseIssue.Diagnostic()))
		a.Disposition = RecoveryRepairClaims
		return
	}
	if blob.ResponseRedacted {
		check("canonical_claims", errors.New("normalization submission contained secret-like data"))
		a.Disposition = RecoveryRepairClaims
		return
	}
	if err := json.Unmarshal([]byte(resolved), &envelope); !check("independent_claims", err) {
		return
	}
	if hasNormalizerDraft(draft) {
		if err := validatedNormalizerDraft(draft, compiled); err != nil {
			check("canonical_claims", err)
			a.Disposition = RecoveryRepairClaims
			return
		}
	}
	if draft != nil {
		envelope["structured_payload"] = draft
	}
	if forbiddenFilesRead != nil {
		envelope["files_read"] = forbiddenFilesRead
	}
	body, err = json.Marshal(envelope)
	if !check("canonical_claims", err) {
		return
	}
	if hasNormalizerDraft(draft) && stage == nil {
		// An unstarted normalization can follow a malformed outer envelope.
		// Reuse live claim decoding before deciding that the only missing
		// evidence is a durable normalization output.
		input, err := c.decodeCollectionClaims(cp.TaskID, body)
		if err == nil {
			err = validateStoppedWorkClaims(work, input, localResultSubmissionContract(c, cp.TaskID).runtimeCompletion)
		}
		if err == nil {
			check("normalization_stage", errors.New("no durable normalizer output is available for result-only recovery"))
		} else {
			assessCollectionClaimsRejection(a, err, check)
		}
		return
	}
	err = c.validateRecoveryCanonicalClaims(ctx, cp, body, blob.ResponseIssue, stage, draft)
	if err == nil {
		a.Disposition = RecoveryCommitLocal
		return
	}
	assessCollectionClaimsRejection(a, err, check)
}

func (c *Coordinator) assessRecoveryClaims(ctx context.Context, item *TodoItem, a *ResultCollectionRecoveryAssessment, work *resultCollectionWork, compiled *CompiledResultContract, check func(string, error) bool) {
	cp, taskID := a.Checkpoint, item.ID
	if a.OriginalReason == RepairFailureAction {
		check("normalization_action", errors.New("the result normalization action failed"))
		return
	}
	if p := acceptedRepairProof(cp); p != nil {
		if cp.ContractUpgrade.SourceOrigin != nil && !check("accepted_origin", c.validateAcceptedCollectionOrigin(ctx, cp)) {
			return
		}
		original, err := c.readAcceptedRepairResult(ctx, cp.ContractUpgrade.Original, p.ResultSHA256)
		if !check("accepted_repair_source", err) {
			return
		}
		if !check("accepted_repair_binding", c.validateAcceptedRepairClaims(ctx, cp, original)) {
			return
		}
		c.assessAcceptedReferenceSelection(ctx, a, work, compiled, check)
		return
	}
	if len(selectedAcceptedReferences(ctx)) > 0 {
		check("reference_selection", fmt.Errorf("reference selection requires an accepted-result repair"))
		return
	}
	ref, kind := cp.Prepared, "claims"
	if ref == nil {
		ref, kind = cp.Response, "response"
	}
	if ref == nil && cp.LegacySubmission != nil {
		if check("legacy_submission", validateLegacyCollection(cp)) {
			a.Disposition = RecoveryResumeCollection
			a.AdditionalTurns = 2
		}
		return
	}
	if ref == nil {
		switch a.OriginalReason {
		case RepairFailureCommit, repairFailureCollectionUnavailable, repairFailureCollectionBudget, repairFailurePreparation, RepairFailureResultMissing:
			a.Disposition = RecoveryResumeCollection
			// A storage continuation can reuse remaining turns, but a lost
			// response at the limit requires the explicit result-only grant's
			// bounded allowance. Consumed reservations are never refunded.
			if a.OriginalReason != RepairFailureCommit || a.RemainingTurns == 0 {
				a.AdditionalTurns = 2
			}
		default:
			check("claims_unavailable", fmt.Errorf("no claims to reclassify failure %q", a.OriginalReason))
		}
		return
	}
	blob, err := c.readCollectionBlobRecord(ctx, cp, kind, ref)
	if !check("claims_integrity", err) {
		return
	}
	if blob.ResponseIssue != nil && !check("response_envelope_sources", c.validateRejectedCollectionResponseSources(ctx, cp, work, compiled, blob, true)) {
		return
	}
	raw := blob.Data
	frame, framed := collectionResponseFrame(raw)
	if blob.ResponseIssue == nil && (len(bytes.TrimSpace(raw)) == 0 || framed && len(frame) == 0) {
		check("evidence_keys", nil)
		if c.validateRecoveryIndependentClaims(ctx, cp, work, compiled, "", check) {
			check("canonical_claims", fmt.Errorf("collector returned no result"))
			a.Disposition = RecoveryRepairClaims
		}
		return
	}
	inspection, _ := collectionResponseJSON(raw)
	probe, sourceFieldDiagnostic, err := c.collectionSourceFieldRepair(ctx, cp, compiled, string(inspection))
	if err != nil {
		check(collectionSourceFieldRepairCheck, err)
		return
	}
	refContract, _, err := c.boundResultContract(taskID)
	if !check("result_contract", err) {
		return
	}
	if refContract != nil && refContract.Normalizer != "" {
		c.assessNormalizedRecoveryClaims(ctx, a, work, compiled, *refContract, blob, probe, sourceFieldDiagnostic, check)
		return
	}
	resolved, err := c.resolveResultEvidenceKeys(ctx, taskID, probe)
	if err != nil {
		c.assessRecoveryEvidenceKeyRejection(ctx, a, work, compiled, probe, resolved, sourceFieldDiagnostic, err, check)
		return
	}
	check("evidence_keys", nil)
	if !c.validateRecoveryIndependentClaims(ctx, cp, work, compiled, resolved, check) {
		return
	}
	if sourceFieldDiagnostic != "" {
		assessCollectionSourceFieldRecollection(a, sourceFieldDiagnostic, check)
		return
	}
	claims := []byte(resolved)
	err = c.validateRecoveryCanonicalClaims(ctx, cp, claims, blob.ResponseIssue, nil, nil)
	if err == nil {
		a.Disposition = RecoveryCommitLocal
	} else {
		// The sealed context and independent claims above already establish
		// source integrity without requiring an active submission lease.
		assessCollectionClaimsRejection(a, err, check)
	}
}

func (c *Coordinator) assessRecoveryEvidenceKeyRejection(ctx context.Context, a *ResultCollectionRecoveryAssessment, work *resultCollectionWork, compiled *CompiledResultContract, probe, resolved, sourceFieldDiagnostic string, err error, check func(string, error) bool) {
	if !isResultEvidenceFormatError(err) {
		check("evidence_keys", err)
		return
	}
	// A key-bearing contract requires a verified JSON claims boundary to
	// authorize cold repair. Diagnose readable sources even in a rejected frame.
	if resolved == "" && compiled != nil && compiled.evidenceKeys != nil &&
		!bytes.HasPrefix(bytes.TrimSpace([]byte(probe)), []byte("{")) && !jsontext.Value(probe).IsValid() {
		if c.validateRecoveryIndependentClaims(ctx, a.Checkpoint, work, compiled, probe, check) {
			check("evidence_keys", err)
		}
		return
	}
	check("evidence_keys", nil)
	if resolved == "" {
		resolved = probe
	}
	if !c.validateRecoveryIndependentClaims(ctx, a.Checkpoint, work, compiled, resolved, check) {
		return
	}
	if sourceFieldDiagnostic != "" {
		assessCollectionSourceFieldRecollection(a, sourceFieldDiagnostic, check)
		return
	}
	check("canonical_claims", err)
	a.Disposition = RecoveryRepairClaims
}

func assessCollectionClaimsRejection(a *ResultCollectionRecoveryAssessment, err error, check func(string, error) bool) {
	check("canonical_claims", err)
	reason := localResultRejectionReason(err)
	if isResultReferenceChoiceError(err) || reason == RepairFailureInvalidSchema || reason == RepairFailureResultMissing {
		a.Disposition = RecoveryRepairClaims
	}
}
