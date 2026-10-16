package belief

import (
	"encoding/hex"
	"encoding/json/v2"
	"slices"
)

const (
	MaxDiagnosticEvidenceBytes = 16 << 10
	DiagnosticAdapterVersion   = "diagnostic_measurement_v1"
)

// DiagnosticRevisions identify pinned inputs, not the entire host environment.
type DiagnosticRevisions struct {
	Subject     string `json:"subject_revision"`
	Source      string `json:"source_revision"`
	Environment string `json:"environment_revision"`
}

// DiagnosticSource is the expected mapping supplied by a trusted runtime.
// Stage 1 only checks this typed contract. It neither establishes journal
// authenticity nor admits evidence; durable source validation comes later.
type DiagnosticSource struct {
	EpisodeID    string `json:"episode_id"`
	DecisionID   string `json:"decision_id"`
	ExecutionID  string `json:"execution_id"`
	ChildTaskID  string `json:"child_task_id"`
	Occurrence   int    `json:"child_occurrence"`
	InvocationID string `json:"invocation_id"`
	ReceiptKey   string `json:"receipt_key"`
	EventID      string `json:"source_event_id"`
	EventHash    string `json:"source_event_hash"`
	Cursor       uint64 `json:"source_cursor"`
	Adapter      string `json:"adapter_version"`
	ExperimentID string `json:"experiment_id"`
}

type DiagnosticEvidence struct {
	SchemaVersion int                 `json:"schema_version"`
	Source        DiagnosticSource    `json:"source"`
	Revisions     DiagnosticRevisions `json:"revisions"`
	Outcome       string              `json:"outcome"`
	Measurement   string              `json:"measurement_digest"`
}

func ValidDiagnosticDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (r DiagnosticRevisions) Validate() error {
	if !ValidDiagnosticDigest(r.Subject) || !ValidDiagnosticDigest(r.Source) || !ValidDiagnosticDigest(r.Environment) {
		return invalid(InvalidDiagnostic, "revisions")
	}
	return nil
}

func DecodeDiagnosticEvidence(data []byte) (DiagnosticEvidence, error) {
	if len(data) > MaxDiagnosticEvidenceBytes {
		return DiagnosticEvidence{}, invalid(ResourceLimit, "diagnostic_evidence_bytes")
	}
	var evidence DiagnosticEvidence
	if err := json.Unmarshal(data, &evidence, json.RejectUnknownMembers(true)); err != nil {
		return DiagnosticEvidence{}, invalid(InvalidDiagnostic, "evidence_json")
	}
	// Numeric zero and an absent field must not be conflated at this boundary.
	root, err := wireObject(data, "schema_version", "source", "revisions", "outcome", "measurement_digest")
	if err != nil {
		return DiagnosticEvidence{}, err
	}
	if _, err := wireObject(root["source"], "episode_id", "decision_id", "execution_id", "child_task_id", "child_occurrence", "invocation_id", "receipt_key", "source_event_id", "source_event_hash", "source_cursor", "adapter_version", "experiment_id"); err != nil {
		return DiagnosticEvidence{}, err
	}
	if _, err := wireObject(root["revisions"], "subject_revision", "source_revision", "environment_revision"); err != nil {
		return DiagnosticEvidence{}, err
	}
	if err := validateDiagnosticEvidence(evidence); err != nil {
		return DiagnosticEvidence{}, err
	}
	return evidence, nil
}

func validateDiagnosticEvidence(e DiagnosticEvidence) error {
	if e.SchemaVersion != SchemaVersion {
		return invalid(UnsupportedVersion, "diagnostic_evidence")
	}
	if e.Source.Adapter != DiagnosticAdapterVersion {
		return invalid(UnsupportedVersion, "diagnostic_adapter")
	}
	for _, id := range []string{e.Source.EpisodeID, e.Source.DecisionID, e.Source.ExecutionID, e.Source.ChildTaskID, e.Source.InvocationID, e.Source.ReceiptKey, e.Source.EventID, e.Source.Adapter, e.Source.ExperimentID} {
		if !validID(id) {
			return invalid(InvalidDiagnostic, "source_identity")
		}
	}
	if e.Source.Occurrence < 1 || e.Source.Occurrence > 2_147_483_647 || e.Source.Cursor == 0 || !ValidDiagnosticDigest(e.Source.EventHash) || !ValidDiagnosticDigest(e.Measurement) {
		return invalid(InvalidDiagnostic, "measurement_identity")
	}
	if !slices.Contains([]string{"pass", "fail"}, e.Outcome) {
		return invalid(InvalidDiagnostic, "outcome")
	}
	return e.Revisions.Validate()
}

// CheckDiagnosticEvidence converts a structurally checked measurement into
// projector input, only when its complete expected mapping and revisions match.
// The caller remains responsible for receipt, artifact and verifier authenticity.
func CheckDiagnosticEvidence(epoch Epoch, expected DiagnosticSource, revisions DiagnosticRevisions, evidence DiagnosticEvidence) (Observation, error) {
	if err := validateDiagnosticEvidence(evidence); err != nil {
		return Observation{}, err
	}
	if evidence.Source != expected || evidence.Revisions != revisions || epoch.ID != expected.EpisodeID || epoch.SubjectRevision != revisions.Subject || epoch.EnvironmentRevision != revisions.Environment || !validID(epoch.Scope.BranchID) {
		return Observation{}, invalid(InvalidDiagnostic, "source_binding")
	}
	id, err := digest("hufu-diagnostic-observation-v1", []string{epoch.Scope.BranchID, epoch.ID, expected.ExecutionID, expected.ExperimentID})
	if err != nil {
		return Observation{}, err
	}
	return Observation{ID: id, ExperimentID: expected.ExperimentID, OutcomeID: evidence.Outcome, SourceID: evidence.Measurement}, nil
}
