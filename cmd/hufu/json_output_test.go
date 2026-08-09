package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestMultiTeamJSONOutputAggregation(t *testing.T) {
	// Test 2 teams in both lexical orders:
	// Team A (partial due to acceptance failure) + Team B (completed)
	// Team B (completed) + Team A (partial due to acceptance failure)

	runTest := func(t *testing.T, team1Name, team1OutcomeStr, team2Name, team2OutcomeStr string) {
		t.Helper()
		tc1 := &teamContext{
			teamName:    team1Name,
			coordinator: &team.Coordinator{},
		}
		tc1.coordinator.SetLastRunResult(&team.RunResult{
			Outcome:       team.RunOutcome(team1OutcomeStr),
			GoalSatisfied: team1OutcomeStr == "completed",
			Acceptance:    &team.AcceptanceResult{Passed: team1OutcomeStr == "completed"},
		})

		tc2 := &teamContext{
			teamName:    team2Name,
			coordinator: &team.Coordinator{},
		}
		tc2.coordinator.SetLastRunResult(&team.RunResult{
			Outcome:       team.RunOutcome(team2OutcomeStr),
			GoalSatisfied: team2OutcomeStr == "completed",
			Acceptance:    &team.AcceptanceResult{Passed: team2OutcomeStr == "completed"},
		})

		loaded := map[string]*teamContext{
			team1Name: tc1,
			team2Name: tc2,
		}

		oldStdout := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe failed: %v", err)
		}
		os.Stdout = w

		err = printResultJSON("multi-team result", loaded, nil)
		w.Close()
		os.Stdout = oldStdout

		if err != nil {
			t.Fatalf("printResultJSON failed: %v", err)
		}

		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)

		var out jsonRunOutput
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
			t.Fatalf("json.Unmarshal failed: %v, raw: %s", err, buf.String())
		}

		if out.Outcome != "partial" {
			t.Errorf("aggregated Outcome = %s, want partial", out.Outcome)
		}
		if out.GoalSatisfied != false {
			t.Errorf("aggregated GoalSatisfied = %v, want false", out.GoalSatisfied)
		}
	}

	t.Run("team-a partial, team-b completed", func(t *testing.T) {
		runTest(t, "team-a", "partial", "team-b", "completed")
	})

	t.Run("team-a completed, team-b partial", func(t *testing.T) {
		runTest(t, "team-a", "completed", "team-b", "partial")
	})
}

