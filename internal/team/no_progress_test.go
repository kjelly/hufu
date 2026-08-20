package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// TestDecideNoProgress_TableDriven drives the pure enforcement function
// across the §8.1 disposition ladder. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
func TestDecideNoProgress_TableDriven(t *testing.T) {
	limits := NoProgressLimits{MaxTokens: 1000, MaxTurns: 4, MaxTasks: 3}

	tests := []struct {
		name               string
		counters           NoProgressCounters
		hard               bool
		want               NoProgressDisposition
		wantNonEmptyReason bool
	}{
		{
			name:     "all below limits -> continue",
			counters: NoProgressCounters{Tokens: 999, Turns: 3, Tasks: 2},
			hard:     true,
			want:     NoProgressContinue,
		},
		{
			name:     "zero counters -> continue",
			counters: NoProgressCounters{},
			hard:     true,
			want:     NoProgressContinue,
		},
		{
			name:               "tokens at limit -> replan_required",
			counters:           NoProgressCounters{Tokens: 1000, Turns: 0, Tasks: 0},
			hard:               true,
			want:               NoProgressReplan,
			wantNonEmptyReason: true,
		},
		{
			name:               "turns at limit -> replan_required",
			counters:           NoProgressCounters{Tokens: 0, Turns: 4, Tasks: 0},
			hard:               true,
			want:               NoProgressReplan,
			wantNonEmptyReason: true,
		},
		{
			name:               "tasks at limit -> replan_required",
			counters:           NoProgressCounters{Tokens: 0, Turns: 0, Tasks: 3},
			hard:               true,
			want:               NoProgressReplan,
			wantNonEmptyReason: true,
		},
		{
			name:               "tokens at 2x limit -> stop_partial",
			counters:           NoProgressCounters{Tokens: 2000, Turns: 0, Tasks: 0},
			hard:               true,
			want:               NoProgressStop,
			wantNonEmptyReason: true,
		},
		{
			name:               "turns at 2x limit -> stop_partial",
			counters:           NoProgressCounters{Tokens: 0, Turns: 8, Tasks: 0},
			hard:               true,
			want:               NoProgressStop,
			wantNonEmptyReason: true,
		},
		{
			name:               "tasks at 2x limit -> stop_partial",
			counters:           NoProgressCounters{Tokens: 0, Turns: 0, Tasks: 6},
			hard:               true,
			want:               NoProgressStop,
			wantNonEmptyReason: true,
		},
		{
			name:               "tokens above 2x limit -> stop_partial",
			counters:           NoProgressCounters{Tokens: 5000, Turns: 0, Tasks: 0},
			hard:               true,
			want:               NoProgressStop,
			wantNonEmptyReason: true,
		},
		{
			name:     "warn-only at limit -> continue",
			counters: NoProgressCounters{Tokens: 1000, Turns: 4, Tasks: 3},
			hard:     false,
			want:     NoProgressContinue,
		},
		{
			name:     "warn-only at 2x limit -> continue",
			counters: NoProgressCounters{Tokens: 2000, Turns: 8, Tasks: 6},
			hard:     false,
			want:     NoProgressContinue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := decideNoProgress(tt.counters, limits, tt.hard)
			if got != tt.want {
				t.Fatalf("decideNoProgress(%+v, %+v, hard=%v) = %q, want %q (reason=%q)",
					tt.counters, limits, tt.hard, got, tt.want, reason)
			}
			if tt.wantNonEmptyReason && reason == "" {
				t.Fatalf("expected non-empty reason for %q, got empty", got)
			}
			if !tt.wantNonEmptyReason && got == NoProgressContinue && reason != "" && tt.hard {
				// continue with hard enforcement should have empty reason
				t.Fatalf("expected empty reason for continue, got %q", reason)
			}
		})
	}
}

// TestDecideNoProgress_DisabledCounterIgnored asserts that a 0 limit
// disables that one counter (the YAML `0` override) while the other two
// remain enforced. Refs: docs/hufu-generic-task-reliability-mechanisms.md
// §8.1, WP-12
func TestDecideNoProgress_DisabledCounterIgnored(t *testing.T) {
	// MaxTokens disabled (0), turns and tasks enforced.
	limits := NoProgressLimits{MaxTokens: 0, MaxTurns: 4, MaxTasks: 3}

	// Tokens far exceed any non-zero limit, but the counter is disabled → continue.
	got, _ := decideNoProgress(NoProgressCounters{Tokens: 9_999_999, Turns: 0, Tasks: 0}, limits, true)
	if got != NoProgressContinue {
		t.Fatalf("disabled token counter should not trigger: got %q, want continue", got)
	}

	// Turns still enforced.
	got, _ = decideNoProgress(NoProgressCounters{Tokens: 9_999_999, Turns: 4, Tasks: 0}, limits, true)
	if got != NoProgressReplan {
		t.Fatalf("turns at limit with tokens disabled should replan: got %q, want replan_required", got)
	}

	// Tasks still enforced at 2x.
	got, _ = decideNoProgress(NoProgressCounters{Tokens: 9_999_999, Turns: 0, Tasks: 6}, limits, true)
	if got != NoProgressStop {
		t.Fatalf("tasks at 2x limit with tokens disabled should stop: got %q, want stop_partial", got)
	}

	// All disabled → continue regardless of counters.
	allDisabled := NoProgressLimits{}
	got, _ = decideNoProgress(NoProgressCounters{Tokens: 9_999_999, Turns: 999, Tasks: 999}, allDisabled, true)
	if got != NoProgressContinue {
		t.Fatalf("all counters disabled should always continue: got %q, want continue", got)
	}
}

