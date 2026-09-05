package team

import (
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Runtime data model for the decision-aware runtime
// (docs/hufu-decision-aware-runtime-spec.md §13-§27).
//
// Configuration-shaped types live in internal/agent next to TeamConfig; they
// are re-exported here so runtime call sites read naturally.

// DecisionRecordSchemaVersion versions the persisted DecisionRecord. Readers
// must reject a record whose version they do not understand rather than
// silently misinterpreting it.
const DecisionRecordSchemaVersion = 1

// Configuration type aliases (spec §12-§13).
type (
	DecisionConfig             = agent.DecisionConfig
	DecisionPolicy             = agent.DecisionPolicy
	DecisionCriterion          = agent.DecisionCriterion
	DisciplinePolicy           = agent.DisciplinePolicy
	AlternativesPolicy         = agent.AlternativesPolicy
	StopPolicy                 = agent.StopPolicy
	KillCriterion              = agent.KillCriterion
	CommitGatePolicy           = agent.CommitGatePolicy
	ReplanPolicy               = agent.ReplanPolicy
	EvidenceIndependencePolicy = agent.EvidenceIndependencePolicy
	OutsideViewPolicy          = agent.OutsideViewPolicy
	AggregationPolicy          = agent.AggregationPolicy
	ChallengePolicy            = agent.ChallengePolicy
	ChallengeTrigger           = agent.ChallengeTrigger
	RevisionPolicy             = agent.RevisionPolicy
	PremortemPolicy            = agent.PremortemPolicy
	ForecastPolicy             = agent.ForecastPolicy
	FinalizationPolicy         = agent.FinalizationPolicy
)

// DecisionOptionKind classifies an option so the runtime can check that
// no-action and information-gathering alternatives were actually considered
// (spec §19).
type DecisionOptionKind string

const (
	OptionExecute     DecisionOptionKind = "execute"
	OptionDefer       DecisionOptionKind = "defer"
	OptionNegotiate   DecisionOptionKind = "negotiate"
	OptionRequestInfo DecisionOptionKind = "request_information"
	OptionReduceScope DecisionOptionKind = "reduce_scope"
	OptionAbandon     DecisionOptionKind = "abandon"
	OptionCustom      DecisionOptionKind = "custom"
)

// IsNoGo reports whether the kind counts as a no-action alternative for the
// AlternativesPolicy gate (spec §19).
func (k DecisionOptionKind) IsNoGo() bool {
	return k == OptionDefer || k == OptionAbandon
}

// Valid reports whether the kind is one of the declared kinds.
func (k DecisionOptionKind) Valid() bool {
	switch k {
	case OptionExecute, OptionDefer, OptionNegotiate, OptionRequestInfo,
		OptionReduceScope, OptionAbandon, OptionCustom:
		return true
	}
	return false
}

// DecisionOption is one candidate course of action (spec §19).
type DecisionOption struct {
	ID          string             `json:"id"`
	Kind        DecisionOptionKind `json:"kind"`
	Title       string             `json:"title,omitempty"`
	Description string             `json:"description,omitempty"`
}

// Assumption lifecycle states (spec §18).
const (
	AssumptionUnknown      = "unknown"
	AssumptionSupported    = "supported"
	AssumptionContradicted = "contradicted"
	AssumptionStale        = "stale"
)

// DecisionAssumption is a typed assumption with an append-only lifecycle. Only
// the three sources in spec §18.1 may change Status; the runtime never infers
// it.
type DecisionAssumption struct {
	ID           string        `json:"id"`
	Statement    string        `json:"statement"`
	Status       string        `json:"status,omitempty"`
	EvidenceRefs []ArtifactRef `json:"evidence_refs,omitempty"`
	Critical     bool          `json:"critical,omitempty"`
	CheckedAt    time.Time     `json:"checked_at,omitzero"`
}

// EffectiveStatus returns the assumption status, defaulting to unknown.
func (a DecisionAssumption) EffectiveStatus() string {
	if a.Status == "" {
		return AssumptionUnknown
	}
	return a.Status
}

// ValidAssumptionStatus reports whether status is a declared lifecycle state.
func ValidAssumptionStatus(status string) bool {
	switch status {
	case AssumptionUnknown, AssumptionSupported, AssumptionContradicted, AssumptionStale:
		return true
	}
	return false
}

// Evidence source types for EvidenceProvenance (spec §28).
const (
	EvidenceSourceArtifact   = "artifact"
	EvidenceSourceURL        = "url"
	EvidenceSourceToolOutput = "tool_output"
	EvidenceSourceMemory     = "memory"
	EvidenceSourceDeclared   = "declared"
)

// EvidenceProvenance records where one piece of evidence came from.
// ParentSourceIDs is runtime-derived and participates in independence
// grouping; DeclaredParentSourceIDs is model-declared and is recorded but
// never trusted for grouping in V1 (spec §28.1).
type EvidenceProvenance struct {
	SourceID                string    `json:"source_id"`
	SourceType              string    `json:"source_type,omitempty"`
	ParentSourceIDs         []string  `json:"parent_source_ids,omitempty"`
	DeclaredParentSourceIDs []string  `json:"declared_parent_source_ids,omitempty"`
	IndependenceGroup       string    `json:"independence_group,omitempty"`
	RetrievedAt             time.Time `json:"retrieved_at,omitzero"`
	ContentHash             string    `json:"content_hash,omitempty"`
}

// DistributionSummary is the numeric shape of a reference class (spec §17).
type DistributionSummary struct {
	Mean   float64 `json:"mean"`
	Median float64 `json:"median"`
	P10    float64 `json:"p10"`
	P90    float64 `json:"p90"`
}

// BaseRateEvidence is the typed outside-view contract (spec §17).
type BaseRateEvidence struct {
	ReferenceClass string              `json:"reference_class"`
	Metric         string              `json:"metric"`
	SampleSize     int                 `json:"sample_size"`
	Distribution   DistributionSummary `json:"distribution"`
	Source         ArtifactRef         `json:"source,omitzero"`
	Limitations    []string            `json:"limitations,omitempty"`
}

// OptionScore is one judge's score for one option. Overall is runtime-computed
// whenever criteria are configured (spec §14.2).
type OptionScore struct {
	OptionID string             `json:"option_id"`
	Criteria map[string]float64 `json:"criteria,omitempty"`
	Overall  float64            `json:"overall"`
}

// DecisionOpinion is one judge's structured judgment (spec §20).
type DecisionOpinion struct {
	ID           string `json:"id"`
	JudgeID      string `json:"judge_id"`
	EvidenceHash string `json:"evidence_hash"`
	Round        int    `json:"round"`

	OptionScores []OptionScore `json:"option_scores,omitempty"`

	PreferredOption    string  `json:"preferred_option,omitempty"`
	SuccessProbability float64 `json:"success_probability,omitempty"`

	KeyAssumptions        []string `json:"key_assumptions,omitempty"`
	DisconfirmingEvidence []string `json:"disconfirming_evidence,omitempty"`
	MissingInformation    []string `json:"missing_information,omitempty"`

	Confidence float64 `json:"confidence,omitempty"`

	// Valid is false when the opinion failed schema validation and was
	// excluded from aggregation. Rejected opinions stay durable for audit.
	Valid          bool   `json:"valid"`
	RejectedReason string `json:"rejected_reason,omitempty"`
}

// DecisionAggregate is the deterministic aggregation of one judgment round.
// It is computed in Go with zero LLM calls (spec §21).
type DecisionAggregate struct {
	ID           string `json:"id"`
	EvidenceHash string `json:"evidence_hash"`
	Round        int    `json:"round"`
	Method       string `json:"method"`
	JudgeCount   int    `json:"judge_count"`

	MeanScores   map[string]float64 `json:"mean_scores,omitempty"`
	MedianScores map[string]float64 `json:"median_scores,omitempty"`
	MinScores    map[string]float64 `json:"min_scores,omitempty"`
	MaxScores    map[string]float64 `json:"max_scores,omitempty"`
	StdDev       map[string]float64 `json:"std_dev,omitempty"`
	MAD          map[string]float64 `json:"mad,omitempty"`

	MeanProbability map[string]float64 `json:"mean_probability,omitempty"`

	PreferredOption string `json:"preferred_option,omitempty"`
}

// DecisionChallenge is the post-aggregate adversarial pass over the candidate
// judgment. It is not adversarial_verify (spec §23, §33).
type DecisionChallenge struct {
	ID                   string   `json:"id"`
	EvidenceHash         string   `json:"evidence_hash"`
	TargetOption         string   `json:"target_option,omitempty"`
	StrongestCountercase string   `json:"strongest_countercase,omitempty"`
	FragileAssumptions   []string `json:"fragile_assumptions,omitempty"`
	MissingEvidence      []string `json:"missing_evidence,omitempty"`
	FalsificationTests   []string `json:"falsification_tests,omitempty"`
	Severity             float64  `json:"severity,omitempty"`
}

// DecisionRevision is one judge's single bounded revision (spec §25).
type DecisionRevision struct {
	ID                string `json:"id"`
	OriginalOpinionID string `json:"original_opinion_id"`
	JudgeID           string `json:"judge_id"`
	EvidenceHash      string `json:"evidence_hash"`

	RevisedScores      []OptionScore `json:"revised_scores,omitempty"`
	RevisedProbability float64       `json:"revised_probability,omitempty"`

	Changed bool   `json:"changed"`
	Reason  string `json:"reason,omitempty"`
}

// FailureMode is one discovered way the plan fails (spec §24).
type FailureMode struct {
	ID                  string        `json:"id"`
	Description         string        `json:"description"`
	Likelihood          float64       `json:"likelihood,omitempty"`
	Impact              float64       `json:"impact,omitempty"`
	EarlyWarningSignals []string      `json:"early_warning_signals,omitempty"`
	Mitigations         []string      `json:"mitigations,omitempty"`
	EvidenceRefs        []ArtifactRef `json:"evidence_refs,omitempty"`
}

// PremortemResult discovers risk; it never rejects an option directly (§24).
type PremortemResult struct {
	ID             string        `json:"id"`
	AssumedOutcome string        `json:"assumed_outcome,omitempty"`
	FailureModes   []FailureMode `json:"failure_modes,omitempty"`
}

// DecisionDegradation records one explicit, non-silent rigor reduction made
// because the budget could not support the configured profile (spec §34.1).
type DecisionDegradation struct {
	Step      string    `json:"step"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Reason    string    `json:"reason,omitempty"`
	Timestamp time.Time `json:"timestamp,omitzero"`
}

// SuccessCriterion is one checkable condition from the request contract (§11).
type SuccessCriterion struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

// Constraint is one declared boundary from the request contract (spec §11).
type Constraint struct {
	ID        string `json:"id"`
	Statement string `json:"statement"`
}

// RequestContract is the request/session-scoped intent contract. It exists so
// intent fields are not duplicated into every TaskDef (spec §11).
type RequestContract struct {
	ID             string `json:"id"`
	RawRequest     string `json:"raw_request,omitempty"`
	DirectQuestion string `json:"direct_question,omitempty"`

	Objective       string               `json:"objective,omitempty"`
	SuccessCriteria []SuccessCriterion   `json:"success_criteria,omitempty"`
	Constraints     []Constraint         `json:"constraints,omitempty"`
	Assumptions     []DecisionAssumption `json:"assumptions,omitempty"`

	Revision  uint64    `json:"revision,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// DecisionRecord is the durable artifact a decision produces. It never stores
// only the winning option (spec §27).
type DecisionRecord struct {
	SchemaVersion int    `json:"schema_version"`
	ID            string `json:"id"`
	RunID         string `json:"run_id,omitempty"`
	TaskID        string `json:"task_id,omitempty"`
	Profile       string `json:"profile,omitempty"`
	EvidenceHash  string `json:"evidence_hash,omitempty"`

	RequestContractRef string `json:"request_contract_ref,omitempty"`

	Options     []DecisionOption     `json:"options,omitempty"`
	Assumptions []DecisionAssumption `json:"assumptions,omitempty"`

	Opinions   []DecisionOpinion   `json:"opinions,omitempty"`
	Aggregates []DecisionAggregate `json:"aggregates,omitempty"`
	Challenges []DecisionChallenge `json:"challenges,omitempty"`
	Revisions  []DecisionRevision  `json:"revisions,omitempty"`
	Premortem  *PremortemResult    `json:"premortem,omitempty"`

	FinalOption      string  `json:"final_option,omitempty"`
	FinalizationMode string  `json:"finalization_mode,omitempty"`
	Probability      float64 `json:"probability,omitempty"`

	AlternativesChecked bool   `json:"alternatives_checked,omitempty"`
	NoGoOptionID        string `json:"no_go_option_id,omitempty"`

	StopPolicySnapshot StopPolicy `json:"stop_policy_snapshot,omitzero"`

	SourceCount            int      `json:"source_count,omitempty"`
	IndependenceGroupCount int      `json:"independence_group_count,omitempty"`
	SharedOriginWarnings   []string `json:"shared_origin_warnings,omitempty"`

	ReplanTriggers []string              `json:"replan_triggers,omitempty"`
	Degradations   []DecisionDegradation `json:"degradations,omitempty"`

	KeyAssumptions          []string `json:"key_assumptions,omitempty"`
	FalsificationConditions []string `json:"falsification_conditions,omitempty"`
	ReviewTriggers          []string `json:"review_triggers,omitempty"`

	// Stale and StaleReason are the only fields that may be set on an already
	// persisted record, and only once, unset → set (spec §35).
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`

	CreatedAt time.Time `json:"created_at,omitzero"`
}

// ValidateSchemaVersion rejects a persisted record written by a newer build.
// A zero version means the record predates in-memory versioning; it is
// normalized on write and never silently reinterpreted on read (spec §35).
func (r DecisionRecord) ValidateSchemaVersion() error {
	if r.SchemaVersion > DecisionRecordSchemaVersion {
		return fmt.Errorf("decision record %s has schema version %d, this build understands at most %d",
			r.ID, r.SchemaVersion, DecisionRecordSchemaVersion)
	}
	return nil
}

// Normalize stamps the current schema version so a record never reaches the
// store unversioned.
func (r *DecisionRecord) Normalize() {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = DecisionRecordSchemaVersion
	}
}
