package team

import (
	"github.com/kjelly/hufu/internal/utils"
)

// Presentation-safe projections of the decision index.
//
// Canonical finalization events retain the runtime decision trail; index,
// JSON, report and TUI consumers must never turn a free-text finalization
// reason into a credential disclosure channel. Split out of decision_index.go
// unchanged.
// Redacted returns a presentation-safe copy of an index projection. Canonical
// finalization events retain the runtime decision trail; index, JSON, report,
// and TUI consumers must never turn a free-text finalization reason into a
// credential disclosure channel.
func (e DecisionIndexEntry) Redacted() DecisionIndexEntry {
	e.DecisionID = utils.RedactSecrets(e.DecisionID)
	e.RunID = utils.RedactSecrets(e.RunID)
	e.TaskID = utils.RedactSecrets(e.TaskID)
	e.Profile = utils.RedactSecrets(e.Profile)
	e.EvidenceHash = utils.RedactSecrets(e.EvidenceHash)
	e.Question = utils.RedactSecrets(e.Question)
	e.FinalOption = utils.RedactSecrets(e.FinalOption)
	e.FinalizationMode = utils.RedactSecrets(e.FinalizationMode)
	e.FinalizationIdentity = utils.RedactSecrets(e.FinalizationIdentity)
	e.FinalizationReason = utils.RedactSecrets(e.FinalizationReason)
	e.FinalizationWarnings = redactDecisionStrings(e.FinalizationWarnings)
	e.FinalizationOutcome = utils.RedactSecrets(e.FinalizationOutcome)
	if e.FinalizationResultRef != nil {
		ref := redactArtifactRef(*e.FinalizationResultRef)
		e.FinalizationResultRef = &ref
	}
	e.FalsificationConditions = redactDecisionStrings(e.FalsificationConditions)
	e.RecordRef = redactArtifactRef(e.RecordRef)
	e.RecordDigest = utils.RedactSecrets(e.RecordDigest)
	e.RecordPath = utils.RedactSecrets(e.RecordPath)
	e.StaleReason = utils.RedactSecrets(e.StaleReason)
	e.Assumptions = redactDecisionAssumptions(e.Assumptions)
	e.AssumptionNotes = redactDecisionStrings(e.AssumptionNotes)
	if e.Outcome != nil {
		outcome := redactDecisionOutcome(*e.Outcome)
		e.Outcome = &outcome
	}
	return e
}

// RedactedDecisionIndexEntries makes a presentation-safe copy of a decision
// projection slice without changing its canonical ordering.
func RedactedDecisionIndexEntries(entries []DecisionIndexEntry) []DecisionIndexEntry {
	redacted := make([]DecisionIndexEntry, len(entries))
	for idx, entry := range entries {
		redacted[idx] = entry.Redacted()
	}
	return redacted
}

func redactDecisionStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	redacted := make([]string, len(values))
	for idx, value := range values {
		redacted[idx] = utils.RedactSecrets(value)
	}
	return redacted
}

func redactArtifactRef(ref ArtifactRef) ArtifactRef {
	ref.ID = utils.RedactSecrets(ref.ID)
	ref.Kind = utils.RedactSecrets(ref.Kind)
	ref.Role = utils.RedactSecrets(ref.Role)
	ref.Path = utils.RedactSecrets(ref.Path)
	ref.Description = utils.RedactSecrets(ref.Description)
	ref.Type = utils.RedactSecrets(ref.Type)
	ref.SHA256 = utils.RedactSecrets(ref.SHA256)
	ref.MediaType = utils.RedactSecrets(ref.MediaType)
	ref.RunID = utils.RedactSecrets(ref.RunID)
	ref.TaskID = utils.RedactSecrets(ref.TaskID)
	ref.Agent = utils.RedactSecrets(ref.Agent)
	ref.Provider = utils.RedactSecrets(ref.Provider)
	ref.ToolCallID = utils.RedactSecrets(ref.ToolCallID)
	return ref
}

func redactArtifactRefs(refs []ArtifactRef) []ArtifactRef {
	if len(refs) == 0 {
		return nil
	}
	redacted := make([]ArtifactRef, len(refs))
	for idx, ref := range refs {
		redacted[idx] = redactArtifactRef(ref)
	}
	return redacted
}

func redactDecisionAssumptions(values []DecisionAssumption) []DecisionAssumption {
	if len(values) == 0 {
		return nil
	}
	redacted := make([]DecisionAssumption, len(values))
	for idx, assumption := range values {
		assumption.ID = utils.RedactSecrets(assumption.ID)
		assumption.Statement = utils.RedactSecrets(assumption.Statement)
		assumption.Status = utils.RedactSecrets(assumption.Status)
		assumption.EvidenceRefs = redactArtifactRefs(assumption.EvidenceRefs)
		redacted[idx] = assumption
	}
	return redacted
}

func redactDecisionOutcome(outcome DecisionOutcomeRecord) DecisionOutcomeRecord {
	outcome.DecisionID = utils.RedactSecrets(outcome.DecisionID)
	outcome.ResolvedOutcome = utils.RedactSecrets(outcome.ResolvedOutcome)
	outcome.SuccessCriteria = redactDecisionStrings(outcome.SuccessCriteria)
	outcome.ObservedEvidence = redactArtifactRefs(outcome.ObservedEvidence)
	outcome.Lessons = redactDecisionStrings(outcome.Lessons)
	outcome.Notes = utils.RedactSecrets(outcome.Notes)
	outcome.ResolvedBy = utils.RedactSecrets(outcome.ResolvedBy)
	outcome.UnverifiedEvidence = redactDecisionStrings(outcome.UnverifiedEvidence)
	outcome.VerificationSummary = utils.RedactSecrets(outcome.VerificationSummary)
	return outcome
}

// fileArtifactResolver adapts an ArtifactStore to ArtifactResolver so outcome
// verification can check that cited evidence really exists.