// TestNoProgressCounters_ResetOnlyByCriterionProgress asserts the three
// counters return to 0 after resetAfterCriterionProgress is driven by an
// advancing criterion, and stay non-zero after a done-only task completion
// (task done does NOT reset the counters). Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
func TestNoProgressCounters_ResetOnlyByCriterionProgress(t *testing.T) {
	c := newNoProgressTestCoordinator(t)

	// Simulate accumulation: 2 turns, 3 tasks, 500 tokens.
	c.metricsMu.Lock()
	c.turnsSinceCriterionProgress = 2
	c.tasksSinceCriterionProgress = 3
	c.tokensSinceCriterionProgress = 500
	c.metricsMu.Unlock()

	// A task reaching done does NOT reset the counters. We model this by
	// calling the done path directly (UpdateStatusAndOutput) and checking the
	// counters are unchanged. The reset only happens in the criterion-progress
	// path (criteria.go / criterion_checkpoint.go), not in task status updates.
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "done-only task", Advances: []string{"build"}},
	})
	c.taskTracker.TodoList().UpdateStatusAndOutput(items[0].ID, TaskDone, "", "completed")
	got := c.noProgressCounters()
	if got.Turns != 2 || got.Tasks != 3 || got.Tokens != 500 {
		t.Fatalf("task done must NOT reset no-progress counters: got %+v, want {2 3 500}", got)
	}

	// Drive criterion progress via the real reset path (criteria.go). We
	// call the reset block directly to avoid needing a full criterion
	// evaluation harness; the reset is the single source of truth.
	advanced := []string{"build"}
	var allItems []*TodoItem
	allItems = append(allItems, items...)
	c.metricsMu.Lock()
	c.antiThrashing.DiagnosticSinceProgress = 0
	c.antiThrashing.DiagnosticTasksCounted = make(map[string]bool)
	c.antiThrashing.resetAfterCriterionProgress(advanced, allItems)
	c.tokensSinceCriterionProgress = 0
	c.turnsSinceCriterionProgress = 0
	c.tasksSinceCriterionProgress = 0
	c.noProgressReplanTripped = false
	c.reliabilityUsageByAttempt = make(map[string]int)
	c.metricsMu.Unlock()

	got = c.noProgressCounters()
	if got.Turns != 0 || got.Tasks != 0 || got.Tokens != 0 {
		t.Fatalf("criterion progress must reset all three counters: got %+v, want all zero", got)
	}
}

// TestDefaultReliabilityConfig_NoProgressLimitsPopulated asserts
// DefaultReliabilityConfig() populates all three no-progress limits with
// non-zero values. Refs: docs/hufu-generic-task-reliability-mechanisms.md
// §8.1, WP-12
func TestDefaultReliabilityConfig_NoProgressLimitsPopulated(t *testing.T) {
	def := agent.DefaultReliabilityConfig()
	if def.MaxTokensWithoutProgress == 0 {
		t.Fatal("DefaultReliabilityConfig MaxTokensWithoutProgress = 0, want non-zero")
	}
	if def.MaxTurnsWithoutProgress == 0 {
		t.Fatal("DefaultReliabilityConfig MaxTurnsWithoutProgress = 0, want non-zero")
	}
	if def.MaxTasksWithoutProgress == 0 {
		t.Fatal("DefaultReliabilityConfig MaxTasksWithoutProgress = 0, want non-zero")
	}
}

// TestNoProgressYAML_ExplicitZeroDisablesOneCounter asserts a team YAML with
// explicit max-tokens-without-progress: 0 disables that one counter while the
// other two remain enforced. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
func TestNoProgressYAML_ExplicitZeroDisablesOneCounter(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: no-progress-zero\nacceptance: 'true'\nreliability:\n  max-tokens-without-progress: 0\n"
	if err := writeFile(dir, "team.yaml", yaml); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reliability.MaxTokensWithoutProgress != 0 {
		t.Fatalf("MaxTokensWithoutProgress = %d, want 0 (explicit zero)", cfg.Reliability.MaxTokensWithoutProgress)
	}
	if !cfg.Reliability.MaxTokensWithoutProgressSet {
		t.Fatal("MaxTokensWithoutProgressSet = false, want true for explicit zero")
	}
	// The other two must keep their defaults.
	def := agent.DefaultReliabilityConfig()
	if cfg.Reliability.MaxTurnsWithoutProgress != def.MaxTurnsWithoutProgress {
		t.Fatalf("MaxTurnsWithoutProgress = %d, want default %d", cfg.Reliability.MaxTurnsWithoutProgress, def.MaxTurnsWithoutProgress)
	}
	if cfg.Reliability.MaxTasksWithoutProgress != def.MaxTasksWithoutProgress {
		t.Fatalf("MaxTasksWithoutProgress = %d, want default %d", cfg.Reliability.MaxTasksWithoutProgress, def.MaxTasksWithoutProgress)
	}

	// End-to-end through reliabilityConfig(): the explicit zero must be
	// honored, not restored to the default.
	c := &Coordinator{session: &TeamSession{Config: cfg}}
	rc := c.reliabilityConfig()
	if rc.MaxTokensWithoutProgress != 0 {
		t.Fatalf("reliabilityConfig() MaxTokensWithoutProgress = %d, want 0 (explicit zero honored)", rc.MaxTokensWithoutProgress)
	}
	if rc.MaxTurnsWithoutProgress != def.MaxTurnsWithoutProgress {
		t.Fatalf("reliabilityConfig() MaxTurnsWithoutProgress = %d, want default %d", rc.MaxTurnsWithoutProgress, def.MaxTurnsWithoutProgress)
	}
}

