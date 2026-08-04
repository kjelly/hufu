package team

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestEvaluateRunOutcome(t *testing.T) {
	tests := []struct {
		name       string
		goalMode   GoalMode
		acceptance AcceptanceState
		unresolved []TaskReference
		cancelled  bool
		budget     bool
		runFailed  bool
		outcome    RunOutcome
		goal       bool
		exitCode   int
	}{
		{name: "outcome mode without configured acceptance is unverified", goalMode: GoalModeOutcome, outcome: RunOutcomeUnverified, goal: false, exitCode: 7},
		{name: "exploratory mode without configured acceptance is unverified", goalMode: GoalModeExploratory, outcome: RunOutcomeUnverified, goal: false, exitCode: 7},
		{name: "acceptance passed", acceptance: AcceptancePassed, outcome: RunOutcomeCompleted, goal: true, exitCode: 0},
		{name: "acceptance failed", acceptance: AcceptanceFailed, outcome: RunOutcomePartial, exitCode: 7},
		{name: "pending task", unresolved: []TaskReference{{ID: "1", Status: string(TaskPending)}}, outcome: RunOutcomePartial, exitCode: 7},
		{name: "failed task", unresolved: []TaskReference{{ID: "1", Status: string(TaskError)}}, outcome: RunOutcomePartial, exitCode: 7},
		{name: "blocked task", unresolved: []TaskReference{{ID: "1", Status: string(TaskBlocked)}}, outcome: RunOutcomeBlocked, exitCode: 7},
		{name: "budget exceeded", budget: true, outcome: RunOutcomePartial, exitCode: 7},
		{name: "run failure", runFailed: true, outcome: RunOutcomeFailed, exitCode: 1},
		{name: "cancelled", cancelled: true, outcome: RunOutcomeCancelled, exitCode: 130},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateRunOutcome(RunEvaluationInput{
				GoalMode:        tt.goalMode,
				Acceptance:      tt.acceptance,
				UnresolvedTasks: tt.unresolved,
				Cancelled:       tt.cancelled,
				BudgetExceeded:  tt.budget,
				RunFailed:       tt.runFailed,
			})
			if got.Outcome != tt.outcome || got.GoalSatisfied != tt.goal || got.ExitCode != tt.exitCode {
				t.Fatalf("evaluation = outcome=%q goal=%t exit=%d, want outcome=%q goal=%t exit=%d", got.Outcome, got.GoalSatisfied, got.ExitCode, tt.outcome, tt.goal, tt.exitCode)
			}
			expectedAcceptance := tt.acceptance
			if expectedAcceptance == "" {
				expectedAcceptance = AcceptanceNotConfigured
			}
			if got.Acceptance == nil || got.Acceptance.State != expectedAcceptance {
				t.Fatalf("evaluation did not preserve acceptance state: %#v", got.Acceptance)
			}
		})
	}
}

