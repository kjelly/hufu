package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

type jsonOutputEventJournal struct {
	store *team.EventStore
}

func (j jsonOutputEventJournal) Append(ctx context.Context, event team.RunEvent) (team.RunEvent, error) {
	return j.store.AppendPersistedContext(ctx, event)
}

func (j jsonOutputEventJournal) ReadEvents(ctx context.Context) ([]team.RunEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return j.store.ReadEvents()
}

func (j jsonOutputEventJournal) VerifyHashChain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return j.store.VerifyHashChain()
}

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

func TestJSONOutputIncludesContentFreeContextRoutingAggregate(t *testing.T) {
	c := &team.Coordinator{}
	c.SetSessionData(&team.SessionData{CoordinatorContextManifests: []team.ContextInjectionManifest{{
		SchemaVersion: 1, RequestID: "request-1", RequestHash: "hash-1", RunID: "run-1", Attempt: 1,
		Agent: "coordinator", Phase: team.PhaseInit, Trigger: team.ContextTriggerCoordinatorStart, Purpose: "coordinator_start", ModelCalled: true,
		Items: []team.ContextManifestItem{{ID: "goal", Included: true, Tokens: 12}, {ID: "memory-1", Included: false, Reason: team.ContextOmittedPhase, Tokens: 7}},
	}}})
	c.SetLastRunResult(&team.RunResult{Outcome: team.RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &team.AcceptanceResult{Passed: true}})

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = printResultJSON("done", map[string]*teamContext{"demo": {teamName: "demo", coordinator: c}}, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var out jsonRunOutput
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Teams) != 1 {
		t.Fatalf("teams = %#v", out.Teams)
	}
	summary := out.Teams[0].ContextRouting
	if summary.Requests != 1 || summary.ModelCalls != 1 || summary.Fallbacks != 0 || summary.Purposes["coordinator_start"] != 1 || summary.Included != 1 || summary.Omitted != 1 || summary.IncludedTokens != 12 || summary.OmittedTokens != 7 || summary.OmitReasons[string(team.ContextOmittedPhase)] != 1 {
		t.Fatalf("context routing JSON = %#v", summary)
	}
}

