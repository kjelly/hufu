// Package auditverify implements the read-only, offline-verifiable audit
// layer described in spec.md ("Proof-Carrying Run"). It never introduces a
// second event store, artifact store, or completion evaluator: every
// dimension below is derived by re-reading and re-verifying data already
// owned by internal/team (EventStore hash chain, EvidenceManifest,
// AcceptanceResult, ExecutionReceipt) and by calling that package's existing
// verification primitives rather than re-implementing them.
//
// internal/auditverify may import internal/team. internal/team must never
// import internal/auditverify.
package auditverify

import "github.com/kjelly/hufu/internal/team"

// AuditSchemaVersion is the schema version for AuditVerificationResult.
const AuditSchemaVersion = 1

// AuditVerdict is the top-level pass/fail/incomplete verdict for a run audit.
type AuditVerdict string

const (
	AuditVerdictPass       AuditVerdict = "pass"
	AuditVerdictFail       AuditVerdict = "fail"
	AuditVerdictIncomplete AuditVerdict = "incomplete"
)

// AuditDimensionStatus is the per-dimension status inside an audit result.
type AuditDimensionStatus string

const (
	AuditDimensionPass       AuditDimensionStatus = "pass"
	AuditDimensionFail       AuditDimensionStatus = "fail"
	AuditDimensionIncomplete AuditDimensionStatus = "incomplete"
	AuditDimensionSkipped    AuditDimensionStatus = "skipped"
)

// AuditDimensionResult is one row of the audit verdict (integrity, evidence,
// acceptance, ...).
type AuditDimensionResult struct {
	Status AuditDimensionStatus `json:"status"`
	Reason string               `json:"reason,omitempty"`
}

// AuditFinding is a single standardized diagnostic emitted while deriving an
// AuditVerificationResult. Findings are additive diagnostics; the dimension
// statuses above remain the authoritative verdict inputs.
type AuditFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`

	RunID   string `json:"run_id,omitempty"`
	TaskID  string `json:"task_id,omitempty"`
	Attempt int    `json:"attempt,omitempty"`

	Ref string `json:"ref,omitempty"`
}

// Finding severities.
const (
	FindingSeverityCritical = "critical"
	FindingSeverityWarning  = "warning"
	FindingSeverityInfo     = "info"
)

// Finding codes (spec.md §48).
const (
	CodeEventHashMismatch        = "AUDIT-EVENT-HASH-MISMATCH"
	CodeEventChainBroken         = "AUDIT-EVENT-CHAIN-BROKEN"
	CodeTerminalMissing          = "AUDIT-TERMINAL-MISSING"
	CodeTerminalConflict         = "AUDIT-TERMINAL-CONFLICT"
	CodeManifestMissing          = "AUDIT-MANIFEST-MISSING"
	CodeManifestHashMismatch     = "AUDIT-MANIFEST-HASH-MISMATCH"
	CodeArtifactMissing          = "AUDIT-ARTIFACT-MISSING"
	CodeArtifactHashMismatch     = "AUDIT-ARTIFACT-HASH-MISMATCH"
	CodeBindingConflict          = "AUDIT-BINDING-CONFLICT"
	CodeReceiptMissing           = "AUDIT-RECEIPT-MISSING"
	CodeReceiptAmbiguous         = "AUDIT-RECEIPT-AMBIGUOUS"
	CodeAcceptanceNotPassed      = "AUDIT-ACCEPTANCE-NOT-PASSED"
	CodeCriterionEvidenceMissing = "AUDIT-CRITERION-EVIDENCE-MISSING"
	CodeCompletionUnjustified    = "AUDIT-COMPLETION-UNJUSTIFIED"
	CodeBundleHashMismatch       = "AUDIT-BUNDLE-HASH-MISMATCH"
	CodeBundleFileMissing        = "AUDIT-BUNDLE-FILE-MISSING"
	CodeBundlePathUnsafe         = "AUDIT-BUNDLE-PATH-UNSAFE"
	CodeRecheckUnavailable       = "AUDIT-RECHECK-UNAVAILABLE"
)

// AuditVerificationResult is the complete output of hufu audit verify.
type AuditVerificationResult struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`

	Verdict AuditVerdict `json:"verdict"`

	Integrity  AuditDimensionResult `json:"integrity"`
	Provenance AuditDimensionResult `json:"provenance"`
	Evidence   AuditDimensionResult `json:"evidence"`
	Acceptance AuditDimensionResult `json:"acceptance"`
	Completion AuditDimensionResult `json:"completion"`
	Recheck    AuditDimensionResult `json:"recheck"`

	ExpectedOutcome team.RunOutcome `json:"expected_outcome,omitempty"`
	DerivedOutcome  team.RunOutcome `json:"derived_outcome,omitempty"`

	Findings []AuditFinding `json:"findings,omitempty"`
}

// addFinding appends a finding and returns it, for convenient chaining at
// call sites that also want to set a dimension's Reason from the same text.
func (r *AuditVerificationResult) addFinding(code, severity, message, taskID string, attempt int, ref string) {
	if r == nil {
		return
	}
	r.Findings = append(r.Findings, AuditFinding{
		Code: code, Severity: severity, Message: message,
		RunID: r.RunID, TaskID: taskID, Attempt: attempt, Ref: ref,
	})
}

// mandatoryDimensions returns the five dimensions that participate in the
// overall verdict per spec.md §35. Recheck is deliberately excluded.
func (r *AuditVerificationResult) mandatoryDimensions() []AuditDimensionResult {
	return []AuditDimensionResult{r.Integrity, r.Provenance, r.Evidence, r.Acceptance, r.Completion}
}

// finalizeVerdict derives the overall Verdict from the mandatory dimensions.
func (r *AuditVerificationResult) finalizeVerdict() {
	if r == nil {
		return
	}
	hasFail := false
	hasIncomplete := false
	for _, d := range r.mandatoryDimensions() {
		switch d.Status {
		case AuditDimensionFail:
			hasFail = true
		case AuditDimensionIncomplete:
			hasIncomplete = true
		}
	}
	switch {
	case hasFail:
		r.Verdict = AuditVerdictFail
	case hasIncomplete:
		r.Verdict = AuditVerdictIncomplete
	default:
		r.Verdict = AuditVerdictPass
	}
}

// VerifyOptions configures hufu audit verify.
type VerifyOptions struct {
	// Recheck, when true, re-executes deterministic/read-only verifiers
	// (spec.md §17) instead of leaving the Recheck dimension skipped.
	Recheck bool
}
