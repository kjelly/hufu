package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const actionHashDomain = "hufu-operator-action-v1\x00"

var ErrStaleAction = errors.New("stale_action")

type ActionBuildInput struct {
	Target        ActionTarget
	Preconditions ActionPreconditions
	ReasonCode    string
	Availability  string
	SourceRefs    []string
}

type actionDefinition struct {
	kind         string
	actor        string
	risk         string
	confirmation string
	command      string
}

func actionDefinitionFor(id string) (actionDefinition, bool) {
	switch id {
	case ActionSelectScope:
		return actionDefinition{kind: "inspect", actor: "user", risk: "read-only", confirmation: "none"}, true
	case ActionInspectIntegrity:
		return actionDefinition{kind: "inspect", actor: "user", risk: "read-only", confirmation: "none", command: "inspect-replay"}, true
	case ActionInspectTaskRecovery:
		return actionDefinition{kind: "inspect", actor: "user", risk: "read-only", confirmation: "none", command: "inspect-task"}, true
	case ActionProvideInput:
		return actionDefinition{kind: "provide_input", actor: "user", risk: "approval", confirmation: "existing-runtime-gate"}, true
	case ActionReviewApproval:
		return actionDefinition{kind: "review", actor: "user", risk: "approval", confirmation: "explicit-review"}, true
	case ActionWaitRuntime:
		return actionDefinition{kind: "wait", actor: "runtime", risk: "read-only", confirmation: "none"}, true
	case ActionResumeSession:
		return actionDefinition{kind: "mutate", actor: "user", risk: "local-write", confirmation: "existing-runtime-gate", command: "session-resume"}, true
	case ActionReconcileTask:
		return actionDefinition{kind: "mutate", actor: "user", risk: "external-effect", confirmation: "explicit-review", command: "session-reconcile"}, true
	case ActionRetryTask:
		return actionDefinition{kind: "mutate", actor: "user", risk: "external-effect", confirmation: "explicit-review", command: "session-retry"}, true
	case ActionReviewResult:
		return actionDefinition{kind: "inspect", actor: "user", risk: "read-only", confirmation: "none", command: "inspect-run"}, true
	case ActionNoneRequired:
		return actionDefinition{kind: "none", actor: "user", risk: "read-only", confirmation: "none"}, true
	default:
		return actionDefinition{}, false
	}
}

// BuildAction is the sole v1 action registry and typed argv builder. Mutation
// facades remain deliberately unpublished in Phase 2, so their suggestions
// are retained for explanation but can never contain executable argv.
func BuildAction(id string, input ActionBuildInput) (ActionSuggestion, error) {
	definition, ok := actionDefinitionFor(id)
	if !ok {
		return ActionSuggestion{}, fmt.Errorf("unknown operator action %q", id)
	}
	action := ActionSuggestion{
		ID: id, Kind: definition.kind, Actor: definition.actor,
		ReasonCode: strings.TrimSpace(input.ReasonCode), Availability: strings.TrimSpace(input.Availability),
		Risk: definition.risk, Target: input.Target, Preconditions: input.Preconditions,
		Argv: []string{}, CWD: input.Target.Workspace, Confirmation: definition.confirmation,
		SourceRefs: cloneSorted(input.SourceRefs),
	}
	if action.Availability == "" {
		action.Availability = "available"
	}
	if !contains([]string{"available", "blocked", "unknown"}, action.Availability) {
		return ActionSuggestion{}, fmt.Errorf("operator action %q has invalid availability %q", id, action.Availability)
	}
	if definition.kind == "mutate" {
		action.Availability = "blocked"
		if action.Preconditions.ExpectedRevision != "" {
			key, err := ComputeActionRevalidationKey(action)
			if err != nil {
				return ActionSuggestion{}, err
			}
			action.RevalidationKey = key
		}
	}
	if action.Availability == "available" && definition.command != "" {
		action.Argv = buildRegisteredArgv(definition.command, input.Target)
		if len(action.Argv) == 0 {
			action.Availability = "blocked"
		}
	}
	if action.Availability != "available" {
		action.Argv = []string{}
	}
	return action, nil
}

