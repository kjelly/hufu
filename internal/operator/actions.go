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
	Target                  ActionTarget
	Preconditions           ActionPreconditions
	ReasonCode              string
	Availability            string
	SourceRefs              []string
	MutationFacadeAvailable bool
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
		return actionDefinition{kind: ActionKindInspect, actor: "user", risk: "read-only", confirmation: "none"}, true
	case ActionInspectIntegrity:
		return actionDefinition{kind: ActionKindInspect, actor: "user", risk: "read-only", confirmation: "none", command: "inspect-replay"}, true
	case ActionInspectTaskRecovery:
		return actionDefinition{kind: ActionKindInspect, actor: "user", risk: "read-only", confirmation: "none", command: "inspect-task"}, true
	case ActionAssessResultRepair:
		return actionDefinition{kind: ActionKindInspect, actor: "user", risk: "read-only", confirmation: "none", command: "assess-result-repair"}, true
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
		return actionDefinition{kind: ActionKindInspect, actor: "user", risk: "read-only", confirmation: "none", command: "inspect-run"}, true
	case ActionNoneRequired:
		return actionDefinition{kind: "none", actor: "user", risk: "read-only", confirmation: "none"}, true
	default:
		return actionDefinition{}, false
	}
}

// BuildAction is the sole v1 action registry and typed argv builder. Mutation
// argv is published only when the caller declares the exact-scope facade
// available and supplies a durable revision for mandatory revalidation.
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
		action.Availability = ActionAvailabilityAvailable
	}
	if !contains([]string{ActionAvailabilityAvailable, ActionAvailabilityBlocked, ActionAvailabilityUnknown}, action.Availability) {
		return ActionSuggestion{}, fmt.Errorf("operator action %q has invalid availability %q", id, action.Availability)
	}
	if definition.kind == "mutate" {
		if !input.MutationFacadeAvailable || action.Preconditions.ExpectedRevision == "" {
			action.Availability = ActionAvailabilityBlocked
		}
		if action.Preconditions.ExpectedRevision != "" {
			key, err := ComputeActionRevalidationKey(action)
			if err != nil {
				return ActionSuggestion{}, err
			}
			action.RevalidationKey = key
		}
	}
	if action.Availability == ActionAvailabilityAvailable && definition.command != "" {
		action.Argv = buildRegisteredArgv(definition.command, input.Target)
		if len(action.Argv) == 0 {
			action.Availability = ActionAvailabilityBlocked
		}
	}
	if action.Availability != ActionAvailabilityAvailable {
		action.Argv = []string{}
	}
	return action, nil
}

