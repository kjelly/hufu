package main

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
)

func TestValidateRunFlags(t *testing.T) {
	// Save originals to restore after each subtest.
	origOutput := opts.outputFormat
	origSteps := opts.stepsMode
	origTUI := opts.tuiMode
	origUnattended := opts.unattended
	origDefault := opts.defaultTeam
	origAgentTeam := opts.agentTeamName
	origDisplayMode := opts.displayMode
	origEventFormat := opts.eventFormat
	defer func() {
		opts.outputFormat = origOutput
		opts.stepsMode = origSteps
		opts.tuiMode = origTUI
		opts.unattended = origUnattended
		opts.defaultTeam = origDefault
		opts.agentTeamName = origAgentTeam
		opts.displayMode = origDisplayMode
		opts.eventFormat = origEventFormat
	}()

	// resetAll sets all flags to their default (non-conflicting) values
	// before each subtest runs, so earlier subtests don't leak state.
	resetAll := func() {
		opts.outputFormat = ""
		opts.stepsMode = false
		opts.tuiMode = false
		opts.unattended = false
		opts.defaultTeam = false
		opts.agentTeamName = ""
		opts.displayMode = "auto"
		opts.eventFormat = "text"
	}

	t.Run("accepts empty output format", func(t *testing.T) {
		resetAll()
		if err := validateRunFlags(); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})
	t.Run("accepts text and json", func(t *testing.T) {
		resetAll()
		for _, v := range []string{"text", "json"} {
			resetAll()
			opts.outputFormat = v
			if err := validateRunFlags(); err != nil {
				t.Errorf("expected nil for %q, got %v", v, err)
			}
		}
	})
	t.Run("rejects unknown output format", func(t *testing.T) {
		resetAll()
		opts.outputFormat = "yaml"
		err := validateRunFlags()
		if err == nil || !strings.Contains(err.Error(), "invalid --output") {
			t.Errorf("expected invalid --output error, got %v", err)
		}
	})
	t.Run("rejects unknown display mode", func(t *testing.T) {
		resetAll()
		opts.displayMode = "jsonl"
		err := validateRunFlags()
		if err == nil || !strings.Contains(err.Error(), "invalid --display-mode") {
			t.Errorf("expected invalid display mode error, got %v", err)
		}
	})
	t.Run("rejects unknown event format", func(t *testing.T) {
		resetAll()
		opts.eventFormat = "yaml"
		if err := validateRunFlags(); err == nil || !strings.Contains(err.Error(), "invalid --event-format") {
			t.Errorf("expected invalid event format error, got %v", err)
		}
	})
	t.Run("json implies quiet", func(t *testing.T) {
		resetAll()
		opts.outputFormat = "json"
		opts.quietMode = false
		_ = validateRunFlags()
		if !opts.quietMode {
			t.Error("expected quietMode to be set when output is json")
		}
	})
	t.Run("rejects --steps + --tui combination", func(t *testing.T) {
		resetAll()
		opts.stepsMode = true
		opts.tuiMode = true
		err := validateRunFlags()
		if err == nil || !strings.Contains(err.Error(), "cannot use --steps") {
			t.Errorf("expected cannot use --steps error, got %v", err)
		}
	})
	t.Run("rejects --default + --agent-team combination", func(t *testing.T) {
		resetAll()
		opts.defaultTeam = true
		opts.agentTeamName = "anything"
		err := validateRunFlags()
		if err == nil || !strings.Contains(err.Error(), "cannot use --default") {
			t.Errorf("expected cannot use --default error, got %v", err)
		}
	})
	t.Run("unattended disables --steps", func(t *testing.T) {
		resetAll()
		opts.unattended = true
		opts.stepsMode = true
		_ = validateRunFlags()
		if opts.stepsMode {
			t.Error("expected stepsMode to be disabled in unattended mode")
		}
	})
	t.Run("unattended disables --tui", func(t *testing.T) {
		resetAll()
		opts.unattended = true
		opts.tuiMode = true
		_ = validateRunFlags()
		if opts.tuiMode {
			t.Error("expected tuiMode to be disabled in unattended mode")
		}
	})
}