func buildRegisteredArgv(command string, target ActionTarget) []string {
	var argv []string
	switch command {
	case "inspect-replay":
		if target.RunID == "" || target.Workspace == "" {
			return []string{}
		}
		argv = []string{"hufu", "inspect", "replay", target.RunID}
		argv = appendFlag(argv, "--workspace", target.Workspace)
		argv = appendFlag(argv, "--branch", target.BranchID)
		return argv
	case "inspect-task":
		if target.TaskID == "" || target.RunID == "" || target.Workspace == "" {
			return []string{}
		}
		argv = []string{"hufu", "inspect", "task", target.TaskID}
		argv = appendFlag(argv, "--workspace", target.Workspace)
		argv = appendFlag(argv, "--run", target.RunID)
		argv = appendFlag(argv, "--branch", target.BranchID)
		if target.Attempt > 0 {
			argv = append(argv, "--attempt", strconv.Itoa(target.Attempt))
		}
		return argv
	case "inspect-run":
		if target.RunID == "" || target.Workspace == "" {
			return []string{}
		}
		argv = []string{"hufu", "inspect", "run", target.RunID}
		argv = appendFlag(argv, "--workspace", target.Workspace)
		argv = appendFlag(argv, "--branch", target.BranchID)
		return argv
	case "session-resume":
		argv = []string{"hufu", "session", "resume"}
	case "session-reconcile":
		argv = []string{"hufu", "session", "reconcile"}
	case "session-retry":
		argv = []string{"hufu", "session", "retry"}
	default:
		return []string{}
	}
	argv = appendFlag(argv, "--workspace", target.Workspace)
	argv = appendFlag(argv, "--team", target.TeamID)
	argv = appendFlag(argv, "--run", target.RunID)
	argv = appendFlag(argv, "--branch", target.BranchID)
	argv = appendFlag(argv, "--task", target.TaskID)
	if target.Attempt > 0 {
		argv = append(argv, "--attempt", strconv.Itoa(target.Attempt))
	}
	return argv
}

func appendFlag(argv []string, flag, value string) []string {
	if strings.TrimSpace(value) == "" {
		return argv
	}
	return append(argv, flag, value)
}