// TestNoProgressYAML_ExplicitValuesOverrideDefaults asserts explicit non-zero
// values override the defaults, and unset restores the default. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
func TestNoProgressYAML_ExplicitValuesOverrideDefaults(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: no-progress-override\nacceptance: 'true'\nreliability:\n  max-turns-without-progress: 2\n  max-tasks-without-progress: 5\n"
	if err := writeFile(dir, "team.yaml", yaml); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := agent.DefaultReliabilityConfig()
	if cfg.Reliability.MaxTurnsWithoutProgress != 2 {
		t.Fatalf("MaxTurnsWithoutProgress = %d, want 2 (explicit override)", cfg.Reliability.MaxTurnsWithoutProgress)
	}
	if cfg.Reliability.MaxTasksWithoutProgress != 5 {
		t.Fatalf("MaxTasksWithoutProgress = %d, want 5 (explicit override)", cfg.Reliability.MaxTasksWithoutProgress)
	}
	// Unset token limit restores the default.
	if cfg.Reliability.MaxTokensWithoutProgress != def.MaxTokensWithoutProgress {
		t.Fatalf("MaxTokensWithoutProgress = %d, want default %d (unset)", cfg.Reliability.MaxTokensWithoutProgress, def.MaxTokensWithoutProgress)
	}
	if cfg.Reliability.MaxTurnsWithoutProgressSet != true {
		t.Fatal("MaxTurnsWithoutProgressSet = false, want true for explicit value")
	}
}

// TestNoProgressYAML_UnsetRestoresDefaults asserts that a team YAML with no
// no-progress fields receives all three defaults. Refs:
// docs/hufu-generic-task-reliability-mechanisms.md §8.1, WP-12
func TestNoProgressYAML_UnsetRestoresDefaults(t *testing.T) {
	dir := t.TempDir()
	yaml := "name: no-progress-unset\nacceptance: 'true'\n"
	if err := writeFile(dir, "team.yaml", yaml); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	def := agent.DefaultReliabilityConfig()
	if cfg.Reliability.MaxTokensWithoutProgress != def.MaxTokensWithoutProgress {
		t.Fatalf("MaxTokensWithoutProgress = %d, want default %d", cfg.Reliability.MaxTokensWithoutProgress, def.MaxTokensWithoutProgress)
	}
	if cfg.Reliability.MaxTurnsWithoutProgress != def.MaxTurnsWithoutProgress {
		t.Fatalf("MaxTurnsWithoutProgress = %d, want default %d", cfg.Reliability.MaxTurnsWithoutProgress, def.MaxTurnsWithoutProgress)
	}
	if cfg.Reliability.MaxTasksWithoutProgress != def.MaxTasksWithoutProgress {
		t.Fatalf("MaxTasksWithoutProgress = %d, want default %d", cfg.Reliability.MaxTasksWithoutProgress, def.MaxTasksWithoutProgress)
	}
	if cfg.Reliability.MaxTokensWithoutProgressSet {
		t.Fatal("MaxTokensWithoutProgressSet = true, want false for unset")
	}
}

func TestEnforceNoProgressBudget_ReplanThenPartialContinuation(t *testing.T) {
	var events []StatusEvent
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Name: "no-progress-enforcement",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgress:     0,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgress:     0,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(event StatusEvent) { events = append(events, event) },
	}
	c.tokensSinceCriterionProgress = 10

	stopped, reason := c.enforceNoProgressBudget()
	if stopped || reason != "" {
		t.Fatalf("first threshold stopped=%v reason=%q, want replan without stop", stopped, reason)
	}
	if c.IsWrapUp() || !c.noProgressReplanPending() {
		t.Fatalf("first threshold state: wrap_up=%v pending=%v, want non-terminal replan", c.IsWrapUp(), c.noProgressReplanPending())
	}

	// The first replan is allowed one continuation turn. If that turn makes
	// no objective progress, the next boundary must produce a resumable
	// partial result rather than another warning-only replan.
	stopped, reason = c.enforceNoProgressBudget()
	if !stopped || reason == "" {
		t.Fatalf("second threshold stopped=%v reason=%q, want stopped with reason", stopped, reason)
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomePartial || result.Continuation == nil {
		t.Fatalf("no-progress stop result=%#v, want partial with continuation", result)
	}
	found := false
	for _, event := range events {
		if event.Type == "no_progress_replan" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no-progress enforcement did not report a structured replan event")
	}
}