func TestEvaluateRunOutcomeGoalModes(t *testing.T) {
	tests := []struct {
		name       string
		goalMode   GoalMode
		acceptance AcceptanceState
		unresolved []TaskReference
		cancelled  bool
		budget     bool
		runFailed  bool
		outcome    RunOutcome
		goal       bool
		stopReason StopReason
		exitCode   int
	}{
		{
			name:       "outcome mode without acceptance is unverified",
			goalMode:   GoalModeOutcome,
			acceptance: AcceptanceNotConfigured,
			outcome:    RunOutcomeUnverified,
			goal:       false,
			stopReason: StopReasonAcceptanceNotSet,
			exitCode:   7,
		},
		{
			name:       "outcome mode with passed acceptance is completed",
			goalMode:   GoalModeOutcome,
			acceptance: AcceptancePassed,
			outcome:    RunOutcomeCompleted,
			goal:       true,
			stopReason: StopReasonCompleted,
			exitCode:   0,
		},
		{
			name:       "exploratory mode without acceptance is unverified",
			goalMode:   GoalModeExploratory,
			acceptance: AcceptanceNotConfigured,
			outcome:    RunOutcomeUnverified,
			goal:       false,
			stopReason: StopReasonAcceptanceNotSet,
			exitCode:   7,
		},
		{
			name:       "exploratory mode with passed acceptance is completed with satisfied goal",
			goalMode:   GoalModeExploratory,
			acceptance: AcceptancePassed,
			outcome:    RunOutcomeCompleted,
			goal:       true,
			stopReason: StopReasonCompleted,
			exitCode:   0,
		},
		{
			name:       "acceptance failed in outcome mode",
			goalMode:   GoalModeOutcome,
			acceptance: AcceptanceFailed,
			outcome:    RunOutcomePartial,
			goal:       false,
			stopReason: StopReasonAcceptanceFailed,
			exitCode:   7,
		},
		{
			name:       "pending task produces unresolved_tasks stop reason",
			goalMode:   GoalModeOutcome,
			unresolved: []TaskReference{{ID: "1", Status: string(TaskPending)}},
			outcome:    RunOutcomePartial,
			goal:       false,
			stopReason: StopReasonUnresolvedTasks,
			exitCode:   7,
		},
		{
			name:       "blocked task produces external_blockage stop reason",
			goalMode:   GoalModeOutcome,
			unresolved: []TaskReference{{ID: "1", Status: string(TaskBlocked)}},
			outcome:    RunOutcomeBlocked,
			goal:       false,
			stopReason: StopReasonExternalBlockage,
			exitCode:   7,
		},
		{
			name:       "budget exceeded produces budget_exceeded stop reason",
			goalMode:   GoalModeOutcome,
			budget:     true,
			outcome:    RunOutcomePartial,
			goal:       false,
			stopReason: StopReasonBudgetExceeded,
			exitCode:   7,
		},
		{
			name:       "cancelled produces cancelled stop reason",
			goalMode:   GoalModeOutcome,
			cancelled:  true,
			outcome:    RunOutcomeCancelled,
			goal:       false,
			stopReason: StopReasonCancelled,
			exitCode:   130,
		},
		{
			name:       "run failure produces run_failed stop reason",
			goalMode:   GoalModeOutcome,
			runFailed:  true,
			outcome:    RunOutcomeFailed,
			goal:       false,
			stopReason: StopReasonRunFailed,
			exitCode:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EvaluateRunOutcome(RunEvaluationInput{
				GoalMode:        tt.goalMode,
				Acceptance:      tt.acceptance,
				UnresolvedTasks: tt.unresolved,
				Cancelled:       tt.cancelled,
				BudgetExceeded:  tt.budget,
				RunFailed:       tt.runFailed,
			})
			if got.Outcome != tt.outcome || got.GoalSatisfied != tt.goal || got.ExitCode != tt.exitCode || got.StopReason != tt.stopReason {
				t.Fatalf("evaluation = outcome=%q goal=%t exit=%d stopReason=%q, want outcome=%q goal=%t exit=%d stopReason=%q",
					got.Outcome, got.GoalSatisfied, got.ExitCode, got.StopReason, tt.outcome, tt.goal, tt.exitCode, tt.stopReason)
			}
		})
	}
}

func TestAcceptanceNotConfiguredIsNotPassed(t *testing.T) {
	result := &AcceptanceResult{State: AcceptanceNotConfigured}
	if result.State == AcceptancePassed || result.Passed {
		t.Fatalf("not-configured acceptance must not be represented as passed: %#v", result)
	}
}

