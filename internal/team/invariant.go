package team

import "slices"

const InvariantManifestSchemaVersion = 1

type InvariantSeverity string

const (
	InvariantSeverityError   InvariantSeverity = "error"
	InvariantSeverityWarning InvariantSeverity = "warning"
	InvariantSeverityInfo    InvariantSeverity = "info"
)

type InvariantDefinition struct {
	ID        string            `json:"id" yaml:"id"`
	Statement string            `json:"statement" yaml:"statement"`
	Severity  InvariantSeverity `json:"severity" yaml:"severity"`
	AppliesTo []string          `json:"applies_to" yaml:"applies-to"`
}

type InvariantManifest struct {
	SchemaVersion int                   `json:"schema_version" yaml:"schema-version"`
	Invariants    []InvariantDefinition `json:"invariants" yaml:"invariants"`
}

type InvariantVerificationMode string

const (
	InvariantVerificationReport InvariantVerificationMode = "report"
	InvariantVerificationGate   InvariantVerificationMode = "gate"
)

func cloneInvariantCatalog(catalog []InvariantDefinition) []InvariantDefinition {
	if catalog == nil {
		return nil
	}
	cloned := make([]InvariantDefinition, len(catalog))
	for index, definition := range catalog {
		cloned[index] = definition
		cloned[index].AppliesTo = slices.Clone(definition.AppliesTo)
	}
	return cloned
}

func validInvariantVerificationMode(mode InvariantVerificationMode) bool {
	return mode == "" || mode == InvariantVerificationReport || mode == InvariantVerificationGate
}

type InvariantAssessmentStatus string

const (
	InvariantPreserved InvariantAssessmentStatus = "preserved"
	InvariantViolated  InvariantAssessmentStatus = "violated"
	InvariantUnknown   InvariantAssessmentStatus = "unknown"
)

// InvariantAssessmentClaim is the model-owned assessment shape accepted at
// local and external result boundaries. Runtime attribution is deliberately
// absent and is attached only after the claim is matched to a durable context
// manifest.
type InvariantAssessmentClaim struct {
	InvariantID     string                    `json:"invariant_id"`
	Status          InvariantAssessmentStatus `json:"status"`
	Summary         string                    `json:"summary"`
	FindingIndex    *int                      `json:"finding_index,omitempty"`
	MissingEvidence []string                  `json:"missing_evidence,omitempty"`
}

// InvariantAssessment is the runtime-attested form persisted in TaskResult.
type InvariantAssessment struct {
	InvariantID          string                    `json:"invariant_id"`
	ContextItemID        string                    `json:"context_item_id"`
	InvariantContentHash string                    `json:"invariant_content_hash"`
	Severity             InvariantSeverity         `json:"severity"`
	Status               InvariantAssessmentStatus `json:"status"`
	Summary              string                    `json:"summary"`
	FindingIndex         *int                      `json:"finding_index,omitempty"`
	MissingEvidence      []string                  `json:"missing_evidence,omitempty"`
}

type InvariantVerificationResult struct {
	ContextManifestFingerprint string                `json:"context_manifest_fingerprint"`
	Assessments                []InvariantAssessment `json:"assessments"`
}