func TestExecutionUnresolvedTaskExcludesHistoricalFailures(t *testing.T) {
	started := time.Now().Add(-time.Minute)
	historical := &team.TodoItem{ID: "old", Status: team.TaskError, EndedAt: started}
	prior := snapshotUnresolvedTasks([]*team.TodoItem{historical})

	if got := executionUnresolvedTask([]*team.TodoItem{historical}, prior); got != nil {
		t.Fatalf("unchanged restored failure should not fail this execution, got %#v", got)
	}

	fresh := &team.TodoItem{ID: "new", Status: team.TaskBlocked, EndedAt: time.Now()}
	if got := executionUnresolvedTask([]*team.TodoItem{historical, fresh}, prior); got != fresh {
		t.Fatalf("current execution failure = %#v, want %#v", got, fresh)
	}

	// A retry can reuse an ID; its new terminal timestamp makes it a current
	// execution failure rather than stale session state.
	historical.EndedAt = time.Now()
	if got := executionUnresolvedTask([]*team.TodoItem{historical}, prior); got != historical {
		t.Fatalf("re-created failure = %#v, want %#v", got, historical)
	}
}

func TestIsInteractiveEnvironment(t *testing.T) {
	// Save CI env vars
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI"} {
		t.Setenv(k, "")
	}
	// In test environment, stdin is typically not a TTY
	result := tools.IsInteractiveEnvironment()
	// We can only assert it's a bool — actual value depends on test environment
	_ = result
}

func TestRenderExecutionSummary(t *testing.T) {
	summary := executionSummary{teams: []string{"dev"}, workspaces: []string{"/tmp/workspace"}, total: 4, done: 1, errored: 1, skipped: 1, pending: 1}
	res := &team.RunResult{Outcome: team.RunOutcomePartial, StopReason: team.StopReasonBudgetExceeded, GoalMode: team.GoalModeOutcome}
	out := formatExecutionSummary(summary, 3*time.Second, []*team.RunResult{res})
	for _, want := range []string{"Team:      dev", "1 done", "3s", "Outcome:   partial", "Status:    Budget exhausted"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q: %q", want, out)
		}
	}
}

func TestMultiTeamFinishedMsgAggregationPrecedence(t *testing.T) {
	tc1 := &teamContext{teamName: "teamA", coordinator: &team.Coordinator{}}
	tc1.coordinator.SetLastRunResult(&team.RunResult{
		Outcome:       team.RunOutcomeCompleted,
		GoalSatisfied: false,
		GoalMode:      team.GoalModeExploratory,
		Acceptance:    &team.AcceptanceResult{State: team.AcceptanceNotConfigured},
	})
	tc2 := &teamContext{teamName: "teamB", coordinator: &team.Coordinator{}}
	tc2.coordinator.SetLastRunResult(&team.RunResult{
		Outcome:         team.RunOutcomePartial,
		GoalSatisfied:   false,
		GoalMode:        team.GoalModeOutcome,
		StopReason:      team.StopReasonUnresolvedTasks,
		UnresolvedTasks: []team.TaskReference{{ID: "t1", Status: "pending"}},
	})

	loadedTeams := map[string]*teamContext{
		"teamA": tc1,
		"teamB": tc2,
	}

	var names []string
	for name := range loadedTeams {
		names = append(names, name)
	}
	sort.Strings(names)

	var resList []*team.RunResult
	for _, name := range names {
		tc := loadedTeams[name]
		if tc != nil && tc.coordinator != nil {
			if r := tc.coordinator.LastRunResult(); r != nil {
				resList = append(resList, r)
			}
		}
	}
	if len(resList) != 2 {
		t.Fatalf("resList length = %d, want 2", len(resList))
	}
	aggregated := team.AggregateRunResults(resList, nil, team.RunStats{})

	if aggregated.GoalMode != team.GoalModeOutcome {
		t.Fatalf("aggregated goal_mode = %q, want %q (outcome wins over exploratory)", aggregated.GoalMode, team.GoalModeOutcome)
	}
	if aggregated.Outcome != team.RunOutcomePartial || aggregated.GoalSatisfied {
		t.Fatalf("aggregated result = %#v, want outcome=partial, goal_satisfied=false", aggregated)
	}
	if aggregated.StopReason != team.StopReasonUnresolvedTasks {
		t.Fatalf("aggregated stop_reason = %q, want %q (unresolved tasks preserved, not budget_exceeded)", aggregated.StopReason, team.StopReasonUnresolvedTasks)
	}
	if len(aggregated.UnresolvedTasks) != 1 || aggregated.UnresolvedTasks[0].ID != "t1" {
		t.Fatalf("aggregated unresolved tasks = %#v, want t1", aggregated.UnresolvedTasks)
	}
}

