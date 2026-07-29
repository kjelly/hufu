package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/anomalyco/hufu/internal/team"
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