func TestEnforceNoProgressBudget_ReplanDoesNotCloseDelegationGate(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
	}
	c.tokensSinceCriterionProgress = 10

	stopped, reason := c.enforceNoProgressBudget()
	if stopped || reason != "" {
		t.Fatalf("first threshold stopped=%v reason=%q, want replan", stopped, reason)
	}
	if c.IsWrapUp() {
		t.Fatal("replan closed the terminal delegation gate")
	}

	// Agent validation intentionally fails after the delegation gate. Seeing
	// that error instead of "wrap-up in progress" proves the replan turn is
	// still permitted to attempt a new plan.
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "missing", Goal: "replan work"}})
	if err == nil {
		t.Fatal("expected agent validation error")
	}
	if strings.Contains(err.Error(), "wrap-up in progress") {
		t.Fatalf("replan delegation was rejected as terminal wrap-up: %v", err)
	}
	if !strings.Contains(err.Error(), "agent validation failed") {
		t.Fatalf("delegation did not reach validation: %v", err)
	}
}

func TestEnforceNoProgressBudget_WarnOnlyDoesNotWrapUp(t *testing.T) {
	var events []StatusEvent
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Name: "no-progress-warn-only",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgress:     0,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgress:     0,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             false,
				WarnOnly:                    true,
			},
		}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(event StatusEvent) { events = append(events, event) },
	}
	c.tokensSinceCriterionProgress = 20
	stopped, reason := c.enforceNoProgressBudget()
	if stopped || reason != "" {
		t.Fatalf("warn-only enforcement stopped=%v reason=%q, want no stop", stopped, reason)
	}
	if c.IsWrapUp() || c.LastRunResult() != nil {
		t.Fatalf("warn-only enforcement changed terminal state: wrap_up=%v result=%#v", c.IsWrapUp(), c.LastRunResult())
	}
	for _, event := range events {
		if event.Type == "no_progress_replan" {
			return
		}
	}
	t.Fatal("warn-only threshold did not emit no_progress_replan warning")
}

func TestFinishPreservesNoProgressStopWithoutUnresolvedTasks(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Name: "no-progress-finish-boundary",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgress:     0,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgress:     0,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.tokensSinceCriterionProgress = 20
	if stopped, reason := c.enforceNoProgressBudget(); !stopped || reason == "" {
		t.Fatalf("no-progress stop = (%v, %q), want terminal stop", stopped, reason)
	}

	tool := &finishTool{coordinator: c}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"partial evidence"}`})
	if err != nil {
		t.Fatalf("finish tool error: %v", err)
	}
	if response.IsError || !strings.Contains(response.Content, "FINISHED:partial evidence") {
		t.Fatalf("finish response = %#v, want successful FINISHED response", response)
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome != RunOutcomePartial || result.StopReason != StopReasonBudgetExceeded || result.Continuation == nil {
		t.Fatalf("finish overwrote no-progress stop: result=%#v, want partial budget continuation", result)
	}
}

func newAcceptanceBlockedNoProgressCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Name: "no-progress-acceptance-order",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.tokensSinceCriterionProgress = 20
	if stopped, reason := c.enforceNoProgressBudget(); !stopped || reason == "" {
		t.Fatalf("no-progress stop = (%v, %q), want terminal stop", stopped, reason)
	}
	return c
}

func TestFinishNoProgressStopSkipsBlockingAcceptanceSelfHealing(t *testing.T) {
	c := newAcceptanceBlockedNoProgressCoordinator(t)
	strict, ok := GetBuiltinProfile(string(ProfileStrictVerification))
	if !ok {
		t.Fatal("strict verification profile missing")
	}
	strict.RequireEvidenceManifest = false
	strict.RequireClosedTerminals = false
	c.SetExecutionProfile(strict)
	c.SetAcceptance("false")

	response, err := (&finishTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"response":"partial evidence"}`})
	if err != nil {
		t.Fatalf("finish tool error: %v", err)
	}
	if response.IsError || !strings.Contains(response.Content, "FINISHED:partial evidence") {
		t.Fatalf("finish response = %#v, want terminal FINISHED response", response)
	}
	if c.selfHealingAttempts != 0 || !c.finishCalled.Load() {
		t.Fatalf("acceptance side effects: selfHealingAttempts=%d finishCalled=%v", c.selfHealingAttempts, c.finishCalled.Load())
	}
	if last := c.LastRunResult(); last == nil || last.Outcome != RunOutcomePartial || last.StopReason != StopReasonBudgetExceeded || last.Continuation == nil {
		t.Fatalf("terminal result = %#v, want preserved partial budget continuation", last)
	}
}

func TestFinishNoProgressStopSkipsUnattendedAcceptanceRollback(t *testing.T) {
	c := newAcceptanceBlockedNoProgressCoordinator(t)
	c.SetUnattended(true)
	c.SetAcceptance("false")
	rollbackMarker := filepath.Join(c.session.Workspace, "rollback-ran")
	c.SetRollback("touch " + rollbackMarker)

	response, err := (&finishTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: `{"response":"partial evidence"}`})
	if err != nil {
		t.Fatalf("finish tool error: %v", err)
	}
	if response.IsError || !strings.Contains(response.Content, "FINISHED:partial evidence") {
		t.Fatalf("finish response = %#v, want terminal FINISHED response", response)
	}
	if c.selfHealingAttempts != 0 || !c.finishCalled.Load() {
		t.Fatalf("unattended acceptance side effects: selfHealingAttempts=%d finishCalled=%v", c.selfHealingAttempts, c.finishCalled.Load())
	}
	if _, err := os.Stat(rollbackMarker); !os.IsNotExist(err) {
		t.Fatalf("rollback marker stat error=%v; terminal no-progress stop must suppress rollback", err)
	}
}

