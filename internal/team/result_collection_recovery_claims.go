package team

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// Read usable claims for payload inspection and recovery policies. Source
// authentication uses validateSubmissionSourceEnvelope on the original wire.
func independentRecoveryClaims(raw string) (*SubmitResultInput, error) {
	if !jsontext.Value(raw).IsValid() {
		return nil, nil // No independently decodable fields; callers check sealed sources.
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, fmt.Errorf("cannot independently inspect recovery claims: %w", err)
	}
	var sourceFields []submissionSourceField
	for name, value := range fields {
		sourceFields = append(sourceFields, submissionSourceField{name: name, raw: value})
	}
	input := SubmitResultInput{Status: receiptSourceStatus(sourceFields, false), StructuredPayload: fields["structured_payload"]}
	if receipts, supplied := fields["receipt_ids"]; supplied {
		if err := json.Unmarshal(receipts, &input.ReceiptIDs); err != nil {
			return nil, fmt.Errorf("cannot authenticate recovery receipt_ids: %w", err)
		}
	}
	input.MemoryUses, _ = submittedSourceClaims[MemoryUseRef](sourceFields, "memory_uses", false)
	input.AssumptionChecks, _ = submittedSourceClaims[AssumptionCheck](sourceFields, "assumption_checks", false)
	return &input, nil
}

// A preview must not create a process manager or reconcile an owned process.
// Conservatively refuse unsettled or unsuccessful terminal work; the ordinary
// commit still rechecks live terminal evidence before publishing any result.
func validateRecoveryTerminalContext(workspace string, cp *ResultCollectionCheckpoint) error {
	sessions, err := LoadTerminalSessions(workspace)
	if err != nil {
		return err
	}
	var latest *TerminalSession
	for i := range sessions {
		session := &sessions[i]
		if session.RunID != cp.SourceRunID || (session.OwnerTaskID != cp.TaskID && session.ControllerTaskID != cp.TaskID) {
			continue
		}
		if session.Running || session.ExitCode == nil {
			return fmt.Errorf("terminal %s has unsettled process evidence; reconcile it before result-only recovery", session.ID)
		}
		if latest == nil || session.StartedAt.After(latest.StartedAt) {
			latest = session
		}
	}
	if latest != nil && *latest.ExitCode != 0 {
		return fmt.Errorf("latest terminal %s exited with status %d; result-only recovery requires terminal reconciliation", latest.ID, *latest.ExitCode)
	}
	return nil
}

func (c *Coordinator) validateRecoveryIndependentClaims(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract, resolved string, check func(string, error) bool) bool {
	taskID := cp.TaskID
	// Independently inspect source references and receipt claims before any
	// schema-format rejection. Invalid outer JSON is repairable only after the
	// sealed work, dependencies, and decisions above have been verified.
	if err := c.validateSubmissionSourceEnvelope(ctx, taskID, work.Workspace, resolved, cp, compiled); localResultRejectionReason(err) != RepairFailureInvalidSchema {
		if _, receiptFailure := errors.AsType[*resultReceiptSourceError](err); receiptFailure {
			return check("receipt_claims", err)
		}
		if !check("independent_claims", err) {
			return false
		}
	}
	independent, independentErr := independentRecoveryClaims(resolved)
	if independentErr != nil && !check("independent_claims", independentErr) {
		return false
	}
	// Runtime-owned sources can be verified independently of the presence or
	// shape of model-authored payload claims.
	err := c.validateSubmissionRuntimeInputs(ctx, taskID, compiled, submissionSourceOmissions(compiled, resolved, false))
	if !check("runtime_inputs", err) {
		return false
	}
	if compiled != nil && compiled.worksetPaths != nil {
		_, err := c.verifiedWorksetPathSelection(ctx, taskID, compiled.worksetPaths)
		if !check("workset_paths", err) {
			return false
		}
	}
	fields := readableSourceMembers(collectionSourceInspectionJSON(resolved))
	var result TaskResult
	if independent != nil {
		result = independent.taskResult()
	}
	if !check("claims_context", c.validateRecoveryContextClaims(ctx, cp, &result, fields)) {
		return false
	}
	if independent == nil && len(bytes.TrimSpace([]byte(resolved))) != 0 && compiled != nil {
		if len(fields) != 0 && !normalizationContract(c.todoItemByID(taskID)) {
			err := c.validateReadablePayloadSources(ctx, taskID, compiled, resolved, false)
			return localResultRejectionReason(err) == RepairFailureInvalidSchema || check("payload_sources", err)
		}
		if compiled.evidenceReferences != nil || compiled.evidenceInputs != nil || len(compiled.runtimeInputs) > 0 || len(compiled.fixedCollections) > 0 {
			return check("payload_sources", errors.New("malformed JSON prevents independent verification of source references"))
		}
	}
	return independent == nil || c.validateIndependentPayloadClaims(ctx, taskID, compiled, *independent, resolved, true, check)
}