func TestAcceptanceSerializationCanonicalizesPassedBit(t *testing.T) {
	raw, err := json.Marshal(AcceptanceResult{State: AcceptanceNotConfigured, Passed: true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded AcceptanceResult
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != AcceptanceNotConfigured || decoded.Passed {
		t.Fatalf("serialized acceptance = %#v, want not_configured/not-passed", decoded)
	}
	if (AcceptanceResult{State: AcceptanceNotConfigured, Passed: true}).IsPassed() {
		t.Fatal("IsPassed must derive from state, not stale Passed bit")
	}
}

func TestRunOutcomeErrorPreservesCanonicalExitCode(t *testing.T) {
	cause := errors.New("acceptance failed")
	result := &RunResult{Outcome: RunOutcomePartial, ExitCode: 7}
	err := WrapRunOutcomeError(cause, result)
	var outcomeErr *RunOutcomeError
	if !errors.As(err, &outcomeErr) {
		t.Fatalf("error = %T, want RunOutcomeError", err)
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped outcome error did not preserve cause")
	}
	if got := outcomeErr.ProcessExitCode(); got != 7 {
		t.Fatalf("ProcessExitCode() = %d, want 7", got)
	}
}

func TestRunResultStatusDataIncludesAcceptanceState(t *testing.T) {
	data := runResultStatusData(&RunResult{
		Outcome:       RunOutcomeCompleted,
		GoalSatisfied: true,
		ExitCode:      0,
		Acceptance:    &AcceptanceResult{State: AcceptanceNotConfigured},
	})
	if got, want := data["acceptance_state"], AcceptanceNotConfigured; got != want {
		t.Fatalf("acceptance_state = %#v, want %q", got, want)
	}
	if got := data["acceptance_passed"]; got == true {
		t.Fatal("not_configured acceptance must not be reported as passed")
	}
	if got, want := data["exit_code"], 0; got != want {
		t.Fatalf("exit_code = %#v, want %d", got, want)
	}
}

func TestAggregateRunResultsUsesCanonicalPrecedence(t *testing.T) {
	tests := []struct {
		name       string
		results    []*RunResult
		unresolved []TaskReference
		outcome    RunOutcome
		goal       bool
	}{
		{name: "cancelled wins over failed", results: []*RunResult{{Outcome: RunOutcomeFailed, ExitCode: 1}, {Outcome: RunOutcomeCancelled, ExitCode: 130}}, outcome: RunOutcomeCancelled},
		{name: "blocked unresolved wins over budget partial", results: []*RunResult{{Outcome: RunOutcomePartial}}, unresolved: []TaskReference{{ID: "t1", Status: string(TaskBlocked)}}, outcome: RunOutcomeBlocked},
		{name: "persisted blocked result fails closed", results: []*RunResult{{Outcome: RunOutcomeBlocked}}, outcome: RunOutcomeBlocked},
		{name: "failed acceptance remains visible", results: []*RunResult{{Outcome: RunOutcomePartial, Acceptance: &AcceptanceResult{State: AcceptanceFailed}}}, outcome: RunOutcomePartial},
		{name: "all successful with passed gate", results: []*RunResult{{Outcome: RunOutcomeCompleted, Acceptance: &AcceptanceResult{State: AcceptancePassed}}}, outcome: RunOutcomeCompleted, goal: true},
		{name: "all successful without gate in outcome mode", results: []*RunResult{{Outcome: RunOutcomeCompleted, Acceptance: &AcceptanceResult{State: AcceptanceNotConfigured}}}, outcome: RunOutcomeUnverified, goal: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AggregateRunResults(tt.results, tt.unresolved, RunStats{})
			if got.Outcome != tt.outcome || got.GoalSatisfied != tt.goal {
				t.Fatalf("aggregate = outcome=%q goal=%t, want outcome=%q goal=%t", got.Outcome, got.GoalSatisfied, tt.outcome, tt.goal)
			}
		})
	}
}

func TestAggregateRunResultsMixedGoalModesAndUnresolvedFolding(t *testing.T) {
	t.Run("outcome mode wins over exploratory mode in aggregation regardless of order", func(t *testing.T) {
		resExploratory := &RunResult{GoalMode: GoalModeExploratory, Outcome: RunOutcomeCompleted}
		resOutcome := &RunResult{GoalMode: GoalModeOutcome, Outcome: RunOutcomeCompleted, Acceptance: &AcceptanceResult{State: AcceptancePassed}}

		// Order 1: exploratory first
		got1 := AggregateRunResults([]*RunResult{resExploratory, resOutcome}, nil, RunStats{})
		if got1.GoalMode != GoalModeOutcome {
			t.Errorf("got1.GoalMode = %q, want %q", got1.GoalMode, GoalModeOutcome)
		}

		// Order 2: outcome first
		got2 := AggregateRunResults([]*RunResult{resOutcome, resExploratory}, nil, RunStats{})
		if got2.GoalMode != GoalModeOutcome {
			t.Errorf("got2.GoalMode = %q, want %q", got2.GoalMode, GoalModeOutcome)
		}
	})

	t.Run("preserves unresolved tasks from team results instead of classifying as budget_exceeded", func(t *testing.T) {
		partialResult := &RunResult{
			Outcome:         RunOutcomePartial,
			StopReason:      StopReasonUnresolvedTasks,
			GoalMode:        GoalModeOutcome,
			UnresolvedTasks: []TaskReference{{ID: "t1", Status: "pending"}},
		}
		got := AggregateRunResults([]*RunResult{partialResult}, nil, RunStats{})
		if got.Outcome != RunOutcomePartial {
			t.Errorf("got.Outcome = %q, want %q", got.Outcome, RunOutcomePartial)
		}
		if got.StopReason != StopReasonUnresolvedTasks {
			t.Errorf("got.StopReason = %q, want %q", got.StopReason, StopReasonUnresolvedTasks)
		}
		if len(got.UnresolvedTasks) != 1 || got.UnresolvedTasks[0].ID != "t1" {
			t.Errorf("got.UnresolvedTasks = %v, want [{t1 pending}]", got.UnresolvedTasks)
		}
	})
}

func TestAggregateRunResultsDoesNotDoubleCountStats(t *testing.T) {
	t.Run("when caller provides non-zero stats, stats are preserved and not double-counted", func(t *testing.T) {
		result1 := &RunResult{
			Outcome:  RunOutcomeCompleted,
			Stats:    RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1},
			GoalMode: GoalModeOutcome,
		}
		callerStats := RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1}

		got := AggregateRunResults([]*RunResult{result1}, nil, callerStats)
		if got.Stats.TasksTotal != 1 {
			t.Errorf("got.Stats.TasksTotal = %d, want 1 (must not double-count)", got.Stats.TasksTotal)
		}
		if got.Stats.TasksDone != 1 {
			t.Errorf("got.Stats.TasksDone = %d, want 1", got.Stats.TasksDone)
		}
		if got.Stats.AttemptsTotal != 1 {
			t.Errorf("got.Stats.AttemptsTotal = %d, want 1", got.Stats.AttemptsTotal)
		}
	})

	t.Run("when caller provides non-zero stats with all 5 fields, stats are preserved without double-counting", func(t *testing.T) {
		result1 := &RunResult{
			Outcome:  RunOutcomePartial,
			Stats:    RunStats{TasksTotal: 2, TasksDone: 1, TasksUnresolved: 1, AttemptsTotal: 3, AttemptsFailed: 2},
			GoalMode: GoalModeOutcome,
		}
		callerStats := RunStats{TasksTotal: 2, TasksDone: 1, TasksUnresolved: 1, AttemptsTotal: 3, AttemptsFailed: 2}

		got := AggregateRunResults([]*RunResult{result1}, nil, callerStats)
		if got.Stats.TasksTotal != 2 {
			t.Errorf("got.Stats.TasksTotal = %d, want 2", got.Stats.TasksTotal)
		}
		if got.Stats.TasksDone != 1 {
			t.Errorf("got.Stats.TasksDone = %d, want 1", got.Stats.TasksDone)
		}
		if got.Stats.TasksUnresolved != 1 {
			t.Errorf("got.Stats.TasksUnresolved = %d, want 1", got.Stats.TasksUnresolved)
		}
		if got.Stats.AttemptsTotal != 3 {
			t.Errorf("got.Stats.AttemptsTotal = %d, want 3", got.Stats.AttemptsTotal)
		}
		if got.Stats.AttemptsFailed != 2 {
			t.Errorf("got.Stats.AttemptsFailed = %d, want 2", got.Stats.AttemptsFailed)
		}
	})

	t.Run("when caller provides zero stats, stats are folded from results", func(t *testing.T) {
		result1 := &RunResult{
			Outcome:  RunOutcomeCompleted,
			Stats:    RunStats{TasksTotal: 1, TasksDone: 1, AttemptsTotal: 1},
			GoalMode: GoalModeOutcome,
		}
		result2 := &RunResult{
			Outcome:  RunOutcomeCompleted,
			Stats:    RunStats{TasksTotal: 2, TasksDone: 2, AttemptsTotal: 3},
			GoalMode: GoalModeOutcome,
		}

		got := AggregateRunResults([]*RunResult{result1, result2}, nil, RunStats{})
		if got.Stats.TasksTotal != 3 {
			t.Errorf("got.Stats.TasksTotal = %d, want 3", got.Stats.TasksTotal)
		}
		if got.Stats.TasksDone != 3 {
			t.Errorf("got.Stats.TasksDone = %d, want 3", got.Stats.TasksDone)
		}
		if got.Stats.AttemptsTotal != 4 {
			t.Errorf("got.Stats.AttemptsTotal = %d, want 4", got.Stats.AttemptsTotal)
		}
	})
}

