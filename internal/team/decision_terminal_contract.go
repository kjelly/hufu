package team

import (
	"context"
	"slices"
)

// TerminalEntryPoint identifies every runtime path that can request terminal
// processing. Decision-intent runs use this closed set so no compatibility or
// emergency path can bypass primary-decision preparation.
type TerminalEntryPoint string

const (
	TerminalEntryFinishTool       TerminalEntryPoint = "finish_tool"
	TerminalEntryCoordinatorEOF   TerminalEntryPoint = "coordinator_eof"
	TerminalEntryDirectAgent      TerminalEntryPoint = "direct_agent"
	TerminalEntryFastRoute        TerminalEntryPoint = "fast_route"
	TerminalEntryReusedWork       TerminalEntryPoint = "reused_work"
	TerminalEntryResumeCompletion TerminalEntryPoint = "resume_completion"
	TerminalEntryPartialAck       TerminalEntryPoint = "partial_ack"
	TerminalEntryWorkerHardStop   TerminalEntryPoint = "worker_hard_stop"
	TerminalEntryTurnLimit        TerminalEntryPoint = "turn_limit"
	TerminalEntryBudgetStop       TerminalEntryPoint = "budget_stop"
	TerminalEntryNoProgress       TerminalEntryPoint = "no_progress"
	TerminalEntryAcceptanceRepair TerminalEntryPoint = "acceptance_repair"
	TerminalEntryProviderFailure  TerminalEntryPoint = "provider_failure"
	TerminalEntryWatchdog         TerminalEntryPoint = "watchdog"
	TerminalEntrySignal           TerminalEntryPoint = "signal"
	TerminalEntryEmergency        TerminalEntryPoint = "emergency"
	TerminalEntryPanic            TerminalEntryPoint = "panic"
	TerminalEntryEmbedded         TerminalEntryPoint = "embedded"
	TerminalEntryAggregate        TerminalEntryPoint = "aggregate"
)

// TerminalPrimaryMode is the maximum primary-decision work an entry point may
// authorize. Runtime state, budgets, cancellation, and policy may narrow it.
type TerminalPrimaryMode string

const (
	TerminalPrimaryStart      TerminalPrimaryMode = "start"
	TerminalPrimaryResumeOnly TerminalPrimaryMode = "resume_only"
	TerminalPrimaryForbidden  TerminalPrimaryMode = "forbidden"
)

// TerminalEntryPolicy is the deterministic preflight policy for one terminal
// entry point. It contains no mutable runtime state.
type TerminalEntryPolicy struct {
	PrimaryMode TerminalPrimaryMode
}

var terminalEntryPointOrder = []TerminalEntryPoint{
	TerminalEntryFinishTool,
	TerminalEntryCoordinatorEOF,
	TerminalEntryDirectAgent,
	TerminalEntryFastRoute,
	TerminalEntryReusedWork,
	TerminalEntryResumeCompletion,
	TerminalEntryPartialAck,
	TerminalEntryWorkerHardStop,
	TerminalEntryTurnLimit,
	TerminalEntryBudgetStop,
	TerminalEntryNoProgress,
	TerminalEntryAcceptanceRepair,
	TerminalEntryProviderFailure,
	TerminalEntryWatchdog,
	TerminalEntrySignal,
	TerminalEntryEmergency,
	TerminalEntryPanic,
	TerminalEntryEmbedded,
	TerminalEntryAggregate,
}