// Inspect payload references independently of outer envelope validation.
func (c *Coordinator) validateIndependentPayloadClaims(ctx context.Context, taskID string, compiled *CompiledResultContract, input SubmitResultInput, resolved string, recovery bool, check func(string, error) bool) bool {
	var records []taskTranscriptRecord
	var err error
	if compiled != nil && compiled.toolEvidence != nil {
		records, err = submissionToolEvidence(ctx, c, taskID)
	}
	if !check("tool_evidence", err) {
		return false
	}
	// Absence makes no payload source claim. After receipts and runtime
	// sources pass, the ordinary decoder may classify it as format repair;
	// the replacement still goes through full submission validation.
	payload := bytes.TrimSpace(input.StructuredPayload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("null")) {
		return true
	}
	if compiled != nil && len(compiled.runtimeInputs) > 0 && jsontext.Value(payload).Kind() != '{' {
		check("payload_sources", errors.New("structured payload must be an object to verify its runtime input bindings"))
		return false
	}
	// Verify projection provenance too: a citation typo must not conceal a
	// foreign invariant or stale context. Pure representation failures still
	// belong to the ordinary model-schema repair boundary below.
	projected, projectionErr := c.projectResultClaimsWithRecovery(ctx, taskID, input, recovery)
	if projectionErr == nil {
		input = projected
	} else if localResultRejectionReason(rejectProjectedClaims(projectionErr)) != RepairFailureInvalidSchema {
		check("projection_sources", projectionErr)
		return false
	}
	_, payloadErr, repairable := c.validateStructuredPayloadClaims(ctx, taskID, input.StructuredPayload, false, records)
	if payloadErr != nil && !repairable {
		// A model-facing schema shape failure can be repaired, but binding and
		// provenance failures must remain blocked. The normal decoder owns shape.
		issues, _, shapeErr := resultSubmissionIssues(resolved, localResultCandidateValidationInfo(c, taskID))
		if shapeErr != nil || len(issues) == 0 {
			check("payload_sources", payloadErr)
			return false
		}
		// A malformed claims shape cannot prove the validity of source-reference
		// fields. Conservative refusal avoids hiding a second source failure.
		if compiled != nil && (compiled.evidenceReferences != nil || compiled.evidenceInputs != nil || len(compiled.runtimeInputs) > 0 || len(compiled.fixedCollections) > 0) {
			check("payload_sources", payloadErr)
			return false
		}
	}
	return true
}