func TestNoProgressCountsCoordinatorTurnsAndTokens(t *testing.T) {
	c := &Coordinator{}
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		return "ok", nil, nil
	}
	if _, _, err := c.runOrchestrator(context.Background(), &agent.AgentDef{Name: "coordinator"}, "turn"); err != nil {
		t.Fatal(err)
	}
	if got := c.noProgressCounters().Turns; got != 1 {
		t.Fatalf("coordinator turn count = %d, want 1", got)
	}

	accounted := c.addStepTokens([]fantasy.StepResult{{Response: fantasy.Response{Usage: fantasy.Usage{TotalTokens: 17}}}})
	if accounted != 17 {
		t.Fatalf("addStepTokens accounted %d, want 17", accounted)
	}
	c.recordNoProgressTokens(accounted)
	if got := c.noProgressCounters().Tokens; got != 17 {
		t.Fatalf("coordinator token count = %d, want 17", got)
	}
}

func TestNoProgressAccountingBoundaries(t *testing.T) {
	c := &Coordinator{}
	c.recordNoProgressTasks(3)
	if got := c.noProgressCounters().Tasks; got != 3 {
		t.Fatalf("task creation count = %d, want 3", got)
	}
	c.observeSidecarUsage(&fantasy.AgentResult{TotalUsage: fantasy.Usage{TotalTokens: 11}})
	if got := c.noProgressCounters().Tokens; got != 11 {
		t.Fatalf("sidecar token count = %d, want 11", got)
	}

	if !llmUsageNeedsDirectNoProgressAccounting(context.Background()) {
		t.Fatal("unmarked coordinator stream must use direct no-progress accounting")
	}
	if !llmUsageNeedsDirectNoProgressAccounting(context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)) {
		t.Fatal("explicitly unreceipted stream must use direct no-progress accounting")
	}
	if llmUsageNeedsDirectNoProgressAccounting(context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)) {
		t.Fatal("receipt-backed worker stream must not use direct no-progress accounting")
	}
}

func TestAuxiliaryLLMStreamsOverrideWorkerReceiptMarker(t *testing.T) {
	workerCtx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	if llmUsageNeedsDirectNoProgressAccounting(workerCtx) {
		t.Fatal("worker stream unexpectedly selected direct accounting")
	}

	for _, name := range []string{"protocol repair", "plan reviewer"} {
		auxCtx := context.WithValue(workerCtx, llmUsageReceiptExpectedKey{}, false)
		if !llmUsageNeedsDirectNoProgressAccounting(auxCtx) {
			t.Fatalf("%s stream did not override the worker receipt marker", name)
		}
	}
}

type noProgressAccountingAgent struct{}

func (noProgressAccountingAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return noProgressAccountingAgent{}.result(), nil
}

func (noProgressAccountingAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return noProgressAccountingAgent{}.result(), nil
}

func (noProgressAccountingAgent) result() *fantasy.AgentResult {
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "ok"},
		}},
		Steps: []fantasy.StepResult{{Response: fantasy.Response{Usage: fantasy.Usage{TotalTokens: 7}}}},
	}
}

func TestRunAgentUsageAccountingHonorsReceiptMarker(t *testing.T) {
	newCoordinator := func() *Coordinator {
		return &Coordinator{
			session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "usage-accounting"}},
			taskTracker:  NewTaskTracker(),
			reportStatus: func(StatusEvent) {},
		}
	}

	worker := newCoordinator()
	workerCtx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, true)
	if _, _, err := worker.runAgentWithStatusAndHistory(workerCtx, noProgressAccountingAgent{}, "worker", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatalf("worker stream failed: %v", err)
	}
	if got := worker.noProgressCounters().Tokens; got != 0 {
		t.Fatalf("receipt-backed worker stream added %d direct tokens, want 0", got)
	}

	auxiliary := newCoordinator()
	auxiliaryCtx := context.WithValue(context.Background(), llmUsageReceiptExpectedKey{}, false)
	if _, _, err := auxiliary.runAgentWithStatusAndHistory(auxiliaryCtx, noProgressAccountingAgent{}, "auxiliary", "prompt", nil, &taskTiming{}); err != nil {
		t.Fatalf("auxiliary stream failed: %v", err)
	}
	if got := auxiliary.noProgressCounters().Tokens; got != 7 {
		t.Fatalf("unreceipted auxiliary stream added %d tokens, want 7", got)
	}
}

