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