func ComputeActionRevalidationKey(action ActionSuggestion) (string, error) {
	type keyInput struct {
		Target        ActionTarget        `json:"target"`
		Preconditions ActionPreconditions `json:"preconditions"`
		ID            string              `json:"id"`
		Risk          string              `json:"risk"`
		Confirmation  string              `json:"confirmation"`
		SourceRefs    []string            `json:"source_refs"`
	}
	input := keyInput{
		Target: action.Target, Preconditions: action.Preconditions, ID: action.ID,
		Risk: action.Risk, Confirmation: action.Confirmation, SourceRefs: cloneSorted(action.SourceRefs),
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("marshal operator action revalidation input: %w", err)
	}
	digest := sha256.Sum256(append([]byte(actionHashDomain), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func RevalidateAction(expected, current ActionSuggestion) error {
	if expected.RevalidationKey == "" {
		return fmt.Errorf("%w: expected action has no revalidation key", ErrStaleAction)
	}
	key, err := ComputeActionRevalidationKey(current)
	if err != nil {
		return err
	}
	if key != expected.RevalidationKey {
		return fmt.Errorf("%w: target facts changed", ErrStaleAction)
	}
	return nil
}

func SelectActions(facts ActionSelectionFacts) (*ActionSuggestion, []ActionSuggestion, error) {
	snapshot := facts.Snapshot
	target := actionTargetFromSnapshot(snapshot)
	preconditions := ActionPreconditions{
		BindingStatus: snapshot.Scope.BindingStatus, ExpectedActivity: snapshot.Activity.State,
		ExpectedEventID: snapshot.Freshness.EventID, ExpectedEventHash: snapshot.Freshness.EventHash,
	}
	build := func(id, reason, availability string, recovery *RecoveryEligibility) (ActionSuggestion, error) {
		selectedTarget := target
		selectedPreconditions := preconditions
		var refs []string
		if recovery != nil {
			selectedTarget.TaskID = recovery.TaskID
			selectedTarget.Attempt = recovery.Attempt
			selectedPreconditions.ExpectedTaskStatus = recovery.TaskStatus
			selectedPreconditions.ExpectedEventID = recovery.ExpectedEventID
			selectedPreconditions.ExpectedEventHash = recovery.ExpectedEventHash
			selectedPreconditions.ExpectedPolicy = recovery.EffectivePolicy
			selectedPreconditions.ExpectedRevision = recovery.ExpectedRevision
			selectedPreconditions.ExternalEffectState = recovery.ExternalEffectState
			refs = recovery.SourceRefs
		}
		return BuildAction(id, ActionBuildInput{Target: selectedTarget, Preconditions: selectedPreconditions, ReasonCode: reason, Availability: availability, SourceRefs: refs})
	}

	var primary ActionSuggestion
	var secondary []ActionSuggestion
	var err error
	addSecondary := func(id, reason, availability string, recovery *RecoveryEligibility) {
		if err != nil {
			return
		}
		var action ActionSuggestion
		action, err = build(id, reason, availability, recovery)
		if err == nil {
			secondary = append(secondary, action)
		}
	}
	switch {
	case snapshot.Scope.BindingStatus == "ambiguous" || snapshot.Scope.BindingStatus == "conflict":
		primary, err = build(ActionSelectScope, "scope_not_unique", "blocked", nil)
	case snapshot.Integrity.Status == "invalid" || snapshot.Integrity.Status == "unknown":
		primary, err = build(ActionInspectIntegrity, "integrity_not_valid", "available", nil)
	case facts.Recovery != nil && facts.Recovery.ExternalEffectState == "unknown":
		primary, err = build(ActionInspectTaskRecovery, "external_effect_unknown", inspectionAvailability(facts.Recovery), facts.Recovery)
		if facts.Recovery.ReconcileEligible {
			addSecondary(ActionReconcileTask, "reconcile_eligible_facade_unpublished", "blocked", facts.Recovery)
		}
	case facts.Recovery != nil && facts.Recovery.PolicyDenied:
		primary, err = build(ActionInspectTaskRecovery, "recovery_policy_denied", inspectionAvailability(facts.Recovery), facts.Recovery)
	case snapshot.Activity.State == ActivityWaitingInput:
		primary, err = build(ActionProvideInput, "input_required", "available", facts.Recovery)
	case snapshot.Activity.State == ActivityWaitingApproval:
		primary, err = build(ActionReviewApproval, "approval_required", "available", facts.Recovery)
	case snapshot.Integrity.Status == "degraded" || snapshot.Integrity.Projection == "drift":
		primary, err = build(ActionInspectIntegrity, "projection_requires_diagnosis", "available", nil)
	case snapshot.Activity.State == ActivityInterrupted:
		if facts.Recovery != nil && facts.Recovery.TaskID != "" {
			primary, err = build(ActionInspectTaskRecovery, "interrupted_task_requires_inspection", inspectionAvailability(facts.Recovery), facts.Recovery)
			if facts.Recovery.ReconcileEligible {
				addSecondary(ActionReconcileTask, "reconcile_eligible_facade_unpublished", "blocked", facts.Recovery)
			} else if facts.Recovery.RetryEligible {
				addSecondary(ActionRetryTask, "retry_eligible_facade_unpublished", "blocked", facts.Recovery)
			}
		} else {
			resumePreconditions := preconditions
			resumePreconditions.ExpectedRevision = facts.SessionExpectedRevision
			primary, err = BuildAction(ActionResumeSession, ActionBuildInput{
				Target: target, Preconditions: resumePreconditions,
				ReasonCode: "resume_eligible_facade_unpublished", Availability: "blocked", SourceRefs: facts.SessionSourceRefs,
			})
		}
	case slices.Contains([]string{ActivityPlanning, ActivityExecuting, ActivityVerifying, ActivityWrappingUp, ActivityPreflight}, snapshot.Activity.State):
		primary, err = build(ActionWaitRuntime, "runtime_active", "available", nil)
	case snapshot.Activity.State == ActivityFinished && snapshot.Outcome.RunOutcome == "completed" && len(snapshot.Blockers) == 0:
		primary, err = build(ActionNoneRequired, "run_completed", "available", nil)
	case snapshot.Activity.State == ActivityFinished:
		primary, err = build(ActionReviewResult, "terminal_result_requires_review", "available", nil)
	case facts.Recovery != nil && facts.Recovery.TaskID != "":
		primary, err = build(ActionInspectTaskRecovery, "task_requires_review", inspectionAvailability(facts.Recovery), facts.Recovery)
	default:
		primary, err = build(ActionInspectIntegrity, "insufficient_verified_facts", "available", nil)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(secondary) > 2 {
		secondary = secondary[:2]
	}
	if secondary == nil {
		secondary = []ActionSuggestion{}
	}
	return &primary, secondary, nil
}

func inspectionAvailability(recovery *RecoveryEligibility) string {
	if recovery != nil && recovery.TaskID != "" {
		return "available"
	}
	return "blocked"
}

func actionTargetFromSnapshot(snapshot OperatorSnapshot) ActionTarget {
	return ActionTarget{
		Workspace: snapshot.Scope.WorkspaceExact, ProjectID: snapshot.Scope.ProjectID,
		TeamID: snapshot.Scope.TeamName, SessionID: snapshot.Scope.SessionID,
		RunID: snapshot.Scope.RunID, BranchID: snapshot.Scope.BranchID,
	}
}
