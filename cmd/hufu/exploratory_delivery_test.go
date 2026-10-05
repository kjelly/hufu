package main

import (
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func deliveredExploratoryFixture() (*teamContext, *team.RunResult) {
	result := &team.RunResult{
		Outcome:       team.RunOutcomeUnverified,
		GoalMode:      team.GoalModeExploratory,
		StopReason:    team.StopReasonAcceptanceNotSet,
		ExitCode:      7,
		Response:      "research answer",
		Acceptance:    &team.AcceptanceResult{State: team.AcceptanceNotConfigured},
		Stats:         team.RunStats{TasksTotal: 1, TasksDone: 1},
		GoalSatisfied: false,
	}
	coordinator := &team.Coordinator{}
	coordinator.SetLastRunResult(result)
	return &teamContext{coordinator: coordinator}, result
}

func TestExploratoryDeliveryModeExcludesMachineAndUnattendedOutput(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts = runOptions{}
	if !interactiveExploratoryDeliveryModeWithTerminal(true) {
		t.Fatal("terminal text mode should permit exploratory delivery")
	}
	if interactiveExploratoryDeliveryModeWithTerminal(false) {
		t.Fatal("non-terminal invocation must retain non-zero exit")
	}
	for name, change := range map[string]func(){
		"unattended": func() { opts.unattended = true },
		"quiet":      func() { opts.quietMode = true },
		"json":       func() { opts.outputFormat = "json" },
		"jsonl":      func() { opts.eventFormat = "jsonl" },
		"decision":   func() { opts.intent = "decision" },
	} {
		t.Run(name, func(t *testing.T) {
			opts = runOptions{}
			change()
			if interactiveExploratoryDeliveryModeWithTerminal(true) {
				t.Fatal("machine-oriented mode must retain non-zero exit")
			}
		})
	}
}

func TestExploratoryDeliveryRequiresAnInteractiveCompletedResponse(t *testing.T) {
	tc, result := deliveredExploratoryFixture()
	if !isDeliveredExploratoryResult(tc, result, true) {
		t.Fatal("completed exploratory response should be deliverable interactively")
	}
	for name, change := range map[string]func(*team.RunResult){
		"outcome mode":       func(r *team.RunResult) { r.GoalMode = team.GoalModeOutcome },
		"failed acceptance":  func(r *team.RunResult) { r.Acceptance.State = team.AcceptanceFailed },
		"unresolved task":    func(r *team.RunResult) { r.Stats.TasksUnresolved = 1 },
		"unfinished task":    func(r *team.RunResult) { r.Stats.TasksDone = 0 },
		"empty response":     func(r *team.RunResult) { r.Response = " " },
		"wrong stop reason":  func(r *team.RunResult) { r.StopReason = team.StopReasonBudgetExceeded },
		"goal marked as met": func(r *team.RunResult) { r.GoalSatisfied = true },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *result
			acceptance := *result.Acceptance
			copy.Acceptance = &acceptance
			change(&copy)
			if isDeliveredExploratoryResult(tc, &copy, true) {
				t.Fatal("non-deliverable run was treated as an exploratory response")
			}
		})
	}
	if isDeliveredExploratoryResult(tc, result, false) {
		t.Fatal("non-interactive output must retain the non-zero result")
	}
	tc.coordinator.SetExecutionProfile(team.BuiltinProfiles()[team.ProfileStrictVerification])
	if isDeliveredExploratoryResult(tc, result, true) {
		t.Fatal("strict profile must retain the non-zero result")
	}
}

func TestPresentExploratoryDeliveryKeepsCanonicalResult(t *testing.T) {
	tc, result := deliveredExploratoryFixture()
	cause := errors.New("run unverified: acceptance_not_configured")
	err := team.WrapRunOutcomeError(cause, result)
	response, gotErr := presentExploratoryDeliveryWithMode(tc, result.Response, err, true)
	if gotErr != nil || response != result.Response {
		t.Fatalf("delivered response = %q, err = %v", response, gotErr)
	}
	if result.Outcome != team.RunOutcomeUnverified || result.GoalSatisfied || result.ExitCode != 7 {
		t.Fatalf("canonical result changed: %#v", result)
	}
	_, gotErr = presentExploratoryDeliveryWithMode(tc, result.Response, err, false)
	if !errors.Is(gotErr, cause) {
		t.Fatalf("non-interactive error = %v, want original cause", gotErr)
	}
	other := *result
	_, gotErr = presentExploratoryDeliveryWithMode(tc, result.Response, team.WrapRunOutcomeError(cause, &other), true)
	if !errors.Is(gotErr, cause) {
		t.Fatalf("unrelated result error = %v, want original cause", gotErr)
	}
}

func TestExploratoryInvocationExitRequiresEveryCurrentRunDelivered(t *testing.T) {
	tc, result := deliveredExploratoryFixture()
	loaded := map[string]*teamContext{"research": tc}
	if !invocationDeliveredExploratoryResponseWithMode(loaded, nil, result, true) {
		t.Fatal("current exploratory response should permit interactive delivery")
	}
	if invocationDeliveredExploratoryResponseWithMode(loaded, nil, result, false) {
		t.Fatal("machine-oriented invocation must retain non-zero exit")
	}
	if invocationDeliveredExploratoryResponseWithMode(loaded, map[string]*team.RunResult{"research": result}, result, true) {
		t.Fatal("historical result must not count as this invocation's delivery")
	}
	failed := &team.RunResult{Outcome: team.RunOutcomePartial}
	other := &team.Coordinator{}
	other.SetLastRunResult(failed)
	loaded["other"] = &teamContext{coordinator: other}
	if invocationDeliveredExploratoryResponseWithMode(loaded, nil, result, true) {
		t.Fatal("another team's failure must retain non-zero exit")
	}
}
