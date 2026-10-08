package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
)

// State-only projections have no coordinator run ID. Use the replacement's
// runner-owned receipt; the durable manifest separately enforces the active run.
func resolutionRunID(items []*TodoItem, item *TodoItem) string {
	if item == nil || item.Resolution == nil {
		return ""
	}
	resolver := todoItemByID(items, item.Resolution.ResolvedBy)
	if resolver != nil && resolver.ExecutionReceipt != nil {
		return resolver.ExecutionReceipt.RunID
	}
	return ""
}

// VerifiedTaskResolution is a projection, not a status transition. A failed
// occurrence keeps its status, failure and transcript even after replacement.
// Revalidate persisted resolutions so replay cannot turn a label into proof.
func VerifiedTaskResolution(item *TodoItem, items []*TodoItem, runID string) *TodoItem {
	if item == nil || item.Resolution == nil ||
		(item.Resolution.Status != "superseded" && item.Resolution.Status != "reconciled") {
		return nil
	}
	if err := ValidateResolution(item.Resolution, item.ID, items, runID); err != nil {
		return nil
	}
	return todoItemByID(items, item.Resolution.ResolvedBy)
}

// validateResolutionRequirements prevents an unrelated successful task from
// satisfying a failed occurrence's frozen requirements. No task prose is parsed.
func validateResolutionRequirements(target, resolver *TodoItem, runID string) error {
	if target == nil {
		return fmt.Errorf("resolution target is missing")
	}
	if target.ContractID != "" && (target.ContractID != resolver.ContractID ||
		target.ContractHash != resolver.ContractHash || target.ContractRevision != resolver.ContractRevision) {
		return fmt.Errorf("resolving task %s has a different frozen task contract", resolver.ID)
	}
	if !reflect.DeepEqual(target.ResultContract, resolver.ResultContract) ||
		!reflect.DeepEqual(target.Execution, resolver.Execution) ||
		!reflect.DeepEqual(target.Action, resolver.Action) ||
		!reflect.DeepEqual(target.ActionInputBindings, resolver.ActionInputBindings) ||
		!reflect.DeepEqual(target.ResourceScopeSnapshot, resolver.ResourceScopeSnapshot) ||
		target.SideEffect != resolver.SideEffect || target.OutputMode != resolver.OutputMode ||
		target.Phase != resolver.Phase || target.InvariantVerification != resolver.InvariantVerification ||
		!reflect.DeepEqual(target.WorksetBinding, resolver.WorksetBinding) ||
		!reflect.DeepEqual(target.EvidenceFrom, resolver.EvidenceFrom) ||
		(target.RunInputSnapshotHash != "" && target.RunInputSnapshotHash != resolver.RunInputSnapshotHash) ||
		!reflect.DeepEqual(target.BoundInputs, resolver.BoundInputs) {
		return fmt.Errorf("resolving task %s does not preserve the original result, execution or input requirements", resolver.ID)
	}
	expected := normalizedVerificationSpecForCache(target.VerifySpec, target.Verify, target.VerifyMode)
	actual := normalizedVerificationSpecForCache(resolver.VerifySpec, resolver.Verify, resolver.VerifyMode)
	if !reflect.DeepEqual(expected, actual) {
		return fmt.Errorf("resolving task %s has different verification requirements", resolver.ID)
	}
	if resolver.TypedResult != nil && !taskResultStatusIsSuccessful(resolver.TypedResult.Status) {
		return fmt.Errorf("resolving task %s has no successful canonical result", resolver.ID)
	}
	if target.InvariantVerification == InvariantVerificationGate {
		validation := ValidateInvariantVerificationResult(resolver, runID)
		if !validation.Valid || resolver.TypedResult == nil || HasBlockingInvariantAssessment(resolver.InvariantVerification, resolver.TypedResult.InvariantVerification) {
			return fmt.Errorf("resolving task %s lacks a valid clear invariant attestation", resolver.ID)
		}
	}
	if expected != nil {
		verified := resolver.VerifyResult
		if verified == nil || verified.ExitCode != 0 || verified.TimedOut || verified.Overturned {
			return fmt.Errorf("resolving task %s has no passing original verification", resolver.ID)
		}
		observed := normalizedVerificationSpecForCache(verified.Spec, verified.Command, "")
		if !reflect.DeepEqual(expected, observed) {
			return fmt.Errorf("resolving task %s verification evidence does not match the original requirements", resolver.ID)
		}
		if isTaskResultVerificationSpec(expected) {
			if _, err := executeTaskResultAssertVerification("", *expected, resolver.TypedResult); err != nil {
				return fmt.Errorf("resolving task %s no longer satisfies the original assertions: %w", resolver.ID, err)
			}
		}
	}
	return validateResolutionPayloadReceipt(resolver, runID)
}

func validateResolutionPayloadReceipt(resolver *TodoItem, runID string) error {
	if resolver.ResultContract == nil || !resolver.ResultContract.RequireStructured {
		return nil
	}
	if resolver.TypedResult == nil || resolver.TypedResult.StructuredPayload == nil {
		return fmt.Errorf("resolving task %s lacks its required structured payload", resolver.ID)
	}
	payload := resolver.TypedResult.StructuredPayload
	digest := sha256.Sum256(payload.Value)
	receipt := latestSuccessfulExecutionReceipt(resolver, runID)
	if payload.Contract != *resolver.ResultContract || len(payload.Value) > resultPayloadMaxBytes ||
		payload.SHA256 != hex.EncodeToString(digest[:]) || receipt == nil ||
		receipt.TaskID != resolver.ID || receipt.Attempt != resolver.TypedResult.Attempt || resolver.TypedResult.TaskID != resolver.ID ||
		receipt.ResultValidation != ResultValidationValid || receipt.ResultContractID != payload.Contract.ID ||
		receipt.ResultPayloadSHA256 != payload.SHA256 {
		return fmt.Errorf("resolving task %s structured payload has no matching successful validation receipt", resolver.ID)
	}
	return nil
}