func TestJSONOutputProjectsFinalizationRedactionAndPreservesTypedScalars(t *testing.T) {
	workspace := t.TempDir()
	session := &team.TeamSession{
		Dir:       t.TempDir(),
		Workspace: workspace,
		Config:    agent.TeamConfig{Name: "finalization-json"},
	}
	coordinator, err := team.NewCoordinator(session, "", "", nil, nil, nil, team.RoleModels{}, 2, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatalf("NewCoordinator failed: %v", err)
	}
	eventStore, err := team.NewEventStore(workspace, "run-json", "session-json")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer func() { _ = eventStore.Close() }()
	record := team.DecisionRecord{
		SchemaVersion:        1,
		ID:                   "decision-json",
		RunID:                "run-json",
		TaskID:               "task-json",
		Profile:              "standard",
		FinalOption:          "ship",
		Probability:          0.73,
		FinalizationMode:     "coordinator",
		FinalizationIdentity: "coordinator",
		FinalizationOutcome:  "selected",
		FinalizationReason:   "api_key=run-json-finalization-secret",
		FinalizationWarnings: []string{
			"api_key=run-json-finalization-warning",
		},
		CreatedAt: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(struct {
		DecisionID       string               `json:"decision_id"`
		RunID            string               `json:"run_id"`
		TaskID           string               `json:"task_id"`
		Attempt          int                  `json:"attempt"`
		Profile          string               `json:"profile"`
		Record           *team.DecisionRecord `json:"record"`
		Question         string               `json:"question"`
		ForecastRequired bool                 `json:"forecast_required"`
	}{
		DecisionID: "decision-json", RunID: "run-json", TaskID: "task-json", Attempt: 1,
		Profile: "standard", Record: &record, Question: "Ship?", ForecastRequired: true,
	})
	if err != nil {
		t.Fatalf("marshal decision_finalized payload: %v", err)
	}
	if _, err := eventStore.AppendPersistedContext(context.Background(), team.RunEvent{
		Type: agent.EventDecisionFinalized, Actor: "decision-runtime", RunID: "run-json", TaskID: "task-json", Attempt: 1,
		IdempotencyKey: "decision-json-finalized", Payload: payload,
	}); err != nil {
		t.Fatalf("append decision_finalized event: %v", err)
	}
	coordinator.SetEventJournal(jsonOutputEventJournal{store: eventStore})

	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatalf("OpenDecisionIndex failed: %v", err)
	}
	if err := index.Append(team.DecisionIndexEntry{
		SchemaVersion:        team.DecisionIndexSchemaVersion,
		DecisionID:           "decision-json",
		RunID:                "run-json",
		TaskID:               "task-json",
		Profile:              "standard",
		Question:             "Ship?",
		FinalOption:          "ship",
		Probability:          0.73,
		ForecastRequired:     true,
		FinalizationMode:     "coordinator",
		FinalizationIdentity: "coordinator",
		FinalizationOutcome:  "selected",
		FinalizationReason:   "api_key=run-json-finalization-secret",
		FinalizationWarnings: []string{"api_key=run-json-finalization-warning"},
		CreatedAt:            time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed decision index: %v", err)
	}
	coordinator.SetSessionData(&team.SessionData{CoordinatorContextManifests: []team.ContextInjectionManifest{{
		SchemaVersion: 1, RequestID: "request-1", RunID: "run-1", Attempt: 1,
		Agent: "coordinator", Phase: team.PhaseInit, Trigger: team.ContextTriggerCoordinatorStart,
		Purpose: "coordinator_start", ModelCalled: true,
		Items: []team.ContextManifestItem{{ID: "goal", Included: true, Tokens: 12}, {ID: "memory-1", Included: false, Reason: team.ContextOmittedPhase, Tokens: 7}},
	}}})
	coordinator.SetLastRunResult(&team.RunResult{Outcome: team.RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &team.AcceptanceResult{Passed: true}})

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = printResultJSON("done", map[string]*teamContext{"finalization": {teamName: "finalization", coordinator: coordinator}}, nil)
	_ = w.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		GoalSatisfied json.RawMessage `json:"goal_satisfied"`
		Teams         []struct {
			Decisions      []json.RawMessage `json:"decisions"`
			ContextRouting json.RawMessage   `json:"context_routing"`
		} `json:"teams"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw.GoalSatisfied) != "true" {
		t.Fatalf("goal_satisfied = %s, want JSON boolean true", raw.GoalSatisfied)
	}
	if len(raw.Teams) != 1 || len(raw.Teams[0].Decisions) != 1 {
		t.Fatalf("run JSON projections = %#v", raw)
	}
	var decision map[string]json.RawMessage
	if err := json.Unmarshal(raw.Teams[0].Decisions[0], &decision); err != nil {
		t.Fatal(err)
	}
	if string(decision["probability"]) != "0.73" || string(decision["schema_version"]) != "2" {
		t.Fatalf("decision scalar types/values = probability %s schema %s", decision["probability"], decision["schema_version"])
	}
	if string(decision["forecast_required"]) != "true" {
		t.Fatalf("forecast_required = %s, want JSON boolean true", decision["forecast_required"])
	}
	if got := string(decision["finalization_reason"]); got != `"api_key=[REDACTED]"` {
		t.Fatalf("finalization reason = %s, want redaction marker", got)
	}
	var warnings []string
	if err := json.Unmarshal(decision["finalization_warnings"], &warnings); err != nil {
		t.Fatalf("finalization warnings = %s: %v", decision["finalization_warnings"], err)
	}
	if len(warnings) != 1 || warnings[0] != "api_key=[REDACTED]" {
		t.Fatalf("finalization warnings = %v, want redaction marker", warnings)
	}
	if strings.Contains(string(raw.Teams[0].Decisions[0]), "run-json-finalization-secret") || strings.Contains(string(raw.Teams[0].Decisions[0]), "run-json-finalization-warning") {
		t.Fatalf("run JSON exposed finalization secret/warning: %s", raw.Teams[0].Decisions[0])
	}

	var contextRouting map[string]json.RawMessage
	if err := json.Unmarshal(raw.Teams[0].ContextRouting, &contextRouting); err != nil {
		t.Fatal(err)
	}
	if string(contextRouting["included_tokens"]) != "12" || string(contextRouting["omitted_tokens"]) != "7" {
		t.Fatalf("context token scalars = included %s omitted %s", contextRouting["included_tokens"], contextRouting["omitted_tokens"])
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