func TestJSONOutputDoesNotReportAbortedRunAsCompleted(t *testing.T) {
	tc := &teamContext{teamName: "aborted", coordinator: &team.Coordinator{}}
	tc.coordinator.SetLastRunResult(&team.RunResult{
		Outcome:       team.RunOutcomeCancelled,
		GoalSatisfied: false,
		Reason:        "run aborted (cancelled by user)",
		ExitCode:      130,
	})
	loaded := map[string]*teamContext{"aborted": tc}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = printResultJSON("", loaded, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRunOutput
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != "cancelled" || out.GoalSatisfied || out.ExitCode != 130 || out.Reason == "" {
		t.Fatalf("aborted JSON output = %#v", out)
	}
}

func TestJSONOutputPreservesAcceptanceNotConfigured(t *testing.T) {
	tc := &teamContext{teamName: "no-gate", coordinator: &team.Coordinator{}}
	tc.coordinator.SetLastRunResult(&team.RunResult{
		Outcome:       team.RunOutcomeUnverified,
		GoalSatisfied: false,
		GoalMode:      team.GoalModeOutcome,
		StopReason:    team.StopReasonAcceptanceNotSet,
		Acceptance:    &team.AcceptanceResult{State: team.AcceptanceNotConfigured},
	})

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = printResultJSON("done", map[string]*teamContext{"no-gate": tc}, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRunOutput
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Acceptance == nil || out.Acceptance.State != team.AcceptanceNotConfigured || out.Acceptance.Passed {
		t.Fatalf("acceptance output = %#v, want not_configured and not passed", out.Acceptance)
	}
	if out.GoalMode != "outcome" || out.StopReason != "acceptance_not_configured" || out.Outcome != "unverified" || out.GoalSatisfied {
		t.Fatalf("JSON output = %#v, want outcome/acceptance_not_configured/unverified/unsatisfied", out)
	}
}

func TestCanonicalNonSuccessfulRunResultIgnoresRestoredHistoricalResult(t *testing.T) {
	coordinator := &team.Coordinator{}
	historical := &team.RunResult{Outcome: team.RunOutcomePartial, ExitCode: 7}
	coordinator.SetLastRunResult(historical)
	tc := &teamContext{teamName: "restored", coordinator: coordinator}
	loaded := map[string]*teamContext{"restored": tc}
	if got := canonicalNonSuccessfulRunResult(loaded, map[string]*team.RunResult{"restored": historical}); got != nil {
		t.Fatalf("historical result selected: %#v", got)
	}

	current := &team.RunResult{Outcome: team.RunOutcomePartial, ExitCode: 7}
	coordinator.SetLastRunResult(current)
	got := canonicalNonSuccessfulRunResult(loaded, map[string]*team.RunResult{"restored": historical})
	if got == nil || got.Outcome != current.Outcome || got.ExitCode != current.ExitCode {
		t.Fatalf("current result = %#v, want outcome=%q exit=%d", got, current.Outcome, current.ExitCode)
	}
}

func TestCanonicalNonSuccessfulRunResultDelegatesExitCodeSelection(t *testing.T) {
	failed := &team.RunResult{Outcome: team.RunOutcomeFailed, ExitCode: 1}
	partial := &team.RunResult{Outcome: team.RunOutcomePartial, ExitCode: 7}
	loaded := map[string]*teamContext{
		"failed":  {teamName: "failed", coordinator: &team.Coordinator{}},
		"partial": {teamName: "partial", coordinator: &team.Coordinator{}},
	}
	loaded["failed"].coordinator.SetLastRunResult(failed)
	loaded["partial"].coordinator.SetLastRunResult(partial)

	got := canonicalNonSuccessfulRunResult(loaded, nil)
	if got == nil || got.Outcome != team.RunOutcomeFailed || got.ExitCode != 1 {
		t.Fatalf("canonical result = %#v, want failed/1", got)
	}
}

func TestJSONOutputIgnoresHistoricalUnresolvedTasks(t *testing.T) {
	tc := &teamContext{teamName: "resumed", coordinator: &team.Coordinator{}}
	tc.coordinator.SetLastRunResult(&team.RunResult{
		Outcome:       team.RunOutcomeCompleted,
		GoalSatisfied: true,
		Acceptance:    &team.AcceptanceResult{State: team.AcceptancePassed},
	})
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = printResultJSONWithPrior("done", map[string]*teamContext{"resumed": tc}, nil, map[string]map[string]time.Time{
		"resumed": {"old": time.Time{}},
	})
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRunOutput
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Outcome != string(team.RunOutcomeCompleted) || !out.GoalSatisfied {
		t.Fatalf("JSON output = %#v, want completed/satisfied", out)
	}
}

func TestJSONOutputDoesNotDoubleCountStats(t *testing.T) {
	// Construct tracked Todo items (1 done, 1 error with 1 retry)
	// SummarizeRunStats will derive non-zero caller stats:
	// TasksTotal: 2, TasksDone: 1, TasksUnresolved: 1, AttemptsTotal: 3, AttemptsFailed: 2
	tracker := team.NewTaskTracker()
	added := tracker.TodoList().AddBatch([]team.TodoSpec{
		{Agent: "worker-1", Desc: "done task"},
		{Agent: "worker-2", Desc: "error task"},
	})
	added[0].Status = team.TaskDone
	added[1].Status = team.TaskError
	added[1].Retries = 1

	expectedStats := team.SummarizeRunStats(tracker.TodoList().Items())
	if expectedStats.TasksTotal != 2 || expectedStats.TasksDone != 1 || expectedStats.TasksUnresolved != 1 || expectedStats.AttemptsTotal != 3 || expectedStats.AttemptsFailed != 2 {
		t.Fatalf("unexpected fixture stats: %#v", expectedStats)
	}

	coord := &team.Coordinator{}
	coord.SetTaskTracker(tracker)
	coord.SetLastRunResult(&team.RunResult{
		Outcome:       team.RunOutcomePartial,
		GoalSatisfied: false,
		Acceptance:    &team.AcceptanceResult{State: team.AcceptanceNotConfigured},
		Stats:         expectedStats,
	})

	tc := &teamContext{teamName: "dev", coordinator: coord}
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = printResultJSON("partial", map[string]*teamContext{"dev": tc}, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRunOutput
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}

	// Assert that ALL 5 public JSON stats fields match expectedStats exactly once, NOT double-counted
	if out.Stats.TasksTotal != 2 {
		t.Errorf("out.Stats.TasksTotal = %d, want 2 (must not double-count to 4)", out.Stats.TasksTotal)
	}
	if out.Stats.TasksDone != 1 {
		t.Errorf("out.Stats.TasksDone = %d, want 1 (must not double-count to 2)", out.Stats.TasksDone)
	}
	if out.Stats.TasksUnresolved != 1 {
		t.Errorf("out.Stats.TasksUnresolved = %d, want 1 (must not double-count to 2)", out.Stats.TasksUnresolved)
	}
	if out.Stats.AttemptsTotal != 3 {
		t.Errorf("out.Stats.AttemptsTotal = %d, want 3 (must not double-count to 6)", out.Stats.AttemptsTotal)
	}
	if out.Stats.AttemptsFailed != 2 {
		t.Errorf("out.Stats.AttemptsFailed = %d, want 2 (must not double-count to 4)", out.Stats.AttemptsFailed)
	}
}
