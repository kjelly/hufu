package team

import (
	"fmt"
	"sort"
	"strings"
)

// The commit gate (docs/hufu-decision-aware-runtime-spec.md §30).
//
// A decision does not imply permission to execute. For a task that can change
// state, the runtime checks the commit prerequisites before the tool process
// starts, so a missing rollback path or an unverifiable outcome blocks the
// mutation instead of being discovered after it.
//
// Every requirement below is decidable from the task contract. The draft spec
// left require-observability undefined and mapped require-rollback onto a
// recovery policy this repo does not have; both are pinned to real fields here.

// defaultCommitGateClasses are the side-effect classes a commit gate guards
// when a profile does not name its own. They match nonReplayableSideEffect in
// recovery.go: exactly the mutations a crash cannot simply replay.
func defaultCommitGateClasses() []SideEffectClass {
	return []SideEffectClass{
		SideEffectExternalWrite,
		SideEffectInfraMutation,
		SideEffectCredential,
		SideEffectUnknown,
	}
}

// CommitGateInput is everything the gate needs. It is a plain struct so the
// decision is testable without a live coordinator.
type CommitGateInput struct {
	Task   TaskDef
	Policy CommitGatePolicy

	// ToolRecovery describes the compensating operation the task's tool
	// declares, if any. It is how require-rollback is satisfied: this repo has
	// no "rollback" recovery policy, only a compensate tool (recovery.go).
	ToolRecovery ToolRecoverySpec
}

// CommitGateDecision is the gate's verdict. A blocked decision always carries a
// canonical reason code so the event log, TUI and tests agree on the cause.
type CommitGateDecision struct {
	Applicable bool
	Allowed    bool
	Reason     string
	Detail     string
	// Missing lists every unsatisfied prerequisite, not only the first, so an
	// operator fixes the contract once rather than iterating.
	Missing []string
}

// Error renders a blocked decision for a tool response or an event payload.
func (d CommitGateDecision) Error() string {
	if d.Allowed {
		return ""
	}
	if d.Detail == "" {
		return d.Reason
	}
	return fmt.Sprintf("%s: %s", d.Reason, d.Detail)
}

// gateApplies reports whether the task's side-effect class is guarded.
func gateApplies(policy CommitGatePolicy, class SideEffectClass) bool {
	guarded := defaultCommitGateClasses()
	if len(policy.RequiredForSideEffects) > 0 {
		guarded = guarded[:0]
		for _, name := range policy.RequiredForSideEffects {
			guarded = append(guarded, SideEffectClass(name))
		}
	}
	for _, candidate := range guarded {
		if candidate == class {
			return true
		}
	}
	return false
}

// EvaluateCommitGate decides whether a side-effecting task may start.
func EvaluateCommitGate(input CommitGateInput) CommitGateDecision {
	class := input.Task.SideEffect
	if class == "" {
		class = SideEffectNone
	}
	if !gateApplies(input.Policy, class) {
		return CommitGateDecision{Applicable: false, Allowed: true}
	}

	missing := map[string]string{}

	if input.Policy.RequireVerification && !taskHasVerification(input.Task) {
		missing[ReasonCommitGateMissingVerification] =
			"task declares no verify command and no verify_spec, so the mutation's outcome cannot be checked"
	}
	if input.Policy.RequireEvidence && !taskProducesEvidence(input.Task) {
		missing[ReasonCommitGateMissingEvidence] =
			"task declares no assertion-bearing verify_spec, so the mutation would leave nothing to accept against"
	}
	if input.Policy.RequireReconcile && !taskCanReconcile(input.Task) {
		missing[ReasonCommitGateMissingReconcile] =
			"task must set recovery: reconcile and a reconcile_tool so an interrupted mutation can be classified"
	}
	if input.Policy.RequireRollback && strings.TrimSpace(input.ToolRecovery.CompensateTool) == "" {
		missing[ReasonCommitGateMissingRecovery] =
			"task's tool declares no compensating operation, so a completed mutation could not be undone"
	}
	if input.Policy.RequireObservability && !taskIsObservable(input.Task) {
		missing[ReasonCommitGateMissingObservability] =
			"task must declare expected_state_change and a reconcile_tool so the actual state can be observed against the intended one"
	}

	if len(missing) == 0 {
		return CommitGateDecision{Applicable: true, Allowed: true}
	}

	reasons := make([]string, 0, len(missing))
	for reason := range missing {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)

	details := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		details = append(details, missing[reason])
	}
	return CommitGateDecision{
		Applicable: true,
		Allowed:    false,
		Reason:     reasons[0],
		Detail:     strings.Join(details, "; "),
		Missing:    reasons,
	}
}

// taskHasVerification reports whether the task's outcome is objectively
// checkable.
func taskHasVerification(task TaskDef) bool {
	if strings.TrimSpace(task.Verify) != "" {
		return true
	}
	return task.VerifySpec != nil
}

// taskProducesEvidence reports whether the task declares a verification that
// yields evidence results, rather than only a pass/fail exit code.
//
// require-verification and require-evidence are deliberately different: a
// `verify: test -f out.pdf` proves something ran, while an assertion-bearing
// verify_spec produces the structured evidence acceptance is judged against.
// A profile that demands evidence is asking for the second.
func taskProducesEvidence(task TaskDef) bool {
	spec := task.VerifySpec
	if spec == nil {
		return false
	}
	return len(spec.Assertions) > 0 ||
		len(spec.ToolCallAssertions) > 0 ||
		len(spec.TaskResultAssertions) > 0
}

// taskCanReconcile reports whether an interrupted mutation could be classified
// as complete, partial or not-started.
func taskCanReconcile(task TaskDef) bool {
	return task.Recovery == RecoveryReconcile && strings.TrimSpace(task.ReconcileTool) != ""
}

// taskIsObservable reports whether the task says what it intends to change and
// offers a read-only probe to see what actually changed.
func taskIsObservable(task TaskDef) bool {
	return strings.TrimSpace(task.ExpectedStateChange) != "" && strings.TrimSpace(task.ReconcileTool) != ""
}