func TestAggregateRunResultsUsesWinningExitCode(t *testing.T) {
	got := AggregateRunResults([]*RunResult{
		{Outcome: RunOutcomeFailed, ExitCode: 1},
		{Outcome: RunOutcomePartial, ExitCode: 7},
	}, nil, RunStats{})
	if got.Outcome != RunOutcomeFailed || got.ExitCode != 1 {
		t.Fatalf("aggregate = outcome=%q exit=%d, want failed/1", got.Outcome, got.ExitCode)
	}

	got = AggregateRunResults([]*RunResult{
		{Outcome: RunOutcomeCancelled, ExitCode: 130},
		{Outcome: RunOutcomeFailed, ExitCode: 1},
	}, nil, RunStats{})
	if got.Outcome != RunOutcomeCancelled || got.ExitCode != 130 {
		t.Fatalf("aggregate = outcome=%q exit=%d, want cancelled/130", got.Outcome, got.ExitCode)
	}

	for _, results := range [][]*RunResult{
		{{Outcome: RunOutcomeFailed, ExitCode: 1}, {Outcome: RunOutcomeFailed, ExitCode: 2}},
		{{Outcome: RunOutcomeFailed, ExitCode: 2}, {Outcome: RunOutcomeFailed, ExitCode: 1}},
	} {
		got = AggregateRunResults(results, nil, RunStats{})
		if got.Outcome != RunOutcomeFailed || got.ExitCode != 2 {
			t.Fatalf("aggregate = outcome=%q exit=%d, want deterministic failed/2", got.Outcome, got.ExitCode)
		}
	}
}