func buildRegisteredArgv(command string, target ActionTarget) []string {
	if command == "assess-result-repair" {
		if target.Workspace == "" || target.RunID == "" || target.BranchID == "" || target.TaskID == "" {
			return []string{}
		}
		argv := []string{"hufu", "resume", "--workspace", target.Workspace, "--run", target.RunID, "--branch", target.BranchID,
			"--execution-profile", "default", "--repair-result", target.TaskID, "--dry-run"}
		return appendFlag(argv, "--team", target.TeamID)
	}
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
		return BuildAction(id, ActionBuildInput{Target: selectedTarget, Preconditions: selectedPreconditions, ReasonCode: reason, Availability: availability, SourceRefs: refs, MutationFacadeAvailable: facts.MutationFacadeAvailable})
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
		primary, err = build(ActionSelectScope, "scope_not_unique", ActionAvailabilityBlocked, nil)
	case snapshot.Integrity.Status == "invalid" || snapshot.Integrity.Status == "unknown":
		primary, err = build(ActionInspectIntegrity, "integrity_not_valid", ActionAvailabilityAvailable, nil)
	case facts.Recovery != nil && facts.Recovery.ExternalEffectState == "unknown":
		primary, err = build(ActionInspectTaskRecovery, "external_effect_unknown", inspectionAvailability(facts.Recovery), facts.Recovery)
		if facts.Recovery.ReconcileEligible {
			addSecondary(ActionReconcileTask, "reconcile_eligible", ActionAvailabilityAvailable, facts.Recovery)
		}
	case facts.Recovery != nil && facts.Recovery.PolicyDenied:
		primary, err = build(ActionInspectTaskRecovery, "recovery_policy_denied", inspectionAvailability(facts.Recovery), facts.Recovery)
	case snapshot.Activity.State == ActivityWaitingInput:
		primary, err = build(ActionProvideInput, "input_required", ActionAvailabilityAvailable, facts.Recovery)
	case snapshot.Activity.State == ActivityWaitingApproval:
		primary, err = build(ActionReviewApproval, "approval_required", ActionAvailabilityAvailable, facts.Recovery)
	case snapshot.Integrity.Status == "degraded" || snapshot.Integrity.Projection == "drift":
		primary, err = build(ActionInspectIntegrity, "projection_requires_diagnosis", ActionAvailabilityAvailable, nil)
	case snapshot.Activity.State == ActivityInterrupted:
		if facts.Recovery != nil && facts.Recovery.TaskID != "" {
			primary, err = build(ActionInspectTaskRecovery, "interrupted_task_requires_inspection", inspectionAvailability(facts.Recovery), facts.Recovery)
			if facts.Recovery.ReconcileEligible {
				addSecondary(ActionReconcileTask, "reconcile_eligible", ActionAvailabilityAvailable, facts.Recovery)
			} else if facts.Recovery.RetryEligible {
				addSecondary(ActionRetryTask, "retry_eligible", ActionAvailabilityAvailable, facts.Recovery)
			}
		} else {
			resumePreconditions := preconditions
			resumePreconditions.ExpectedRevision = facts.SessionExpectedRevision
			primary, err = BuildAction(ActionResumeSession, ActionBuildInput{
				Target: target, Preconditions: resumePreconditions,
				ReasonCode: "resume_eligible", Availability: ActionAvailabilityAvailable, SourceRefs: facts.SessionSourceRefs,
				MutationFacadeAvailable: facts.MutationFacadeAvailable,
			})
		}
	case slices.Contains([]string{ActivityPlanning, ActivityExecuting, ActivityVerifying, ActivityWrappingUp, ActivityPreflight}, snapshot.Activity.State):
		primary, err = build(ActionWaitRuntime, "runtime_active", ActionAvailabilityAvailable, nil)
	case isTerminalReplan(facts):
		resumePreconditions := preconditions
		resumePreconditions.ExpectedRevision = facts.SessionExpectedRevision
		primary, err = BuildAction(ActionResumeSession, ActionBuildInput{
			Target: target, Preconditions: resumePreconditions,
			ReasonCode: "replan_required", Availability: ActionAvailabilityAvailable, SourceRefs: facts.SessionSourceRefs,
			MutationFacadeAvailable: facts.MutationFacadeAvailable,
		})
	case snapshot.Activity.State == ActivityFinished && snapshot.Outcome.RunOutcome == "completed" && len(snapshot.Blockers) == 0:
		primary, err = build(ActionNoneRequired, "run_completed", ActionAvailabilityAvailable, nil)
	case isFinishedRecovery(snapshot, facts.Recovery):
		primary, err = build(ActionInspectTaskRecovery, "terminal_task_requires_recovery", inspectionAvailability(facts.Recovery), facts.Recovery)
		secondary, err = appendRecoveryMutationAction(secondary, facts.Recovery, build, err)
	case snapshot.Activity.State == ActivityFinished:
		primary, err = build(ActionReviewResult, "terminal_result_requires_review", ActionAvailabilityAvailable, nil)
	case facts.Recovery != nil && facts.Recovery.TaskID != "":
		primary, err = build(ActionInspectTaskRecovery, "task_requires_review", inspectionAvailability(facts.Recovery), facts.Recovery)
		secondary, err = appendRecoveryMutationAction(secondary, facts.Recovery, build, err)
	default:
		primary, err = build(ActionInspectIntegrity, "insufficient_verified_facts", ActionAvailabilityAvailable, nil)
	}
	if err != nil {
		return nil, nil, err
	}
	if len(secondary) > 2 {
		secondary = secondary[:2]
	}
	secondary, err = appendCollectionAssessmentActions(facts, primary, secondary, build)
	if err != nil {
		return nil, nil, err
	}
	if secondary == nil {
		secondary = []ActionSuggestion{}
	}
	return &primary, secondary, nil
}

