// Package qualitative projects explicit, provenance-bearing claims without a
// probability model. It performs no I/O, verification, authorization or inference
// from prose. A declared fact or verification remains an input claim: callers
// must authenticate sources and receipts before using the projection elsewhere.
package qualitative

const (
	SchemaVersion   = 1
	ReducerVersion  = "qualitative_v1"
	MaxInputBytes   = 1 << 20
	MaxInputRecords = 2048
	MaxRecords      = 512
	MaxTextBytes    = 4096
)

// Scope is an immutable subject and environment boundary, not a live fingerprint.
// Records from another branch or revision must be projected separately.
type Scope struct {
	BranchID            string `json:"branch_id"`
	SubjectID           string `json:"subject_id"`
	SubjectRevision     string `json:"subject_revision"`
	EnvironmentRevision string `json:"environment_revision"`
}

// Source identifies a caller-supplied, immutable source. Reference is an opaque
// locator, never opened by this package. Digest is the lowercase SHA-256 of the
// source bytes, supplied (not checked against those bytes) by the caller.
type Source struct {
	ID        string `json:"id"`
	Revision  string `json:"revision"`
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

type Kind string

const (
	EvidenceRecord   Kind = "evidence"
	HypothesisRecord Kind = "hypothesis"
	RelationRecord   Kind = "relation"
	GapRecord        Kind = "gap"
	RetractionRecord Kind = "retraction"
)

type EvidenceKind string

const (
	Fact                  EvidenceKind = "fact"
	Interpretation        EvidenceKind = "interpretation"
	ObjectiveVerification EvidenceKind = "objective_verification"
)

type Knowledge string

const (
	Known   Knowledge = "known"
	Unknown Knowledge = "unknown"
)

type Outcome string

const (
	Pass           Outcome = "pass"
	Fail           Outcome = "fail"
	OutcomeUnknown Outcome = "unknown"
)

// Verification is a reference to an objective result, not a claim that this
// library ran a verifier or validated a runtime ExecutionReceipt.
type Verification struct {
	ReceiptID        string  `json:"receipt_id"`
	VerifierRevision string  `json:"verifier_revision"`
	Outcome          Outcome `json:"outcome"`
}

type Evidence struct {
	Kind         EvidenceKind  `json:"kind"`
	Knowledge    Knowledge     `json:"knowledge"`
	Statement    string        `json:"statement,omitempty"`
	Verification *Verification `json:"verification,omitempty"`
}

type Hypothesis struct {
	Claim string `json:"claim"`
}

type Stance string

const (
	Support         Stance = "support"
	Counterevidence Stance = "counterevidence"
)

// Relation is an explicit interpretation by its source. Neither pass/fail nor
// fact/interpretation automatically supplies a stance toward a hypothesis.
type Relation struct {
	HypothesisID string `json:"hypothesis_id"`
	EvidenceID   string `json:"evidence_id"`
	Stance       Stance `json:"stance"`
	Rationale    string `json:"rationale"`
}

type Gap struct {
	HypothesisID string `json:"hypothesis_id"`
	Question     string `json:"question"`
}

// Retraction is a permanent tombstone for a non-retraction record in this input
// set. Corrections need a new ID. Retracting a retraction is not supported.
type Retraction struct {
	TargetID string `json:"target_id"`
	Reason   string `json:"reason"`
}

// Record is a tagged union with exactly one matching payload. IDs are unique
// across all record kinds within Scope. Source and Scope are mandatory even for
// interpretations, gaps and retractions.
type Record struct {
	ID         string      `json:"id"`
	Kind       Kind        `json:"kind"`
	Scope      Scope       `json:"scope"`
	Source     Source      `json:"source"`
	Evidence   *Evidence   `json:"evidence,omitempty"`
	Hypothesis *Hypothesis `json:"hypothesis,omitempty"`
	Relation   *Relation   `json:"relation,omitempty"`
	Gap        *Gap        `json:"gap,omitempty"`
	Retraction *Retraction `json:"retraction,omitempty"`
}

// Input is a complete set, not an ordered event journal or incremental patch.
// References may precede their targets in the slice but must resolve in the set.
type Input struct {
	SchemaVersion  int      `json:"schema_version"`
	ReducerVersion string   `json:"reducer_version"`
	Scope          Scope    `json:"scope"`
	Records        []Record `json:"records"`
}

type Status string

const (
	Unresolved Status = "unknown"
	Supported  Status = "supported"
	Countered  Status = "countered"
	Contested  Status = "contested"
	Withdrawn  Status = "withdrawn"
)

// HypothesisView classifies active links, never truth or task success. Pending
// links cite unknown or withdrawn evidence; gaps remain explicit questions.
// Lists contain record IDs, not independent sample counts or confidence weights.
type HypothesisView struct {
	ID                 string   `json:"id"`
	Status             Status   `json:"status"`
	SupportIDs         []string `json:"support_ids"`
	CounterevidenceIDs []string `json:"counterevidence_ids"`
	PendingRelationIDs []string `json:"pending_relation_ids"`
	GapIDs             []string `json:"gap_ids"`
}

type State struct {
	SchemaVersion  int              `json:"schema_version"`
	ReducerVersion string           `json:"reducer_version"`
	Scope          Scope            `json:"scope"`
	Records        []Record         `json:"records"`
	RetractedIDs   []string         `json:"retracted_ids"`
	Hypotheses     []HypothesisView `json:"hypotheses"`
	InputDigest    string           `json:"input_digest"`
	Digest         string           `json:"digest,omitempty"`
}

type ErrorCode string

const (
	InvalidInput       ErrorCode = "invalid_input"
	UnsupportedVersion ErrorCode = "unsupported_version"
	ResourceLimit      ErrorCode = "resource_limit"
	IdentityConflict   ErrorCode = "identity_conflict"
	ScopeMismatch      ErrorCode = "scope_mismatch"
	InvalidReference   ErrorCode = "invalid_reference"
)

// Error exposes content-free structural diagnostics. It does not echo evidence.
type Error struct {
	Code  ErrorCode
	Field string
}

func (e *Error) Error() string                   { return string(e.Code) + ": " + e.Field }
func invalid(code ErrorCode, field string) error { return &Error{Code: code, Field: field} }