// Collection repair may carry inert assumption reports only when the frozen
// occurrence cannot apply them to a governing decision. Accepted reference
// repair also preserves all of its original reports exactly.
func (c *Coordinator) validateRecoveryContextClaims(ctx context.Context, cp *ResultCollectionCheckpoint, result *TaskResult, fields []submissionSourceField) error {
	p := acceptedRepairProof(cp)
	retrievalIDs, itemIDs, _ := readableMemorySourceIDs(fields, false)
	if len(result.MemoryUses) > 0 || len(retrievalIDs) > 0 || len(itemIDs) > 0 {
		if cp == nil || p != nil {
			return fmt.Errorf("result-only recovery requires memory attribution reconciliation")
		}
		bound, err := c.collectionSourceFieldsBound(ctx, cp)
		if err != nil {
			return err
		}
		if !bound {
			return fmt.Errorf("result-only recovery requires memory attribution reconciliation")
		}
	}
	assumptionIDs, evidence := readableAssumptionSources(fields, false)
	hasAssumptions := len(result.AssumptionChecks) > 0 || len(assumptionIDs) > 0 || len(evidence) > 0
	if p == nil {
		if !hasAssumptions {
			return nil
		}
		return c.validateCollectionAssumptionsAreInert(ctx, cp)
	}
	original, err := c.readAcceptedRepairResult(ctx, cp.ContractUpgrade.Original, p.ResultSHA256)
	if err != nil {
		return err
	}
	if (len(original.AssumptionChecks) > 0 || len(result.AssumptionChecks) > 0) && !sameCanonicalEvidence(original.AssumptionChecks, result.AssumptionChecks) {
		return fmt.Errorf("accepted-result repair must preserve original assumption checks")
	}
	if hasAssumptions {
		if discipline := c.disciplineFor(cp.TaskID); discipline != nil && discipline.decisionID != "" {
			return fmt.Errorf("accepted-result repair cannot replay governing decision checks")
		}
	}
	return nil
}

func (c *Coordinator) validateInertCollectionAssumptions(ctx context.Context, cp *ResultCollectionCheckpoint, checks []AssumptionCheck) error {
	if len(checks) == 0 {
		return nil
	}
	return c.validateCollectionAssumptionsAreInert(ctx, cp)
}

func (c *Coordinator) validateCollectionAssumptionsAreInert(ctx context.Context, cp *ResultCollectionCheckpoint) error {
	if cp == nil {
		return fmt.Errorf("result-only recovery requires the frozen occurrence for assumption reports")
	}
	if discipline := c.disciplineFor(cp.TaskID); discipline != nil && discipline.decisionID != "" {
		return fmt.Errorf("result-only recovery requires reconciliation of the governing decision")
	}
	item := c.todoItemByID(cp.TaskID)
	admission, found, err := c.validateTaskOccurrenceAdmission(ctx, taskDefFromTodoItem(item), cp.TaskID, cp.Attempt)
	if err != nil {
		return err
	}
	// The admission belongs to the task occurrence on this lineage. A resumed
	// dispatch can finish that same occurrence under a different execution run.
	if !found || admission.Enabled {
		return fmt.Errorf("result-only recovery requires proof that assumption reports cannot change a governing decision")
	}
	return nil
}

// Ordinary resume validates every retained completed task, including tasks
// outside the selected repair's direct dependency set. Preview must not promise
// reuse or workflow continuation while an unrelated accepted source is invalid.
func (c *Coordinator) assessRecoveryAcceptedSources(ctx context.Context, snapshot *RunInputSnapshot, a *ResultCollectionRecoveryAssessment, check func(string, error) bool) bool {
	count := 0
	for _, source := range c.taskTracker.TodoList().Items() {
		if source == nil || IsPrimaryOccurrence(source) || source.Status != TaskDone {
			continue
		}
		var err error
		if source.TypedResult == nil || !taskResultStatusIsSuccessful(source.TypedResult.Status) {
			err = fmt.Errorf("task %s has no accepted successful result", source.ID)
		} else if snapshot != nil {
			err = c.validateCompletedResumeEvidence(ctx, source, snapshot)
		} else {
			err = c.validateAcceptedDependencyPayloadWithContext(ctx, source)
		}
		if !check("accepted_source", err) {
			return false
		}
		count++
	}
	a.AcceptedTasks = new(count)
	return true
}