func TestEvaluateRunOutcomeUnknownAcceptanceFailsClosed(t *testing.T) {
	got := EvaluateRunOutcome(RunEvaluationInput{Acceptance: AcceptanceState("unexpected")})
	if got.Outcome != RunOutcomePartial || got.GoalSatisfied || got.Acceptance == nil || got.Acceptance.State != AcceptanceFailed {
		t.Fatalf("evaluation = %#v, want partial, unsatisfied, failed acceptance", got)
	}
}

func TestUnresolvedTaskReferencesIncludesProtocolIncompleteAndSkipsResolvedFailures(t *testing.T) {
	items := []*TodoItem{
		{ID: "pending", Status: TaskPending, Agent: "worker"},
		{ID: "protocol", Status: TaskProtocolIncomplete, Agent: "worker"},
		{ID: "done", Status: TaskDone, Agent: "worker"},
		{ID: "resolved", Status: TaskError, Agent: "worker", Resolution: &TaskResolution{Status: "waived"}},
	}
	refs := UnresolvedTaskReferences(items)
	if len(refs) != 2 {
		t.Fatalf("unresolved refs = %#v, want pending and protocol-incomplete only", refs)
	}
	if refs[0].ID != "pending" || refs[1].ID != "protocol" {
		t.Fatalf("unresolved refs = %#v, want stable task order", refs)
	}
}

func TestParseGoalMode(t *testing.T) {
	tests := []struct {
		input    string
		expected GoalMode
		wantErr  bool
	}{
		{"outcome", GoalModeOutcome, false},
		{"exploratory", GoalModeExploratory, false},
		{"  EXPLORATORY ", GoalModeExploratory, false},
		{"", GoalModeOutcome, false},
		{"invalid", "", true},
		{"typo", "", true},
	}
	for _, tt := range tests {
		got, err := ParseGoalMode(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseGoalMode(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.expected {
			t.Errorf("ParseGoalMode(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestFormatCanonicalStatus(t *testing.T) {
	tests := []struct {
		name     string
		res      *RunResult
		expected string
	}{
		{"nil result", nil, "All tasks completed"},
		{"goal satisfied", &RunResult{GoalSatisfied: true, Outcome: RunOutcomeCompleted}, "Execution completed successfully"},
		{"exploratory completed", &RunResult{GoalSatisfied: false, Outcome: RunOutcomeCompleted, GoalMode: GoalModeExploratory}, "Execution completed; goal unverified"},
		{"outcome unverified", &RunResult{GoalSatisfied: false, Outcome: RunOutcomeUnverified, GoalMode: GoalModeOutcome}, "Execution completed; goal unverified (no acceptance configured)"},
		{"blocked", &RunResult{Outcome: RunOutcomeBlocked, StopReason: StopReasonExternalBlockage}, "Execution blocked"},
		{"budget exceeded", &RunResult{Outcome: RunOutcomePartial, StopReason: StopReasonBudgetExceeded}, "Budget exhausted"},
		{"acceptance failed", &RunResult{Outcome: RunOutcomePartial, StopReason: StopReasonAcceptanceFailed}, "Acceptance check failed"},
		{"cancelled", &RunResult{Outcome: RunOutcomeCancelled, StopReason: StopReasonCancelled}, "Execution cancelled"},
		{"failed", &RunResult{Outcome: RunOutcomeFailed, StopReason: StopReasonRunFailed}, "Execution failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCanonicalStatus(tt.res)
			if got != tt.expected {
				t.Errorf("FormatCanonicalStatus() = %q, want %q", got, tt.expected)
			}
		})
	}
}
