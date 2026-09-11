package team

import (
	"fmt"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Runtime data model for the decision-aware runtime
// (docs/architecture/decision-runtime.md §13-§27).
//
// Configuration-shaped types live in internal/agent next to TeamConfig; they
// are re-exported here so runtime call sites read naturally.

// DecisionRecordSchemaVersion versions the persisted DecisionRecord. Readers
// must reject a record whose version they do not understand rather than
// silently misinterpreting it.
const DecisionRecordSchemaVersion = 3

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
	OptionProposalPolicy       = agent.OptionProposalPolicy
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

// DecisionOptionOrigin records where an option came from. Without it, a reader
// of a DecisionRecord sees "three options were considered" and assumes someone
// thought of all three (spec §19.1).
type DecisionOptionOrigin string

const (
	// OptionOriginDeclared is an option the task contract declared.
	OptionOriginDeclared DecisionOptionOrigin = "declared"
	// OptionOriginProposed is an option the proposal stage produced.
	OptionOriginProposed DecisionOptionOrigin = "proposed"
	// OptionOriginRuntime is an option the runtime injected because a policy
	// required it and neither the contract nor the proposal supplied one.
	OptionOriginRuntime DecisionOptionOrigin = "runtime"
)

// ValidOptionOrigin reports whether value is a declared origin. An empty
// origin is treated as declared, which is what pre-proposal records are.
func ValidOptionOrigin(value DecisionOptionOrigin) bool {
	switch value {
	case "", OptionOriginDeclared, OptionOriginProposed, OptionOriginRuntime:
		return true
	}
	return false
}

// EffectiveOrigin returns the option's provenance, defaulting to declared.
func (o DecisionOption) EffectiveOrigin() DecisionOptionOrigin {
	if o.Origin == "" {
		return OptionOriginDeclared
	}
	return o.Origin
}

// DecisionOption is one candidate course of action (spec §19).
type DecisionOption struct {
	ID          string               `json:"id" yaml:"id"`
	Kind        DecisionOptionKind   `json:"kind" yaml:"kind"`
	Origin      DecisionOptionOrigin `json:"origin,omitempty" yaml:"origin,omitempty"`
	Title       string               `json:"title,omitempty" yaml:"title,omitempty"`
	Description string               `json:"description,omitempty" yaml:"description,omitempty"`
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
	ID           string        `json:"id" yaml:"id"`
	Statement    string        `json:"statement" yaml:"statement"`
	Status       string        `json:"status,omitempty" yaml:"status,omitempty"`
	EvidenceRefs []ArtifactRef `json:"evidence_refs,omitempty" yaml:"evidence-refs,omitempty"`
	Critical     bool          `json:"critical,omitempty" yaml:"critical,omitempty"`
	CheckedAt    time.Time     `json:"checked_at,omitzero" yaml:"checked-at,omitempty"`
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
	SourceID                string    `json:"source_id" yaml:"source-id"`
	SourceType              string    `json:"source_type,omitempty" yaml:"source-type,omitempty"`
	ParentSourceIDs         []string  `json:"parent_source_ids,omitempty" yaml:"parent-source-ids,omitempty"`
	DeclaredParentSourceIDs []string  `json:"declared_parent_source_ids,omitempty" yaml:"declared-parent-source-ids,omitempty"`
	IndependenceGroup       string    `json:"independence_group,omitempty" yaml:"independence-group,omitempty"`
	RetrievedAt             time.Time `json:"retrieved_at,omitzero" yaml:"retrieved-at,omitempty"`
	ContentHash             string    `json:"content_hash,omitempty" yaml:"content-hash,omitempty"`
}

// DistributionSummary is the numeric shape of a reference class (spec §17).
type DistributionSummary struct {
	Mean   float64 `json:"mean" yaml:"mean"`
	Median float64 `json:"median" yaml:"median"`
	P10    float64 `json:"p10" yaml:"p10"`
	P90    float64 `json:"p90" yaml:"p90"`
}

// BaseRateEvidence is the typed outside-view contract (spec §17).
type BaseRateEvidence struct {
	ReferenceClass string              `json:"reference_class" yaml:"reference-class"`
	Metric         string              `json:"metric" yaml:"metric"`
	SampleSize     int                 `json:"sample_size" yaml:"sample-size"`
	Distribution   DistributionSummary `json:"distribution" yaml:"distribution"`
	Source         ArtifactRef         `json:"source,omitzero" yaml:"source"`
	Limitations    []string            `json:"limitations,omitempty" yaml:"limitations,omitempty"`
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

	// AgentID names the concrete worker capability routing resolved this
	// judge to, if any. It is empty when the opinion was formed by the
	// team's legacy judge-model sidecar rather than a routed role.
	AgentID string `json:"agent_id,omitempty"`

	// Pinned is true when AgentID was forced by a judge-role.pin
	// configuration rather than resolved by ranking (spec.md v2 §34).
	// BindingReason carries the pin's declared reason. Both are empty on
	// every non-pinned path.
	Pinned        bool   `json:"pinned,omitempty"`
	BindingReason string `json:"binding_reason,omitempty"`

	// Model and Provider record the effective model and canonical transport
	// provider selected at invocation time (spec.md v2 §13 model/provider
	// diversity) — set by the runner alongside AgentID.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
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

	// AgentID names the concrete worker capability routing resolved this
	// challenger to, if any. It is empty when the challenge was formed by
	// the team's legacy judge-model sidecar rather than a routed role.
	AgentID string `json:"agent_id,omitempty"`

	// Pinned is true when AgentID was forced by a challenge-role.pin
	// configuration rather than resolved by ranking (spec.md v2 §34).
	// BindingReason carries the pin's declared reason. Both are empty on
	// every non-pinned path.
	Pinned        bool   `json:"pinned,omitempty"`
	BindingReason string `json:"binding_reason,omitempty"`

	// Model and Provider record the effective model and canonical transport
	// provider selected at invocation time (spec.md v2 §13 model/provider
	// diversity).
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
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

	// AgentID names the concrete worker capability routing resolved this
	// judge's revision to. REVISE reuses JUDGE round 1's binding rather than
	// resolving independently, so this is always identical to the original
	// opinion's AgentID for the same JudgeID. Empty on the legacy sidecar path.
	AgentID string `json:"agent_id,omitempty"`

	// Pinned and BindingReason carry forward the original opinion's pin
	// status, since REVISE reuses that same binding rather than resolving
	// independently (spec.md v2 §34).
	Pinned        bool   `json:"pinned,omitempty"`
	BindingReason string `json:"binding_reason,omitempty"`

	// Model and Provider durably identify the effective REVISE invocation.
	// REVISE must retain the original JUDGE AgentID while still exposing the
	// actual model/provider selected for this provider call.
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
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
	SchemaVersion              int          `json:"schema_version"`
	ID                         string       `json:"id"`
	RunID                      string       `json:"run_id,omitempty"`
	TaskID                     string       `json:"task_id,omitempty"`
	Profile                    string       `json:"profile,omitempty"`
	EvidenceHash               string       `json:"evidence_hash,omitempty"`
	EvidenceArtifactRef        *ArtifactRef `json:"evidence_artifact_ref,omitempty"`
	ReferenceEvidenceResultRef *ArtifactRef `json:"reference_evidence_result_ref,omitempty"`

	RequestContractRef      string `json:"request_contract_ref,omitempty"`
	RequestContractRevision uint64 `json:"request_contract_revision,omitempty"`

	Options     []DecisionOption     `json:"options,omitempty"`
	Assumptions []DecisionAssumption `json:"assumptions,omitempty"`

	Opinions   []DecisionOpinion   `json:"opinions,omitempty"`
	Aggregates []DecisionAggregate `json:"aggregates,omitempty"`
	Challenges []DecisionChallenge `json:"challenges,omitempty"`
	Revisions  []DecisionRevision  `json:"revisions,omitempty"`
	Premortem  *PremortemResult    `json:"premortem,omitempty"`

	// JudgeDiversity/ChallengeDiversity report the diversity actually
	// achieved across each role's durable bindings (spec.md v2 §32), nil
	// when that role was never capability-routed at all.
	JudgeDiversity     *BindingDiversitySummary `json:"judge_diversity,omitempty"`
	ChallengeDiversity *BindingDiversitySummary `json:"challenge_diversity,omitempty"`

	FinalOption           string       `json:"final_option,omitempty"`
	FinalizationMode      string       `json:"finalization_mode,omitempty"`
	FinalizationIdentity  string       `json:"finalization_identity,omitempty"`
	FinalizationReason    string       `json:"finalization_reason,omitempty"`
	FinalizationResultRef *ArtifactRef `json:"finalization_result_ref,omitempty"`
	FinalizationStale     bool         `json:"finalization_stale,omitempty"`
	FinalizationWarnings  []string     `json:"finalization_warnings,omitempty"`
	FinalizationOutcome   string       `json:"finalization_outcome,omitempty"`
	Probability           float64      `json:"probability,omitempty"`

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

// ValidateSchemaVersion accepts only the explicit legacy schema or the current
// schema. Zero, negative, and future versions are never compatibility signals.
func (r DecisionRecord) ValidateSchemaVersion() error {
	switch r.SchemaVersion {
	case 1, 2, DecisionRecordSchemaVersion:
		return nil
	default:
		return fmt.Errorf("decision record %s has unsupported schema version %d", r.ID, r.SchemaVersion)
	}
}

// Normalize stamps the current schema version so a record never reaches the
// store unversioned.
func (r *DecisionRecord) Normalize() {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = DecisionRecordSchemaVersion
	}
}
