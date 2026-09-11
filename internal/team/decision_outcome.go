package team

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Decision outcomes (docs/architecture/decision-runtime.md §40, §49.2).
//
// Recording an outcome is the entry point Phase 5 needs; it is not Phase 5.
// Nothing here computes a Brier score, reweights a judge, or updates a
// capability. It answers one question durably: what actually happened, and is
// that claim backed by evidence or only asserted.

// Outcome classifications. They describe what happened, not whether the
// decision was good: a bad outcome does not by itself prove the judgment was
// unreasonable given the evidence available at the time (spec §49.2).
const (
	OutcomeSucceeded  = "succeeded"
	OutcomeFailed     = "failed"
	OutcomeMixed      = "mixed"
	OutcomeSuperseded = "superseded"
	OutcomeUnresolved = "unresolved"
)

// ValidOutcome reports whether value is a declared classification.
func ValidOutcome(value string) bool {
	switch value {
	case OutcomeSucceeded, OutcomeFailed, OutcomeMixed, OutcomeSuperseded, OutcomeUnresolved:
		return true
	}
	return false
}

// DecisionOutcomeRecord is what a resolved decision turned out to be. It is a
// separate record: resolving an outcome never edits the DecisionRecord, which
// stays exactly as it was formed (spec §35, §49.2).
type DecisionOutcomeRecord struct {
	SchemaVersion int    `json:"schema_version"`
	DecisionID    string `json:"decision_id"`

	ResolvedOutcome string   `json:"resolved_outcome"`
	Forecast        float64  `json:"forecast,omitempty"`
	SuccessCriteria []string `json:"success_criteria,omitempty"`

	ObservedEvidence []ArtifactRef `json:"observed_evidence,omitempty"`
	Lessons          []string      `json:"lessons,omitempty"`
	Notes            string        `json:"notes,omitempty"`

	// ResolvedBy names who recorded the outcome. It is provenance, never
	// justification: an operator's say-so does not make an outcome verified.
	ResolvedBy string    `json:"resolved_by,omitempty"`
	ResolvedAt time.Time `json:"resolved_at,omitzero"`

	// Verified is true only when the outcome cites evidence that actually
	// resolves in the artifact store. See VerifyOutcomeEvidence.
	Verified            bool     `json:"verified"`
	UnverifiedEvidence  []string `json:"unverified_evidence,omitempty"`
	VerificationSummary string   `json:"verification_summary,omitempty"`
}

// OutcomeSchemaVersion versions the persisted outcome record.
const OutcomeSchemaVersion = 1

// ValidateOutcome checks a submitted outcome before it is recorded.
func ValidateOutcome(outcome *DecisionOutcomeRecord) error {
	if outcome == nil {
		return fmt.Errorf("nil outcome record")
	}
	if strings.TrimSpace(outcome.DecisionID) == "" {
		return fmt.Errorf("outcome requires a decision id")
	}
	if !ValidOutcome(outcome.ResolvedOutcome) {
		return fmt.Errorf("resolved_outcome %q is not one of succeeded, failed, mixed, superseded, unresolved",
			outcome.ResolvedOutcome)
	}
	if outcome.Forecast != 0 && !validUnit(outcome.Forecast) {
		return fmt.Errorf("forecast %v is outside [0, 1] or not finite", outcome.Forecast)
	}
	outcome.Forecast = roundDecision(outcome.Forecast)
	if outcome.SchemaVersion == 0 {
		outcome.SchemaVersion = OutcomeSchemaVersion
	}
	if outcome.SchemaVersion > OutcomeSchemaVersion {
		return fmt.Errorf("outcome record for %s has schema version %d, this build understands at most %d",
			outcome.DecisionID, outcome.SchemaVersion, OutcomeSchemaVersion)
	}
	return nil
}

// ArtifactResolver reports whether an artifact digest exists and is intact.
// FileArtifactStore satisfies this through VerifyOutcomeEvidence's adapter.
type ArtifactResolver interface {
	ResolveDigest(sha256 string) (ArtifactRef, bool)
}

// VerifyOutcomeEvidence decides whether an outcome counts as verified.
//
// This is the "已驗證結果" definition spec §49.2 lists as an entry condition
// for Phase 5, and it is deliberately narrow: an outcome is verified only when
// it cites at least one artifact whose digest actually resolves in the store,
// and every digest it cites resolves. An operator's assertion, however
// confident, sets ResolvedBy but leaves Verified false — the same rule that
// keeps a self-described capability from becoming a trusted one (spec §41).
//
// An outcome with no evidence at all is recorded and unverified, never
// rejected: knowing that something happened without proof is still worth
// keeping, as long as nothing downstream treats it as proven.
func VerifyOutcomeEvidence(outcome *DecisionOutcomeRecord, resolver ArtifactResolver) error {
	if outcome == nil {
		return fmt.Errorf("nil outcome record")
	}
	outcome.Verified = false
	outcome.UnverifiedEvidence = nil

	if len(outcome.ObservedEvidence) == 0 {
		outcome.VerificationSummary = "no observed evidence was cited; outcome recorded as unverified"
		return nil
	}
	if resolver == nil {
		outcome.VerificationSummary = "no artifact store was available to check the cited evidence"
		for _, ref := range outcome.ObservedEvidence {
			outcome.UnverifiedEvidence = append(outcome.UnverifiedEvidence, ref.SHA256)
		}
		return nil
	}

	resolved := make([]ArtifactRef, 0, len(outcome.ObservedEvidence))
	var missing []string
	for _, ref := range outcome.ObservedEvidence {
		digest := strings.TrimSpace(ref.SHA256)
		if digest == "" {
			missing = append(missing, fmt.Sprintf("%s (no digest)", ref.Path))
			continue
		}
		found, ok := resolver.ResolveDigest(digest)
		if !ok {
			missing = append(missing, digest)
			continue
		}
		resolved = append(resolved, found)
	}
	sort.Strings(missing)
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].SHA256 < resolved[j].SHA256 })

	outcome.ObservedEvidence = resolved
	outcome.UnverifiedEvidence = missing
	switch {
	case len(missing) > 0:
		outcome.VerificationSummary = fmt.Sprintf(
			"%d of %d cited artifacts could not be resolved in the artifact store; outcome recorded as unverified",
			len(missing), len(missing)+len(resolved))
	case len(resolved) == 0:
		outcome.VerificationSummary = "no cited artifact resolved; outcome recorded as unverified"
	default:
		outcome.Verified = true
		outcome.VerificationSummary = fmt.Sprintf("%d cited artifacts resolved in the artifact store", len(resolved))
	}
	return nil
}