func TestExplicitAgentReplanEscalatesThroughCoordinator(t *testing.T) {
	tc := &teamContext{coordinator: &team.Coordinator{}}
	injector := newPromptInjector(nil)
	active := new(activeCoordinator)
	calls := 0
	var gotPrompt string
	runCoordinator := func(_ context.Context, prompt string) (string, error) {
		calls++
		gotPrompt = prompt
		return "coordinator replan result", nil
	}

	result, err := runDirectReplanThroughCoordinator(context.Background(), tc, "complete the requested work", injector, active, runCoordinator)
	if err != nil {
		t.Fatalf("replan escalation error: %v", err)
	}
	if calls != 1 || result != "coordinator replan result" {
		t.Fatalf("replan calls=%d result=%q, want one coordinator result", calls, result)
	}
	if !strings.Contains(gotPrompt, "no-progress replan threshold") || !strings.Contains(gotPrompt, "complete the requested work") {
		t.Fatalf("coordinator prompt = %q, want replan context and original request", gotPrompt)
	}
	if active.Load() != nil {
		t.Fatal("active coordinator was not cleared after replan")
	}
}

func TestExplicitAgentReplanPropagatesCoordinatorOutcomeError(t *testing.T) {
	tc := &teamContext{coordinator: &team.Coordinator{}}
	injector := newPromptInjector(nil)
	expected := errors.New("coordinator returned partial run outcome")
	result, err := runDirectReplanThroughCoordinator(context.Background(), tc, "continue the work", injector, new(activeCoordinator), func(context.Context, string) (string, error) {
		return "partial result", expected
	})
	if !errors.Is(err, expected) {
		t.Fatalf("replan error = %v, want coordinator outcome error", err)
	}
	if result != "partial result" {
		t.Fatalf("partial replan result = %q, want propagated result", result)
	}
}

