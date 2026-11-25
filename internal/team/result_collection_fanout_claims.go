package team

import (
	"context"
	"encoding/json/v2"
	"errors"
)

func (c *Coordinator) validateFanoutPayloadSources(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, compiled *CompiledResultContract, inspection string, blob *resultCollectionBlob, prepared bool) error {
	ref, _, err := c.boundResultContract(cp.TaskID)
	if err != nil {
		return err
	}
	raw, _ := collectionResponseJSON(blob.Data)
	var draft []byte
	if ref != nil && ref.Normalizer != "" {
		draft, _ = submittedNormalizerDraft(raw, false)
	}
	stage, err := c.readCollectionNormalization(ref, cp, work.Workspace, draft, blob.Normalization, prepared)
	if err != nil || ref == nil || compiled == nil {
		return err
	}
	if ref.Normalizer != "" && stage == nil {
		return nil // A draft has no canonical payload until the normalizer seals its output.
	}
	if stage == nil {
		return c.validateReadablePayloadSources(ctx, cp.TaskID, compiled, inspection, false)
	}
	fields := c.submissionSourceFields(cp.TaskID, inspection)
	input := SubmitResultInput{normalizerDraft: stage.Raw, resultNormalization: stage}
	_ = json.Unmarshal(fields["status"], &input.Status)
	bound, err := c.bindStagedNormalization(ctx, cp.TaskID, input)
	if localResultRejectionReason(err) == RepairFailureInvalidSchema {
		return nil // The binding checked the sealed output's sources before rejecting its shape.
	} else if err != nil {
		return err
	}
	return c.validatePayloadSourceInput(ctx, cp.TaskID, compiled, inspection, bound, true)
}

// Read payload claims without decoding unrelated envelope fields. Direct tool
// submissions use legacy case folding and the live projection scope.
func (c *Coordinator) validateReadablePayloadSources(ctx context.Context, taskID string, compiled *CompiledResultContract, raw string, direct bool) error {
	if !direct {
		raw = collectionSourceInspectionJSON(raw)
	}
	fields := readableSourceMembers(raw)
	status := receiptSourceStatus(fields, direct)
	found := false
	var formatErr error
	for _, field := range fields {
		if !sourceMemberMatches(field.name, "structured_payload", direct) {
			continue
		}
		found = true
		err := c.validateReadablePayloadField(ctx, taskID, compiled, raw, SubmitResultInput{Status: status, StructuredPayload: field.raw}, !direct)
		if err != nil {
			if localResultRejectionReason(err) != RepairFailureInvalidSchema {
				return err
			}
			// A malformed occurrence cannot conceal claims in a subsequent
			// readable occurrence of structured_payload.
			if formatErr == nil {
				formatErr = err
			}
		}
	}
	if !found {
		return c.validatePayloadSourceInput(ctx, taskID, compiled, raw, SubmitResultInput{}, !direct)
	}
	return formatErr
}

func (c *Coordinator) validateReadablePayloadField(ctx context.Context, taskID string, compiled *CompiledResultContract, raw string, input SubmitResultInput, recovery bool) error {
	payload := input.StructuredPayload
	if payload.IsValid() {
		return c.validateResolvedPayloadSources(ctx, taskID, compiled, raw, input, recovery)
	}
	err := inspectReadablePayloadViews(payload, func(view []byte) error {
		input.StructuredPayload = view
		if err := c.validateResolvedPayloadSources(ctx, taskID, compiled, raw, input, recovery); err != nil && localResultRejectionReason(err) != RepairFailureInvalidSchema {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, err = decodeUniqueJSON(payload)
	if err == nil {
		err = errors.New("structured_payload must contain JSON with unique names")
	}
	return rejectLocalResult(RepairFailureInvalidSchema, err)
}

func (c *Coordinator) validateResolvedPayloadSources(ctx context.Context, taskID string, compiled *CompiledResultContract, raw string, input SubmitResultInput, recovery bool) error {
	input, inspectable, err := c.resolveCollectionPayloadForInspection(ctx, taskID, input)
	if inspectable {
		if sourceErr := c.validatePayloadSourceInput(ctx, taskID, compiled, raw, input, recovery); sourceErr != nil {
			return sourceErr
		}
	}
	return err
}

func (c *Coordinator) validatePayloadSourceInput(ctx context.Context, taskID string, compiled *CompiledResultContract, raw string, input SubmitResultInput, recovery bool) error {
	var sourceErr error
	c.validateIndependentPayloadClaims(ctx, taskID, compiled, input, raw, recovery, func(_ string, err error) bool {
		sourceErr = err
		return err == nil
	})
	if sourceErr != nil {
		return rejectNormalizationRecoveryRead(sourceErr)
	}
	return nil
}

func (c *Coordinator) resolveCollectionPayloadForInspection(ctx context.Context, taskID string, input SubmitResultInput) (SubmitResultInput, bool, error) {
	if !hasNormalizerDraft(input.StructuredPayload) {
		return input, true, nil
	}
	// Inspect only independently decodable payload claims. Malformed adjacent
	// fields still go through the ordinary result-only schema repair.
	wire := `{"structured_payload":` + string(input.StructuredPayload) + `}`
	resolved, err := c.resolveResultEvidenceKeys(ctx, taskID, wire)
	if resolved == "" && isResultReferenceChoiceError(err) {
		return input, true, nil
	}
	if err != nil && (resolved == "" || !isResultEvidenceFormatError(err)) {
		return input, false, rejectNormalizationRecoveryRead(err)
	}
	input.StructuredPayload = c.submissionSourceFields(taskID, resolved)["structured_payload"]
	if err != nil {
		// Keep the original format diagnostics from all workset siblings after
		// the partially resolved payload has passed independent source checks.
		err = rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	return input, true, err
}