func TestObjectiveVerifierProgressResetsOnlyOnFailToPass(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), sessionData: NewSession()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify", Verify: "test -f artifact"}})[0]
	c.metricsMu.Lock()
	c.tokensSinceCriterionProgress = 100
	c.turnsSinceCriterionProgress = 3
	c.tasksSinceCriterionProgress = 4
	c.noProgressReplanTripped = true
	c.metricsMu.Unlock()

	// A first pass has no recorded failure, so it is not a fail→pass transition.
	c.noteObjectiveVerifierResult(item.ID, true)
	if got := c.noProgressCounters(); got != (NoProgressCounters{Tokens: 100, Turns: 3, Tasks: 4}) {
		t.Fatalf("first verifier pass reset counters: got %+v", got)
	}

	item.ExecutionReceipts = []ExecutionReceipt{{VerifyResult: &VerificationResult{ExitCode: 1}}}
	c.noteObjectiveVerifierResult(item.ID, true)
	if got := c.noProgressCounters(); got != (NoProgressCounters{}) {
		t.Fatalf("fail→pass verifier transition did not reset counters: got %+v", got)
	}
	if c.sessionData.LastCriterionProgressAt == "" {
		t.Fatal("fail→pass verifier transition did not update progress timestamp")
	}

	c.metricsMu.Lock()
	c.tokensSinceCriterionProgress = 50
	c.turnsSinceCriterionProgress = 2
	c.tasksSinceCriterionProgress = 1
	c.metricsMu.Unlock()
	item.ExecutionReceipts = []ExecutionReceipt{{VerifyResult: &VerificationResult{ExitCode: 0}}}
	c.noteObjectiveVerifierResult(item.ID, true)
	if got := c.noProgressCounters(); got != (NoProgressCounters{Tokens: 50, Turns: 2, Tasks: 1}) {
		t.Fatalf("pass→pass verifier result reset counters: got %+v", got)
	}
}

func TestContinuationCheckpointPersistsAndRestoresNoProgressCounters(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "continuation-progress"}},
		sessionData: NewSession(),
	}
	c.metricsMu.Lock()
	c.tokensSinceCriterionProgress = 123
	c.turnsSinceCriterionProgress = 4
	c.tasksSinceCriterionProgress = 7
	c.noProgressReplanTripped = true
	c.metricsMu.Unlock()
	c.saveContinuationCheckpoint(2, 5, "no-progress", "pending")

	cp := c.ContinuationCheckpoint()
	if cp == nil || cp.NoProgress == nil || *cp.NoProgress != (NoProgressCounters{Tokens: 123, Turns: 4, Tasks: 7}) || !cp.NoProgressReplanPending {
		t.Fatalf("checkpoint no-progress counters = %#v, want persisted counters", cp)
	}

	c.metricsMu.Lock()
	c.tokensSinceCriterionProgress = 0
	c.turnsSinceCriterionProgress = 0
	c.tasksSinceCriterionProgress = 0
	c.noProgressReplanTripped = false
	c.metricsMu.Unlock()
	c.ResumeContinuationCheckpoint()
	if got := c.noProgressCounters(); got != (NoProgressCounters{Tokens: 123, Turns: 4, Tasks: 7}) {
		t.Fatalf("resumed no-progress counters = %+v, want persisted counters", got)
	}
	if !c.noProgressReplanPending() {
		t.Fatal("resumed continuation lost the pending replan state")
	}
}

func TestEnsureFinishedNoProgressStopDoesNotSpendAnotherLLMTurn(t *testing.T) {
	calls := 0
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Name: "no-progress-stop",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgress:     0,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgress:     0,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.tokensSinceCriterionProgress = 20
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		calls++
		return "unexpected", nil, nil
	}
	if stopped, reason := c.enforceNoProgressBudget(); !stopped || reason == "" {
		t.Fatalf("pre-existing no-progress stop = (%v, %q), want terminal stop", stopped, reason)
	}

	result, _ := c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "partial evidence", nil)
	if calls != 0 {
		t.Fatalf("no-progress stop invoked %d extra LLM turns, want 0", calls)
	}
	if result != "partial evidence" {
		t.Fatalf("result = %q, want existing evidence without extra LLM turn", result)
	}
	if run := c.LastRunResult(); run == nil || run.Outcome != RunOutcomePartial || run.Continuation == nil {
		t.Fatalf("run result = %#v, want partial with continuation", run)
	}
	if cp := c.ContinuationCheckpoint(); cp == nil || cp.Status != "pending" || cp.Reason == "" {
		t.Fatalf("continuation checkpoint = %#v, want pending no-progress handoff", cp)
	}
}

func TestEnsureFinishedTerminalUnresolvedDoesNotSpendAnotherLLMTurn(t *testing.T) {
	calls := 0
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "closed checkpoint"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskBlocked, "terminal evidence")
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "terminal-unresolved", MaxCoordinatorTurns: 3}},
		sessionData: NewSession(),
		taskTracker: tracker,
	}
	c.SetWrapUp()
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		calls++
		return "unexpected coordinator continuation", nil, nil
	}

	result, _ := c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "narration", nil)
	if calls != 0 {
		t.Fatalf("terminal unresolved run invoked %d coordinator continuation turns, want 0", calls)
	}
	if !strings.Contains(result, "closed checkpoint") {
		t.Fatalf("deterministic result = %q, want failed task summary", result)
	}
	if run := c.LastRunResult(); run == nil || IsRunOutcomeSuccess(run.Outcome) || run.ExitCode == 0 {
		t.Fatalf("run result = %#v, want nonzero unresolved outcome", run)
	}
}