func TestExecuteSegmentsExplicitAgentReplanBranchSavesFinalSessionMD(t *testing.T) {
	workspace := t.TempDir()
	tc := &teamContext{
		teamName:    "demo",
		session:     &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "demo"}},
		sessionData: team.NewSession(),
		coordinator: &team.Coordinator{},
	}
	tc.coordinator.SetTaskTracker(team.NewTaskTracker())
	loaded := map[string]*teamContext{"demo": tc}
	injector := newPromptInjector(nil)
	active := new(activeCoordinator)
	originalEventFormat := opts.eventFormat
	opts.eventFormat = "jsonl"
	defer func() { opts.eventFormat = originalEventFormat }()

	directCalls := 0
	coordinatorCalls := 0
	result, err := executeSegmentsWithRunners(
		context.Background(),
		[]team.PromptSegment{
			{Type: team.SegmentSwitchTeam, Name: "demo"},
			{Type: team.SegmentInvokeAgent, Name: "worker", Content: "do the work"},
		},
		nil, "", loaded, injector, active, nil, nil, RouteDecision{},
		func(context.Context, *team.Coordinator, string, string) (*team.DirectAgentResult, error) {
			directCalls++
			return &team.DirectAgentResult{Error: errors.New("direct replan required"), ReplanRequired: true}, nil
		},
		func(_ context.Context, _ *team.Coordinator, prompt string) (string, error) {
			coordinatorCalls++
			tc.sessionData.Entries = append(tc.sessionData.Entries, team.SessionEntry{Role: "assistant", Content: "replanned result"})
			if !strings.Contains(prompt, "do the work") {
				t.Fatalf("coordinator prompt = %q, want original request", prompt)
			}
			return "replanned result", nil
		},
	)
	if err != nil {
		t.Fatalf("executeSegments error: %v", err)
	}
	if directCalls != 1 || coordinatorCalls != 1 || !strings.Contains(result, "replanned result") {
		t.Fatalf("calls/result = %d/%d/%q, want one direct, one coordinator, and replan output", directCalls, coordinatorCalls, result)
	}
	if active.Load() != nil {
		t.Fatal("active coordinator was not cleared")
	}
	if md := team.LoadSessionMD(workspace); !strings.Contains(md, "replanned result") {
		t.Fatalf("final session markdown = %q, want replan result checkpoint", md)
	}
}

func TestOfferFirstTimeWizard(t *testing.T) {
	t.Run("returns error mentioning search paths", func(t *testing.T) {
		// Force non-interactive by setting CI
		t.Setenv("CI", "1")
		err := offerFirstTimeWizard([]string{"/tmp/nonexistent-a", "/tmp/nonexistent-b"})
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "no agent teams found") {
			t.Errorf("expected 'no agent teams found' in error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "/tmp/nonexistent-a") {
			t.Errorf("expected search path in error, got: %v", err)
		}
	})
}

func TestNewChatCompleter(t *testing.T) {
	c := newChatCompleter("myteam", []string{"myteam", "other"}, []string{"developer", "reviewer"}).(*chatCompleter)
	if c.teamName != "myteam" {
		t.Errorf("expected teamName=myteam, got %q", c.teamName)
	}
	if len(c.registry.teams) != 2 {
		t.Errorf("expected 2 teams, got %d", len(c.registry.teams))
	}
	if len(c.registry.agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(c.registry.agents))
	}
}

func TestChatCompleterDo(t *testing.T) {
	c := &chatCompleter{
		teamName: "myteam",
		registry: &teamRegistryLike{
			teams:  []string{"myteam", "alpha"},
			agents: []string{"developer", "reviewer"},
		},
	}

	t.Run("slash command completion", func(t *testing.T) {
		newLine, length := c.Do([]rune("/h"), 2)
		if length != 2 {
			t.Errorf("expected length=2, got %d", length)
		}
		if len(newLine) == 0 {
			t.Error("expected at least one match for /h")
		}
		// should suggest /help
		found := false
		for _, r := range newLine {
			if string(r) == "elp" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected 'elp' suggestion, got %v", newLine)
		}
	})
	t.Run("@ team completion", func(t *testing.T) {
		newLine, _ := c.Do([]rune("@a"), 2)
		if len(newLine) == 0 {
			t.Error("expected at least one match for @a")
		}
		found := false
		for _, r := range newLine {
			if string(r) == "lpha" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected 'lpha' suggestion, got %v", newLine)
		}
	})
	t.Run("@ agent completion", func(t *testing.T) {
		newLine, _ := c.Do([]rune("@d"), 2)
		found := false
		for _, r := range newLine {
			if string(r) == "eveloper" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected 'eveloper' suggestion, got %v", newLine)
		}
	})
	t.Run("no completion for plain text", func(t *testing.T) {
		newLine, _ := c.Do([]rune("hello"), 5)
		if newLine != nil {
			t.Errorf("expected no completions for plain text, got %v", newLine)
		}
	})
}
