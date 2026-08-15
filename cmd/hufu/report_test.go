package main

import (
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestReportRendersContentFreeDeprecatedMemoryUsage(t *testing.T) {
	report := buildReportMD(&reportData{StartedAt: time.Now(), DeprecatedMemory: []team.DeprecatedMemoryToolUsage{{Tool: "stm_write", Calls: 2, Success: 1, FailClosed: 1, Denied: 3}}}, "demo", "")
	for _, want := range []string{"## Deprecated Memory Compatibility Usage", "`stm_write`", "| 2 | 1 | 1 | 3 |", "Only content-free lifecycle counts"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q: %s", want, report)
		}
	}
}

func TestReportRendersContentFreeContextRoutingAggregate(t *testing.T) {
	report := buildReportMD(&reportData{StartedAt: time.Now(), ContextRouting: team.ContextManifestSummary{
		Requests: 2, Included: 5, Omitted: 3, IncludedTokens: 120, OmittedTokens: 80,
		OmitReasons: map[string]int{"phase_mismatch": 1, "token_budget": 2},
	}}, "demo", "")
	for _, want := range []string{"## Context Routing", "Requests:** 2", "Included items:** 5 (120 tokens)", "Omitted items:** 3 (80 tokens)", "`phase_mismatch`: 1", "`token_budget`: 2"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q: %s", want, report)
		}
	}
	for _, forbidden := range []string{"prompt content", "memory body", "tool input"} {
		if strings.Contains(report, forbidden) {
			t.Fatalf("context routing aggregate exposed content %q: %s", forbidden, report)
		}
	}
}

func TestBuildReportMDIncludesVerificationEvidence(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now().Add(-2 * time.Minute),
		Todos: []*team.TodoItem{
			{
				ID:     "1",
				Agent:  "researcher",
				Desc:   "Produce report",
				Status: team.TaskDone,
				Verify: "test -f report.md",
				VerifyResult: &team.VerificationResult{
					Command:  "test -f report.md",
					WorkDir:  "/tmp/project",
					ExitCode: 0,
					Stdout:   "ok",
					Stderr:   "",
					Duration: 2 * time.Second,
					TimedOut: false,
				},
			},
		},
	}

	report := buildReportMD(data, "demo", "finished")

	if !strings.Contains(report, "## Verification Evidence") {
		t.Fatalf("report missing verification section:\n%s", report)
	}
	if !strings.Contains(report, "test -f report.md") {
		t.Fatalf("report missing verify command:\n%s", report)
	}
	if !strings.Contains(report, "Stdout") || !strings.Contains(report, "ok") {
		t.Fatalf("report missing verify stdout:\n%s", report)
	}
	if !strings.Contains(report, "Working directory") || !strings.Contains(report, "/tmp/project") {
		t.Fatalf("report missing verify working directory:\n%s", report)
	}
	if !strings.Contains(report, "Verify") {
		t.Fatalf("task summary missing verify column:\n%s", report)
	}
}

func TestBuildReportMDIncludesCanonicalRunOutcome(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now(),
		RunResult: &team.RunResult{
			Outcome:       team.RunOutcomePartial,
			GoalSatisfied: false,
			GoalMode:      team.GoalModeOutcome,
			StopReason:    team.StopReasonAcceptanceFailed,
			Acceptance:    &team.AcceptanceResult{State: team.AcceptanceFailed, Passed: false},
			Stats:         team.RunStats{TasksUnresolved: 2},
			Metrics:       team.RunMetrics{DiagnosticTasksSinceProgress: 3, RepeatedFailureFingerprints: 2, RecoveryStrategyChanges: 1, TimeSinceCriterionProgressSeconds: 45, TokensSinceCriterionProgress: 123},
		},
	}
	report := buildReportMD(data, "demo", "partial")
	for _, want := range []string{"## Run Outcome", "`partial`", "Goal satisfied:** `false`", "Goal mode:** `outcome`", "Stop reason:** `acceptance_failed`", "Tasks unresolved:** 2", "Acceptance:** `failed`", "Diagnostic tasks since criterion progress:** 3", "Repeated failure fingerprints:** 2", "Recovery strategy changes:** 1", "Time since criterion progress:** 45s", "Tokens since criterion progress:** 123"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
}

func TestBuildReportMDRendersTerminalCleanupWithoutOutput(t *testing.T) {
	report := buildReportMD(&reportData{StartedAt: time.Now(), TerminalSessions: []team.TerminalSession{{
		ID: "terminal-1", OwnerTaskID: "task-1", State: team.TerminalSessionClosed,
		Custodian: team.TerminalCustodianCoordinator, CleanupState: team.TerminalCleanupCompleted,
		OutputRefs: []team.ArtifactRef{{Path: "logs/terminal/terminal-1.log", Type: "terminal_output", Description: "raw secret output"}},
	}}}, "demo", "")
	for _, want := range []string{"## Terminal Session Cleanup", "Automatically contained; safe to retry.", "Terminal output is retained only"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	for _, forbidden := range []string{"logs/terminal/terminal-1.log", "raw secret output"} {
		if strings.Contains(report, forbidden) {
			t.Fatalf("report exposed terminal output reference/content %q:\n%s", forbidden, report)
		}
	}
}

func TestBuildReportMDWarnsWhenRunHasNoTypedVerifiers(t *testing.T) {
	data := &reportData{StartedAt: time.Now(), RunResult: &team.RunResult{
		Metrics: team.RunMetrics{TasksWithVerifier: 2, TypedVerifiers: 0},
	}}
	report := buildReportMD(data, "demo", "partial")
	if !strings.Contains(report, "No typed verifiers were used") {
		t.Fatalf("report missing typed-adoption warning:\n%s", report)
	}
}

func TestReliabilityProjectionsIncludeFailureMaps(t *testing.T) {
	metrics := team.RunMetrics{
		FailuresByClass: map[team.TaskFailureClass]int{team.FailureExecution: 2},
		FailuresByPhase: map[string]int{"verification": 1},
	}
	report := buildReportMD(&reportData{StartedAt: time.Now(), RunResult: &team.RunResult{Metrics: metrics}}, "demo", "partial")
	for _, want := range []string{"Failures by class", "execution:2", "Failures by phase", "verification:1"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}

	workspace := t.TempDir()
	if err := team.SaveSession(workspace, &team.SessionData{RunResult: &team.RunResult{Metrics: metrics}}); err != nil {
		t.Fatal(err)
	}
	fixData := collectFixData(&team.TeamSession{Workspace: workspace}, "")
	for _, want := range []string{"failures_by_class=map[execution:2]", "failures_by_phase=map[verification:1]"} {
		if !strings.Contains(fixData.Reliability, want) {
			t.Fatalf("fix reliability context missing %q: %s", want, fixData.Reliability)
		}
	}
}

func TestBuildReportMDIncludesWorkerMemoryStatsAndIDsWithoutContent(t *testing.T) {
	report := buildReportMD(&reportData{
		StartedAt: time.Now(),
		WorkerMemory: team.WorkerMemoryReport{
			ItemIDs:    []string{"memory-a", "memory-b"},
			Total:      2,
			Session:    1,
			Persistent: 1,
			Confirmed:  1,
			Candidate:  1,
		},
	}, "demo", "")
	for _, want := range []string{"## Worker Memory", "memory-a", "memory-b", "Items:** 2", "Private worker-memory content is intentionally omitted"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "private worker memory report secret") {
		t.Fatalf("report leaked worker-memory content:\n%s", report)
	}
}
