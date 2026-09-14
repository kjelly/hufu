package operator

import (
	"slices"
	"strings"
)

type ActivityFacts struct {
	BindingStatus               string
	EventChain                  string
	RequiredProjection          string
	HasTerminalRun              bool
	PendingInput                bool
	PendingApproval             bool
	WrapUpAccepted              bool
	DurablyInterrupted          bool
	TaskStates                  []string
	HasRunnableDependencyRepair bool
	PreflightRunning            bool
	PreflightSucceeded          bool
	MissingRequiredConfig       bool
	RawReasonCodes              []string
}

func DeriveActivity(facts ActivityFacts) ActivityView {
	states := cloneSorted(facts.TaskStates)
	reasons := cloneSorted(facts.RawReasonCodes)
	view := ActivityView{State: ActivityUnknown, RawTaskStates: states, RawReasonCodes: reasons}
	if invalidActivityFacts(facts) {
		return view
	}
	if facts.HasTerminalRun {
		view.State = ActivityFinished
		return view
	}
	if facts.PendingInput {
		view.State = ActivityWaitingInput
		return view
	}
	if facts.PendingApproval {
		view.State = ActivityWaitingApproval
		return view
	}
	if facts.WrapUpAccepted {
		view.State = ActivityWrappingUp
		return view
	}
	if facts.DurablyInterrupted {
		view.State = ActivityInterrupted
		return view
	}
	if hasAny(states, "blocked", "error", "protocol_incomplete") && !facts.HasRunnableDependencyRepair {
		view.State = ActivityBlocked
		return view
	}
	if slices.Contains(states, "verifying") {
		view.State = ActivityVerifying
		return view
	}
	if slices.Contains(states, "in_progress") {
		view.State = ActivityExecuting
		return view
	}
	if slices.Contains(states, "planned") {
		view.State = ActivityPlanning
		return view
	}
	if facts.PreflightRunning {
		view.State = ActivityPreflight
		return view
	}
	if facts.PreflightSucceeded {
		view.State = ActivityReady
		return view
	}
	if facts.MissingRequiredConfig {
		view.State = ActivityUnconfigured
	}
	return view
}

func DeriveAttention(activity, runOutcome, integrityStatus, externalEffectState string) string {
	if integrityStatus == "invalid" || integrityStatus == "unknown" {
		return AttentionHumanRequired
	}
	if externalEffectState == "unknown" {
		return AttentionReviewRequired
	}
	switch activity {
	case ActivityWaitingInput:
		return AttentionHumanRequired
	case ActivityWaitingApproval, ActivityBlocked:
		return AttentionReviewRequired
	case ActivityInterrupted:
		return AttentionActionAvailable
	case ActivityPreflight, ActivityPlanning, ActivityExecuting, ActivityVerifying, ActivityWrappingUp:
		return AttentionInformational
	case ActivityFinished:
		if runOutcome == "completed" {
			return AttentionNone
		}
		return AttentionReviewRequired
	case ActivityReady:
		return AttentionActionAvailable
	case ActivityUnconfigured:
		return AttentionActionAvailable
	default:
		return AttentionUnknown
	}
}

func invalidActivityFacts(facts ActivityFacts) bool {
	if facts.BindingStatus == "ambiguous" || facts.BindingStatus == "conflict" || facts.BindingStatus == "unknown" {
		return true
	}
	if facts.EventChain != "" && facts.EventChain != "verified" {
		return true
	}
	if facts.RequiredProjection != "" && facts.RequiredProjection != "consistent" {
		return true
	}
	for _, status := range facts.TaskStates {
		switch strings.TrimSpace(status) {
		case "pending", "planned", "in_progress", "paused", "verifying", "done", "error", "blocked", "skipped", "protocol_incomplete":
		default:
			return true
		}
	}
	return false
}

func hasAny(values []string, candidates ...string) bool {
	for _, candidate := range candidates {
		if slices.Contains(values, candidate) {
			return true
		}
	}
	return false
}

func cloneSorted(values []string) []string {
	result := slices.Clone(values)
	if result == nil {
		result = []string{}
	}
	slices.Sort(result)
	return result
}
