package inspect

import (
	"fmt"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

func observationIntegrity(explanation *auditverify.ExplainResult) error {
	if explanation == nil || explanation.Verification == nil {
		return nil
	}
	for _, finding := range explanation.Verification.Findings {
		switch finding.Code {
		case auditverify.CodeEventHashMismatch, auditverify.CodeEventChainBroken,
			auditverify.CodeTerminalConflict, auditverify.CodeManifestHashMismatch,
			auditverify.CodeArtifactHashMismatch, auditverify.CodeBindingConflict,
			auditverify.CodeInvariantAttestationInvalid, auditverify.CodeInvariantWitnessMismatch,
			auditverify.CodeRunInputBindingInvalid:
			// Content-free codes only; raw audit messages can carry private paths.
			return fmt.Errorf("%w: %s", ErrIntegrity, finding.Code)
		}
	}
	return nil
}

func routeEvidence(todo *team.TodoItem, result *team.RunResult, explanation *auditverify.ExplainResult) (state, reason, winnerHash string) {
	if todo.Status != team.TaskDone {
		return "not_verified", "", ""
	}
	var successful []team.ExecutionReceipt
	hasVerification := false
	for _, receipt := range todo.ExecutionReceipts {
		if receipt.TaskID != todo.ID || result != nil && receipt.RunID != result.RunID {
			continue
		}
		if receipt.VerifyResult != nil {
			hasVerification = true
		}
		if receipt.Succeeded() {
			successful = append(successful, receipt)
		}
	}
	if len(successful) == 0 {
		return "unavailable", "missing_receipt", ""
	}
	if !hasVerification {
		return "not_verified", "missing_verification", ""
	}
	if result == nil || result.EvidenceManifest == nil {
		return "unavailable", "evidence_unavailable", ""
	}
	binding, ok := result.EvidenceManifest.VerifiedTaskBinding(todo.ID)
	if !ok || binding.TaskID != todo.ID {
		return "unavailable", "evidence_unavailable", ""
	}
	var winner *team.ExecutionReceipt
	for _, receipt := range successful {
		if receipt.RunID == binding.RunID && receipt.Attempt == binding.Attempt &&
			receipt.ModelExecutionID == binding.ModelExecutionID && receipt.ProducerID == binding.ProducerID &&
			receipt.TranscriptRef == binding.TranscriptRef {
			if winner != nil {
				return "unavailable", "evidence_unavailable", ""
			}
			winner = new(receipt)
		}
	}
	if winner == nil {
		return "unavailable", "missing_receipt", ""
	}
	return qualifyRouteWinner(todo, result, winner, explanation)
}

func qualifyRouteWinner(todo *team.TodoItem, result *team.RunResult, winner *team.ExecutionReceipt, explanation *auditverify.ExplainResult) (state, reason, winnerHash string) {
	if winner.VerifyResult == nil {
		return "not_verified", "missing_verification", ""
	}
	if winner.VerifyResult.ExitCode != 0 || winner.VerifyResult.TimedOut || winner.VerifyResult.Overturned {
		return "not_verified", "verification_failed", ""
	}
	if winner.VerifyResult.Fingerprint == "" {
		return "unavailable", "evidence_unavailable", ""
	}
	if !routeResultContractAvailable(todo, winner) {
		return "unavailable", "evidence_unavailable", ""
	}
	if todo.InvariantVerification == team.InvariantVerificationGate {
		validation := team.ValidateInvariantVerificationResult(todo, result.RunID)
		if !validation.Valid || todo.TypedResult == nil ||
			team.HasBlockingInvariantAssessment(todo.InvariantVerification, todo.TypedResult.InvariantVerification) {
			return "unavailable", "evidence_unavailable", ""
		}
	}
	if explanation == nil || explanation.Verification == nil {
		return "unavailable", "evidence_unavailable", ""
	}
	audit := explanation.Verification
	// These are the existing offline validators. Acceptance and completion
	// remain separate run facts; a legitimate partial run can contain verified tasks.
	if audit.Integrity.Status != auditverify.AuditDimensionPass || audit.Evidence.Status != auditverify.AuditDimensionPass ||
		audit.Provenance.Status != auditverify.AuditDimensionPass || audit.RunInputBinding.Status == auditverify.AuditDimensionFail ||
		audit.RunInputBinding.Status == auditverify.AuditDimensionIncomplete ||
		audit.SemanticRegression.Status == auditverify.AuditDimensionFail || audit.SemanticRegression.Status == auditverify.AuditDimensionIncomplete {
		return "unavailable", "evidence_unavailable", ""
	}
	hash, err := auditverify.HashExecutionReceipt(*winner)
	if err != nil {
		return "unavailable", "evidence_unavailable", ""
	}
	return "verified", "", hash
}

func routeResultContractAvailable(todo *team.TodoItem, receipt *team.ExecutionReceipt) bool {
	contract := todo.ResultContract
	if contract == nil {
		return receipt.ResultContractID == ""
	}
	if contract.SchemaSHA256 == "" || receipt.ResultContractID != contract.ID || receipt.ResultContractSHA256 != contract.SchemaSHA256 {
		return false
	}
	hasPayload := todo.TypedResult != nil && todo.TypedResult.StructuredPayload != nil
	if !contract.RequireStructured && !hasPayload {
		return receipt.ResultValidation == team.ResultValidationAbsent && receipt.ResultPayloadSHA256 == ""
	}
	return hasPayload && receipt.ResultValidation == team.ResultValidationValid && receipt.ResultPayloadSHA256 != ""
}

func routePayloadIntegrity(todo *team.TodoItem, runID string) error {
	if todo.Status != team.TaskDone || todo.ResultContract == nil || todo.TypedResult == nil ||
		todo.TypedResult.StructuredPayload == nil {
		return nil // Missing historical evidence is unavailable, not a contradiction.
	}
	receipt := team.LatestSuccessfulExecutionReceipt(todo, runID)
	if receipt == nil || receipt.ResultValidation != team.ResultValidationValid {
		return nil
	}
	if err := team.ValidatePersistedResultPayloadReceipt(todo, runID); err != nil {
		return fmt.Errorf("%w: structured payload contradicts its validation receipt", ErrIntegrity)
	}
	if receipt.ResultContractSHA256 != todo.ResultContract.SchemaSHA256 {
		return fmt.Errorf("%w: receipt result contract digest contradicts its task", ErrIntegrity)
	}
	return nil
}
