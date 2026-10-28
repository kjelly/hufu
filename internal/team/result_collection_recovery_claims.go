package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// Decode independently verifiable references even when an unrelated claim
// (for example summary or status) has the wrong JSON type. Never let a full
// struct decode failure hide a forged receipt or an invalid source binding.
func independentRecoveryClaims(raw string) (*SubmitResultInput, error) {
	if !jsontext.Value(raw).IsValid() {
		return nil, nil // No independently decodable fields; callers check sealed sources.
	}
	var fields map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, fmt.Errorf("cannot independently inspect recovery claims: %w", err)
	}
	selected := make(map[string]jsontext.Value)
	for _, name := range []string{"artifacts", "receipt_ids", "memory_uses", "assumption_checks", "structured_payload"} {
		if value, ok := fields[name]; ok {
			selected[name] = value
		}
	}
	body, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	var input SubmitResultInput
	if err := json.Unmarshal(body, &input, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return nil, fmt.Errorf("invalid recovery reference claims: %w", err)
	}
	// An invalid or absent status must not weaken successful-receipt checks.
	input.Status = TaskResultStatusSuccess
	if status, ok := fields["status"]; ok {
		var claimed string
		if json.Unmarshal(status, &claimed) == nil && claimed != "" {
			input.Status = claimed
		}
	}
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
	var err error
	// Independently inspect source references and receipt claims before any
	// schema-format rejection. Invalid outer JSON is repairable only after the
	// sealed work, dependencies, and decisions above have been verified.
	independent, independentErr := independentRecoveryClaims(resolved)
	if !check("independent_claims", independentErr) {
		return false
	}
	if independent == nil && compiled != nil && (compiled.evidenceReferences != nil || compiled.evidenceInputs != nil || len(compiled.runtimeInputs) > 0 || len(compiled.fixedCollections) > 0) {
		check("payload_sources", errors.New("malformed JSON prevents independent verification of source references"))
		return false
	}
	if independent != nil {
		input := *independent
		res := input.taskResult()
		res.Attempt = cp.Attempt
		if len(res.AssumptionChecks) > 0 || len(res.MemoryUses) > 0 {
			check("claims_context", fmt.Errorf("result-only recovery does not support nonempty assumption_checks or memory_uses; original context must be reconciled before recovery"))
			return false
		}
		if len(res.Artifacts) > 0 && !check("artifact_claims", validateSubmittedArtifactClaims(ctx, work.Workspace, res.Artifacts)) {
			return false
		}

		if !check("receipt_claims", c.validateTaskResultReceiptClaims(taskID, &res)) {
			return false
		}
		var records []taskTranscriptRecord
		if compiled != nil && compiled.toolEvidence != nil {
			records, err = submissionToolEvidence(ctx, c, taskID)
		}
		if !check("tool_evidence", err) {
			return false
		}
		_, message, repairable := c.validateStructuredPayloadClaims(taskID, input.StructuredPayload, false, records)
		if message != "" && !repairable {
			// A model-facing schema shape failure can be repaired, but binding and
			// provenance failures must remain blocked. The normal decoder owns shape.
			issues, _, shapeErr := resultSubmissionIssues(resolved, localResultCandidateValidationInfo(c, taskID))
			if shapeErr != nil || len(issues) == 0 {
				check("payload_sources", errors.New(message))
				return false
			}
			// A malformed claims shape cannot prove the validity of source-reference
			// fields. Conservative refusal avoids hiding a second source failure.
			if compiled != nil && (compiled.evidenceReferences != nil || compiled.evidenceInputs != nil || len(compiled.runtimeInputs) > 0 || len(compiled.fixedCollections) > 0) {
				check("payload_sources", errors.New(message))
				return false
			}
		}
	}

	return true
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
			err = c.validateAcceptedDependencyPayload(source)
		}
		if !check("accepted_source", err) {
			return false
		}
		count++
	}
	a.AcceptedTasks = new(count)
	return true
}