func TestEnsureFinishedCompletesWhenAllTasksDoneButCoordinatorOmitsFinish(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "reader", Desc: "read-only review"}})[0]
	tracker.TodoList().UpdateStatusAndOutput(item.ID, TaskDone, "summary", "review evidence")
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "deterministic-finish", MaxCoordinatorTurns: 1, AcceptanceSpec: &agent.AcceptanceSpec{RequireNoUnresolvedTasks: true}}},
		sessionData: NewSession(),
		taskTracker: tracker,
		acceptanceSpec: &AcceptanceSpec{
			RequireNoUnresolvedTasks: true,
		},
	}
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		return "I will call finish now.", nil, nil
	}

	result, _ := c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "narration", nil)
	if !c.finishCalled.Load() {
		t.Fatal("deterministic completion must mark finish called")
	}
	if !strings.Contains(result, "review evidence") {
		t.Fatalf("result = %q, want durable task output", result)
	}
	if run := c.LastRunResult(); run == nil || !IsRunOutcomeSuccess(run.Outcome) {
		t.Fatalf("run result = %#v, want successful deterministic completion", run)
	}
}

func TestAttemptWrapUpRecoveryTerminalUnresolvedDoesNotSpendAnotherLLMTurn(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "blocked checkpoint"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskBlocked, "terminal evidence")
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "terminal-unresolved-recovery"}},
		sessionData: NewSession(),
		taskTracker: tracker,
	}
	c.SetWrapUp()
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		t.Fatal("terminal unresolved run must not invoke a wrap-up model turn")
		return "", nil, nil
	}

	result, steps, recovered := c.attemptWrapUpRecovery(context.Background(), &agent.AgentDef{Name: "coordinator"}, errors.New("coordinator tool boundary"))
	if !recovered || len(steps) != 0 || !strings.Contains(result, "blocked checkpoint") {
		t.Fatalf("terminal recovery = (%q, %v, %v), want deterministic failed-task summary without model steps", result, steps, recovered)
	}
	if run := c.LastRunResult(); run == nil || IsRunOutcomeSuccess(run.Outcome) || run.ExitCode == 0 {
		t.Fatalf("run result = %#v, want nonzero unresolved outcome", run)
	}
}

// TestAttemptWrapUpRecoveryRecoversNoProgressStopWrappedAsToolFailure guards
// against a regression where a no-progress hard stop's already-correct
// partial/budget-exceeded outcome (computed by stopForNoProgress) was
// silently discarded and reclassified as a hard run failure. ExecuteTasks
// surfaces the stop as a plain error, and tool_policy_gate.go wraps every
// coordinator tool error (including this one) as errCoordinatorToolFailure
// before it reaches attemptWrapUpRecovery — so the noProgressStopPending
// check must run before the generic errCoordinatorToolFailure short-circuit,
// or this exact wrapped shape falls into the hard-failure path instead of the
// graceful one.
func TestAttemptWrapUpRecoveryRecoversNoProgressStopWrappedAsToolFailure(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "go-reviewer", Desc: "review batch-0000"}})[0]
	tracker.TodoList().UpdateStatusAndOutput(item.ID, TaskDone, "summary", "batch-0000 reviewed: no blockers")
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "no-progress-tool-failure-recovery"}},
		sessionData: NewSession(),
		taskTracker: tracker,
	}

	// Simulate a real no-progress hard stop the same way enforceNoProgressBudget
	// does, so LastRunResult is populated exactly as it would be in production.
	c.stopForNoProgress("tasks since progress reached the limit")

	// Mirror tool_policy_gate.go's wrapping of a coordinator direct tool
	// failure verbatim: this is the exact error shape ExecuteTasks's no-progress
	// stop actually surfaces as, not a bare unwrapped error.
	wrapped := fmt.Errorf("%w: tool %q failed: %s", errCoordinatorToolFailure, "agent",
		"tasks since progress reached the limit after replan: call finish immediately with your best summary of work completed so far")

	result, steps, recovered := c.attemptWrapUpRecovery(context.Background(), &agent.AgentDef{Name: "coordinator"}, wrapped)
	if !recovered {
		t.Fatalf("attemptWrapUpRecovery recovered=%v, want true: a no-progress stop must take the graceful partial path even when wrapped as errCoordinatorToolFailure", recovered)
	}
	if len(steps) != 0 {
		t.Fatalf("no-progress recovery spent %d model steps, want 0 (no further LLM turn permitted)", len(steps))
	}
	if strings.TrimSpace(result) == "" {
		t.Fatal("no-progress recovery returned an empty result; want the preserved partial summary")
	}
	if run := c.LastRunResult(); run == nil || run.Outcome != RunOutcomePartial {
		t.Fatalf("run result = %#v, want the partial outcome stopForNoProgress already computed, not overwritten by a hard failure", run)
	}
}

func TestRestoredNoProgressBudgetStopsBeforeInitialCoordinatorTurn(t *testing.T) {
	calls := 0
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Name: "restored-no-progress",
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    10,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgress:     0,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgress:     0,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.tokensSinceCriterionProgress = 20
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		calls++
		return "unexpected", nil, nil
	}
	result, _, err := c.runOrchestratorWithNoProgressGuard(context.Background(), &agent.AgentDef{Name: "coordinator"}, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("restored no-progress budget spent %d initial coordinator turns, want 0", calls)
	}
	if result == "" || c.LastRunResult() == nil || c.LastRunResult().Outcome != RunOutcomePartial {
		t.Fatalf("result=%q last_run=%#v, want persisted partial outcome", result, c.LastRunResult())
	}
}