// appendCollectionAssessmentActions keeps the primary safety priority and the
// general secondary-action bound. Each blocked collection additionally gets a
// read-only preview, without granting repair authority or replaying tools.
func appendCollectionAssessmentActions(
	facts ActionSelectionFacts,
	primary ActionSuggestion,
	secondary []ActionSuggestion,
	build func(string, string, string, *RecoveryEligibility) (ActionSuggestion, error),
) ([]ActionSuggestion, error) {
	snapshot := facts.Snapshot
	if (snapshot.Scope.BindingStatus != "unique" && snapshot.Scope.BindingStatus != "verified") || primary.ID == ActionWaitRuntime ||
		(snapshot.Integrity.Status != "valid" && snapshot.Integrity.Status != "degraded") ||
		snapshot.Integrity.Projection != "consistent" || snapshot.Integrity.EventChain != "verified" {
		return secondary, nil
	}
	seen := make(map[string]bool)
	candidates := append([]*RecoveryEligibility{facts.Recovery}, facts.ResultCollectionRecoveries...)
	for _, recovery := range candidates {
		if recovery == nil || !recovery.ResultCollectionBlocked || recovery.TaskStatus != "blocked" || recovery.TaskID == "" || seen[recovery.TaskID] {
			continue
		}
		seen[recovery.TaskID] = true
		action, err := build(ActionAssessResultRepair, "result_collection_assessment_required", ActionAvailabilityAvailable, recovery)
		if err != nil {
			return nil, err
		}
		action.CWD = snapshot.Scope.ProjectDir
		secondary = append(secondary, action)
	}
	return secondary, nil
}

func isTerminalReplan(facts ActionSelectionFacts) bool {
	state := facts.Snapshot.Activity.State
	return facts.Recovery != nil && facts.Recovery.ResumeEligible && facts.Recovery.ReasonCode == "recovery_replan" &&
		(state == ActivityFinished || state == ActivityBlocked)
}

func isFinishedRecovery(snapshot OperatorSnapshot, recovery *RecoveryEligibility) bool {
	return snapshot.Activity.State == ActivityFinished && recovery != nil && recovery.TaskID != ""
}

func appendRecoveryMutationAction(
	actions []ActionSuggestion,
	recovery *RecoveryEligibility,
	build func(string, string, string, *RecoveryEligibility) (ActionSuggestion, error),
	priorErr error,
) ([]ActionSuggestion, error) {
	if priorErr != nil {
		return actions, priorErr
	}
	var id, reason string
	switch {
	case recovery != nil && recovery.ReconcileEligible:
		id, reason = ActionReconcileTask, "reconcile_eligible"
	case recovery != nil && recovery.RetryEligible:
		id, reason = ActionRetryTask, "retry_eligible"
	default:
		return actions, nil
	}
	action, err := build(id, reason, ActionAvailabilityAvailable, recovery)
	if err != nil {
		return actions, err
	}
	return append(actions, action), nil
}

func inspectionAvailability(recovery *RecoveryEligibility) string {
	if recovery != nil && recovery.TaskID != "" {
		return ActionAvailabilityAvailable
	}
	return ActionAvailabilityBlocked
}

func actionTargetFromSnapshot(snapshot OperatorSnapshot) ActionTarget {
	return ActionTarget{
		Workspace: snapshot.Scope.WorkspaceExact, ProjectID: snapshot.Scope.ProjectID,
		TeamID: snapshot.Scope.TeamName, SessionID: snapshot.Scope.SessionID,
		RunID: snapshot.Scope.RunID, BranchID: snapshot.Scope.BranchID,
	}
}
