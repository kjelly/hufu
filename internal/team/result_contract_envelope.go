package team

import (
	"context"
	"fmt"
)

func (a *localResultAdmission) bindResultEnvelope(ctx context.Context, compiled *CompiledResultContract, input SubmitResultInput, result *TaskResult, contract taskResultSubmissionContract, recordFailure bool) error {
	if err := a.bindObservedFilesRead(ctx, compiled, input, result, contract); err != nil {
		return err
	}
	if err := validateResultEnvelope(compiled, result); err != nil {
		if recordFailure {
			a.coordinator.recordResultValidationFailure(a.todoID)
		}
		return rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	return nil
}

// The contract may constrain completion status against the fully assembled
// payload. Runtime-owned decisions and accepted source records are bound first;
// a model cannot hide them by submitting an empty collection or a false status.
func validateResultEnvelope(compiled *CompiledResultContract, result *TaskResult) error {
	if compiled == nil || (compiled.envelopeSchema == nil && compiled.runtimeCompletion == "") {
		return nil
	}
	if result == nil {
		return fmt.Errorf("result envelope is missing")
	}
	var value any
	if result.StructuredPayload != nil {
		var err error
		value, err = decodeResultContractJSON(result.StructuredPayload.Value)
		if err != nil {
			return err
		}
	}
	if compiled.runtimeCompletion != "" && taskResultStatusIsSuccessful(result.Status) {
		completion, err := resolveJSONPointer(value, compiled.runtimeCompletion)
		if err != nil {
			return fmt.Errorf("runtime completion is missing: %w", err)
		}
		status, summary, err := runtimeCompletionFields(completion)
		if err != nil || result.Status != status || result.Summary != summary {
			return fmt.Errorf("result status or summary differs from runtime completion")
		}
	}
	if compiled.envelopeSchema == nil {
		return nil
	}
	if err := compiled.envelopeSchema.Validate(map[string]any{"status": result.Status, "structured_payload": value}); err != nil {
		return fmt.Errorf("result envelope violates status/payload contract:\n%s", formatResultPayloadValidationError(err))
	}
	return nil
}