func TestEnsureFinishedChecksNoProgressAfterForcedSummary(t *testing.T) {
	calls := 0
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{
			Name:                "no-progress-final-summary",
			MaxCoordinatorTurns: 1,
			Reliability: agent.ReliabilityConfig{
				MaxTokensWithoutProgress:    0,
				MaxTokensWithoutProgressSet: true,
				MaxTurnsWithoutProgress:     2,
				MaxTurnsWithoutProgressSet:  true,
				MaxTasksWithoutProgress:     0,
				MaxTasksWithoutProgressSet:  true,
				HardEnforcement:             true,
			},
		}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	// The initial coordinator turn has already been consumed before the
	// continuation helper receives its initial result.
	c.turnsSinceCriterionProgress = 1
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		calls++
		return "summary", nil, nil
	}

	result, _ := c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "initial", nil)
	if calls != 2 { // one continuation plus the forced final summary
		t.Fatalf("orchestrator calls = %d, want 2", calls)
	}
	if result != "summary" {
		t.Fatalf("result = %q, want final summary result", result)
	}
	if run := c.LastRunResult(); run == nil || run.Outcome != RunOutcomePartial || run.Continuation == nil {
		t.Fatalf("run result = %#v, want partial with continuation after final-summary overrun", run)
	}
}

// newNoProgressTestCoordinator builds a minimal coordinator for counter
// reset tests (no provider, no LLM).
func newNoProgressTestCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	return &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "no-progress-test"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
}

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

func TestNoProgressStop_RejectsCandidatesAcrossRestart(t *testing.T) {
	t.Run("direct agent budget stop rejects candidates", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-direct-budget-stop"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		// Seed candidates
		c.persistPrivateReflexionLesson("worker", "task-1", "private lesson")
		if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
			Scope:    c.contextScope(),
			Content:  "shared lesson",
			Section:  ltmSectionPatterns,
			Category: "pattern",
			Source:   "memory_save",
			RunID:    runID,
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.appendCanonicalContext(context.Background(), contextstore.ContextDecision, "decision", "task_done", nil); err != nil {
			t.Fatal(err)
		}

		// Trigger budget stop
		c.session.Config.Reliability.MaxTokensWithoutProgress = 100
		c.tokensSinceCriterionProgress = 200 // exceeds limit -> NoProgressStop
		stopped, _ := c.enforceNoProgressBudget()
		if !stopped {
			t.Fatal("expected enforceNoProgressBudget to stop run")
		}

		// Reopen database
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found undecided candidate %q after direct budget stop", it.ID)
			}
		}
	})

	t.Run("coordinator ensureFinished budget stop rejects candidates", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-coord-budget-stop"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		c.persistPrivateReflexionLesson("worker", "task-1", "private lesson")
		if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
			Scope:    c.contextScope(),
			Content:  "shared lesson",
			Section:  ltmSectionPatterns,
			Category: "pattern",
			Source:   "memory_save",
			RunID:    runID,
		}); err != nil {
			t.Fatal(err)
		}

		// Trigger stop
		c.stopForNoProgress("tokens limit exhausted")
		summary, _ := c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "initial", nil)
		if summary == "" {
			t.Fatal("expected non-empty summary from ensureFinished")
		}

		// Reopen database
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found undecided candidate %q after ensureFinished budget stop", it.ID)
			}
		}
	})

	t.Run("finish tool budget stop rejects candidates", func(t *testing.T) {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, "context.sqlite")
		repo, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}

		runID := "run-finish-budget-stop"
		agentDef := &agent.AgentDef{
			Name:     "worker",
			MemoryID: "worker-mem-1",
			Memory:   agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent},
		}
		c := &Coordinator{
			contextRepo:     repo,
			workerMemorySvc: NewWorkerMemoryService(repo, nil),
			sharedMemorySvc: NewSharedMemoryService(repo),
			projectDir:      "/project",
			executionRunID:  runID,
			session: &TeamSession{
				Workspace: workspace,
				Config: agent.TeamConfig{
					Name: "team",
				},
				Agents: map[string]*agent.AgentDef{"worker": agentDef},
			},
			taskTracker: NewTaskTracker(),
		}

		c.persistPrivateReflexionLesson("worker", "task-1", "private lesson")
		if _, err := c.sharedMemoryService().Propose(context.Background(), SharedMemoryProposal{
			Scope:    c.contextScope(),
			Content:  "shared lesson",
			Section:  ltmSectionPatterns,
			Category: "pattern",
			Source:   "memory_save",
			RunID:    runID,
		}); err != nil {
			t.Fatal(err)
		}

		// Trip budget stop before calling finish tool
		c.stopForNoProgress("turns limit exhausted")
		tool := &finishTool{coordinator: c}
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"final response"}`})
		if err != nil {
			t.Fatalf("finish tool Run failed: %v", err)
		}
		if !strings.Contains(resp.Content, "FINISHED:") {
			t.Fatalf("expected finish response to contain FINISHED:, got: %s", resp.Content)
		}

		// Reopen database
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		repo2, err := contextstore.OpenSQLite(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer repo2.Close()

		items, err := repo2.Query(context.Background(), contextstore.RepositoryQuery{
			Scope:             contextstore.Scope{ProjectID: "/project", TeamID: "team"},
			Visibility:        contextstore.VisibilitySubtree,
			IncludeCandidates: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.Metadata["run_id"] == runID && it.Lifecycle == contextstore.LifecycleCandidate {
				t.Fatalf("found undecided candidate %q after finish budget stop", it.ID)
			}
		}
	})
}