var terminalEntryPolicies = map[TerminalEntryPoint]TerminalEntryPolicy{
	TerminalEntryFinishTool:       {PrimaryMode: TerminalPrimaryStart},
	TerminalEntryCoordinatorEOF:   {PrimaryMode: TerminalPrimaryStart},
	TerminalEntryDirectAgent:      {PrimaryMode: TerminalPrimaryStart},
	TerminalEntryFastRoute:        {PrimaryMode: TerminalPrimaryStart},
	TerminalEntryReusedWork:       {PrimaryMode: TerminalPrimaryStart},
	TerminalEntryResumeCompletion: {PrimaryMode: TerminalPrimaryResumeOnly},
	TerminalEntryPartialAck:       {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryWorkerHardStop:   {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryTurnLimit:        {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryBudgetStop:       {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryNoProgress:       {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryAcceptanceRepair: {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryProviderFailure:  {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryWatchdog:         {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntrySignal:           {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryEmergency:        {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryPanic:            {PrimaryMode: TerminalPrimaryForbidden},
	TerminalEntryEmbedded:         {PrimaryMode: TerminalPrimaryStart},
	TerminalEntryAggregate:        {PrimaryMode: TerminalPrimaryForbidden},
}

// TerminalEntryPoints returns the complete stable wire-order inventory.
func TerminalEntryPoints() []TerminalEntryPoint {
	return slices.Clone(terminalEntryPointOrder)
}

// TerminalEntryPolicyFor returns the closed preflight policy for entryPoint.
func TerminalEntryPolicyFor(entryPoint TerminalEntryPoint) (TerminalEntryPolicy, bool) {
	policy, ok := terminalEntryPolicies[entryPoint]
	return policy, ok
}

// TerminalPreparationAction is the result of common pre-terminal processing.
type TerminalPreparationAction string

const (
	TerminalPreparationContinueWork   TerminalPreparationAction = "continue_work"
	TerminalPreparationCommitTerminal TerminalPreparationAction = "commit_terminal"
	TerminalPreparationRecoverOnly    TerminalPreparationAction = "recover_only"
)

// TerminalIntent is the runtime-owned input to common terminal preparation.
type TerminalIntent struct {
	EntryPoint            TerminalEntryPoint
	Cause                 string
	WantsSuccess          bool
	CanContinueSupporting bool
}

// TerminalPreparationProof is the runtime-only proof required by a
// decision-intent FinalizeRun call. Nullable wire fields deliberately do not
// use omitempty so persisted JSON distinguishes null from an absent field.
type TerminalPreparationProof struct {
	SchemaVersion         int                       `json:"schema_version"`
	Kind                  string                    `json:"kind"`
	LogicalRunID          string                    `json:"logical_run_id"`
	ExecutionRunID        string                    `json:"execution_run_id"`
	BranchID              string                    `json:"branch_id"`
	Generation            uint32                    `json:"generation"`
	SupportRevisionDigest *string                   `json:"support_revision_digest"`
	Action                TerminalPreparationAction `json:"action"`
	PrimaryBindingEventID *string                   `json:"primary_binding_event_id"`
	ReasonCodes           []string                  `json:"reason_codes"`
}

// TerminalPreparation is the immutable result of the common pre-terminal
// boundary. Candidate is handed to the existing terminal writer only when the
// action is commit_terminal.
type TerminalPreparation struct {
	Action         TerminalPreparationAction
	Candidate      *RunResult
	PrimaryBinding *PrimaryBindingV1
	Proof          *TerminalPreparationProof
	ReasonCodes    []string
}

// DecisionTerminalPreparationRequest is the complete runtime-owned input to a
// primary decision preparer. Implementations may perform provider work only
// with Context; CleanupContext is deliberately not exposed here.
type DecisionTerminalPreparationRequest struct {
	Intent         TerminalIntent
	Candidate      *RunResult
	LogicalRunID   string
	ExecutionRunID string
	BranchID       string
	Generation     uint32
}

// DecisionTerminalPreparationResult is returned by the primary decision
// service after it either binds one primary result, identifies repairable
// supporting work, or determines that replay-only recovery is required.
type DecisionTerminalPreparationResult struct {
	Action                TerminalPreparationAction
	PrimaryBinding        *PrimaryBindingV1
	PrimaryBindingEventID *string
	SupportRevisionDigest *string
	ReasonCodes           []string
}

// DecisionTerminalPreparer performs primary decision work before terminal
// candidate election. It must honor cancellation and must not call
// FinalizeRun or RequestRunTermination recursively.
type DecisionTerminalPreparer interface {
	PrepareDecisionForTerminal(context.Context, DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error)
}

// DecisionTerminalPreparerFunc adapts a function to DecisionTerminalPreparer.
type DecisionTerminalPreparerFunc func(context.Context, DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error)

func (fn DecisionTerminalPreparerFunc) PrepareDecisionForTerminal(ctx context.Context, request DecisionTerminalPreparationRequest) (DecisionTerminalPreparationResult, error) {
	return fn(ctx, request)
}

// DecisionTerminalConfig enables the primary decision terminal contract for a
// coordinator. Identity and any already-bound primary are durable snapshots;
// they are never inferred from model prose.
type DecisionTerminalConfig struct {
	LogicalRunID           string
	BranchID               string
	Generation             uint32
	Preparer               DecisionTerminalPreparer
	ExistingBinding        *PrimaryBindingV1
	ExistingBindingEventID *string
}
