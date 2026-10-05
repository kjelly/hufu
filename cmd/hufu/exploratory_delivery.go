package main

import (
	"errors"
	"os"
	"strings"

	"github.com/charmbracelet/x/term"
	"github.com/kjelly/hufu/internal/team"
)

// Interactive exploratory output is a delivered response, not proof that the
// user's goal was met. Machine-oriented and unattended invocations keep the
// canonical non-zero exit contract for unverified runs.
func interactiveExploratoryDeliveryMode() bool {
	return interactiveExploratoryDeliveryModeWithTerminal(term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd()))
}

func interactiveExploratoryDeliveryModeWithTerminal(terminal bool) bool {
	if opts.unattended || opts.quietMode || opts.outputFormat == "json" || opts.eventFormat == "jsonl" || opts.intent == "decision" {
		return false
	}
	return terminal
}

func isDeliveredExploratoryResult(tc *teamContext, result *team.RunResult, interactive bool) bool {
	if !interactive || tc == nil || tc.coordinator == nil || tc.coordinator.IsUnattended() || result == nil {
		return false
	}
	profile := tc.coordinator.ExecutionProfile()
	if profile.StrictPolicy || profile.AcceptanceMode != team.AcceptanceAdvisory {
		return false
	}
	return result.Outcome == team.RunOutcomeUnverified &&
		result.GoalMode == team.GoalModeExploratory &&
		result.StopReason == team.StopReasonAcceptanceNotSet &&
		!result.GoalSatisfied &&
		result.Acceptance != nil &&
		result.Acceptance.EffectiveState() == team.AcceptanceNotConfigured &&
		strings.TrimSpace(result.Response) != "" &&
		result.Stats.TasksUnresolved == 0 &&
		result.Stats.TasksDone == result.Stats.TasksTotal &&
		len(result.UnresolvedTasks) == 0
}

func presentExploratoryDelivery(tc *teamContext, response string, err error) (string, error) {
	return presentExploratoryDeliveryWithMode(tc, response, err, interactiveExploratoryDeliveryMode())
}

func presentExploratoryDeliveryWithMode(tc *teamContext, response string, err error, interactive bool) (string, error) {
	if err == nil || strings.TrimSpace(response) == "" {
		return response, err
	}
	outcomeErr, ok := errors.AsType[*team.RunOutcomeError](err)
	if !ok || outcomeErr.Result == nil || tc == nil || tc.coordinator == nil || outcomeErr.Result != tc.coordinator.LastRunResult() {
		return response, err
	}
	if !isDeliveredExploratoryResult(tc, outcomeErr.Result, interactive) {
		return response, err
	}
	if !opts.tuiMode {
		stderrLog("\n⚠ Exploratory response delivered; goal remains unverified (no acceptance configured).\n")
	}
	return response, nil
}

func invocationDeliveredExploratoryResponse(loadedTeams map[string]*teamContext, priorResults map[string]*team.RunResult, outcome *team.RunResult) bool {
	return invocationDeliveredExploratoryResponseWithMode(loadedTeams, priorResults, outcome, interactiveExploratoryDeliveryMode())
}

func invocationDeliveredExploratoryResponseWithMode(loadedTeams map[string]*teamContext, priorResults map[string]*team.RunResult, outcome *team.RunResult, interactive bool) bool {
	if outcome == nil || outcome.Outcome != team.RunOutcomeUnverified || !interactive {
		return false
	}
	observed := false
	for name, tc := range loadedTeams {
		if tc == nil || tc.coordinator == nil {
			continue
		}
		result := tc.coordinator.LastRunResult()
		if result == nil || result == priorResults[name] {
			continue
		}
		observed = true
		if !isDeliveredExploratoryResult(tc, result, true) {
			return false
		}
	}
	return observed
}
