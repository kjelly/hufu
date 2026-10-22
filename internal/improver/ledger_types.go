package improver

import (
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
)

const (
	eventCreated    = "improvement_run_created"
	eventReserved   = "improvement_envelope_reserved"
	eventVariant    = "improvement_variant_bound"
	eventAdmitted   = "improvement_stage_admitted"
	eventBound      = "improvement_stage_bound"
	eventStarted    = "improvement_stage_started"
	eventCompleted  = "improvement_stage_completed"
	eventTransition = "improvement_stage_transition"
	eventStopped    = "improvement_admission_stopped"
	eventFinished   = "improvement_run_finished"
)

type LedgerScope struct {
	ExperimentID string `json:"experiment_id"`
	WorkspaceID  string `json:"workspace_id"`
	BranchID     string `json:"branch_id"`
	RunID        string `json:"run_id"`
	SessionID    string `json:"session_id"`
}

type eventMeta struct {
	SchemaVersion int       `json:"schema_version"`
	ExperimentID  string    `json:"experiment_id"`
	ObservedAt    time.Time `json:"observed_at"`
}

type runCreated struct {
	eventMeta
	Plan     paired.ArtifactRef `json:"plan_ref"`
	Deadline time.Time          `json:"deadline"`
}

type envelopeReserved struct {
	eventMeta
	Group string `json:"group"`
}

type VariantBinding struct {
	Slot     string             `json:"slot"`
	Snapshot paired.ArtifactRef `json:"snapshot_ref"`
	Digest   string             `json:"variant_digest"`
	Origin   string             `json:"origin"`
}

type variantBound struct {
	eventMeta
	VariantBinding
}

// Lease is authority to bind one child, not permission to call a provider.
// The executor must durably Bind and Start before its first provider call.
type Lease struct {
	StageID          string `json:"stage_id"`
	Generation       int64  `json:"generation"`
	Owner            string `json:"owner"`
	ChildWorkspaceID string `json:"child_workspace_id"`
}

type stageAdmitted struct {
	eventMeta
	Lease
	ReservedTokens int64 `json:"reserved_tokens"`
}

type stageBound struct {
	eventMeta
	Lease
	Identity paired.RuntimeIdentity `json:"identity"`
}

type stageStarted struct {
	eventMeta
	Lease
}

// Settlement is supplied only by an evidence-verifying executor adapter.
// The evidence object is a digest-bound import of canonical child records;
// it is never a replacement receipt manufactured by the parent.
type Settlement struct {
	Identity   paired.RuntimeIdentity `json:"identity"`
	Evidence   paired.ArtifactRef     `json:"evidence_ref"`
	Outcome    string                 `json:"outcome"`
	UsageKnown bool                   `json:"usage_known"`
	Tokens     int64                  `json:"tokens_used"`
	Stopped    bool                   `json:"stopped"`
}

type stageCompleted struct {
	eventMeta
	Lease
	Settlement
}

type stageTransition struct {
	eventMeta
	StageID    string `json:"stage_id"`
	Generation int64  `json:"generation"`
	State      string `json:"state"`
	Reason     string `json:"reason_code"`
}

type admissionStopped struct {
	eventMeta
	Reason string `json:"reason_code"`
}

type runFinished struct {
	eventMeta
	Report   paired.ArtifactRef `json:"report_ref"`
	Decision string             `json:"decision"`
}

type AttemptState struct {
	Lease
	State      string                  `json:"state"`
	Identity   *paired.RuntimeIdentity `json:"identity,omitzero"`
	Settlement *Settlement             `json:"settlement,omitzero"`
	Reason     string                  `json:"reason_code,omitempty"`
	Reserved   int64                   `json:"reserved_tokens"`
	AdmittedAt time.Time               `json:"admitted_at"`
}

type StageState struct {
	Spec     StageSpec      `json:"spec"`
	State    string         `json:"state"`
	Reason   string         `json:"reason_code,omitempty"`
	Attempts []AttemptState `json:"attempts"`
}

// LedgerState is a projection only. Modifying it cannot grant admission.
type LedgerState struct {
	Scope        LedgerScope               `json:"scope"`
	Plan         paired.ArtifactRef        `json:"plan_ref"`
	Created      bool                      `json:"created"`
	StartedAt    time.Time                 `json:"started_at"`
	Deadline     time.Time                 `json:"deadline"`
	ObservedAt   time.Time                 `json:"observed_at"`
	Stages       map[string]*StageState    `json:"stages"`
	Variants     map[string]VariantBinding `json:"variants"`
	Envelopes    map[string]int64          `json:"envelopes"`
	Budget       paired.BudgetSummary      `json:"budget"`
	ProbeRuns    int64                     `json:"probe_runs"`
	StopReason   string                    `json:"stop_reason,omitempty"`
	Report       *paired.ArtifactRef       `json:"report_ref,omitzero"`
	Decision     string                    `json:"decision,omitempty"`
	Head         string                    `json:"head"`
	ResourceOver bool                      `json:"resource_over"`
}
