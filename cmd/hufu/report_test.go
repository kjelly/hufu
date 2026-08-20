package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestGenerateRequestedReportsWritesOnlyAutoReportTeams(t *testing.T) {
	originalReportMode := opts.reportMode
	opts.reportMode = false
	t.Cleanup(func() { opts.reportMode = originalReportMode })

	autoWorkspace := t.TempDir()
	otherWorkspace := t.TempDir()
	loadedTeams := map[string]*teamContext{
		"auto": {
			session: &team.TeamSession{Workspace: autoWorkspace, Config: agent.TeamConfig{AutoReport: true}},
		},
		"other": {
			session: &team.TeamSession{Workspace: otherWorkspace, Config: agent.TeamConfig{}},
		},
	}

	generateRequestedReports(loadedTeams, "review complete")
	if _, err := os.Stat(filepath.Join(autoWorkspace, "report.md")); err != nil {
		t.Fatalf("auto-report team did not write report.md: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherWorkspace, "report.md")); !os.IsNotExist(err) {
		t.Fatalf("non-auto-report team unexpectedly wrote report.md: %v", err)
	}
}

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
		Requests: 2, ModelCalls: 1, Fallbacks: 1, Included: 5, Omitted: 3, IncludedTokens: 120, OmittedTokens: 80,
		OmitReasons: map[string]int{"phase_mismatch": 1, "token_budget": 2}, Purposes: map[string]int{"task_execution": 1, "skill_matcher": 1},
	}}, "demo", "")
	for _, want := range []string{"## Context Routing", "Requests:** 2", "Model calls:** 1", "Deterministic fallbacks:** 1", "Included items:** 5 (120 tokens)", "Omitted items:** 3 (80 tokens)", "`phase_mismatch`: 1", "`token_budget`: 2", "`task_execution`: 1", "`skill_matcher`: 1"} {
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

// WP-0 fixes the baseline report projection for a generic batch. It must show
// individual child state and failed objective evidence without depending on a
// consumer-specific item name or presentation format.
func TestCharacterizationReportProjectsGenericChildStates(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now().Add(-time.Minute),
		Todos: []*team.TodoItem{
			{ID: "1", Agent: "worker", Desc: "process alpha", Status: team.TaskDone},
			{ID: "2", Agent: "worker", Desc: "process beta", Status: team.TaskError, VerifyResult: &team.VerificationResult{
				Spec: &team.VerificationSpec{Type: team.VerifyFileExists, Path: "outputs/beta.txt"}, Command: "test -e outputs/beta.txt", ExitCode: 1, Stderr: "missing output",
			}},
			{ID: "3", Agent: "worker", Desc: "process gamma", Status: team.TaskPending},
		},
	}
	report := buildReportMD(data, "", "")

	taskSummaryStart := strings.Index(report, "## Task Summary\n")
	if taskSummaryStart < 0 {
		t.Fatalf("report missing task summary:\n%s", report)
	}
	taskSummaryEnd := strings.Index(report[taskSummaryStart:], "\n---\n\n")
	if taskSummaryEnd < 0 {
		t.Fatalf("task summary has no stable end boundary:\n%s", report)
	}
	taskSummary := report[taskSummaryStart : taskSummaryStart+taskSummaryEnd]
	for _, want := range []string{
		"| 1 | ● | worker | process alpha |",
		"| 2 | ✗ | worker | process beta |",
		"| 3 | ○ | worker | process gamma |",
	} {
		if !strings.Contains(taskSummary, want) {
			t.Fatalf("task summary missing canonical task state/description %q:\n%s", want, taskSummary)
		}
	}

	rowFor := func(id string) string {
		marker := "| " + id + " |"
		rowStart := strings.Index(taskSummary, marker)
		if rowStart < 0 {
			return ""
		}
		rowEnd := strings.IndexByte(taskSummary[rowStart:], '\n')
		if rowEnd < 0 {
			return taskSummary[rowStart:]
		}
		return taskSummary[rowStart : rowStart+rowEnd]
	}
	for _, id := range []string{"1", "3"} {
		row := rowFor(id)
		if strings.Contains(row, "outputs/beta.txt") || strings.Contains(row, "missing output") {
			t.Fatalf("task %s row contains beta verification evidence: %s", id, row)
		}
	}

	evidenceStart := strings.Index(report, "## Verification Evidence\n")
	if evidenceStart < 0 {
		t.Fatalf("report missing verification evidence section:\n%s", report)
	}
	betaMarker := "### Task 2: process beta\n"
	betaStartRel := strings.Index(report[evidenceStart:], betaMarker)
	if betaStartRel < 0 {
		t.Fatalf("verification evidence missing beta task block:\n%s", report[evidenceStart:])
	}
	betaStart := evidenceStart + betaStartRel
	betaEndRel := strings.Index(report[betaStart+len(betaMarker):], "\n---\n\n")
	if betaEndRel < 0 {
		t.Fatalf("beta verification block has no stable end boundary:\n%s", report[betaStart:])
	}
	betaBlock := report[betaStart : betaStart+len(betaMarker)+betaEndRel]
	for _, want := range []string{"outputs/beta.txt", "missing output"} {
		if !strings.Contains(betaBlock, want) {
			t.Fatalf("beta verification block missing %q:\n%s", want, betaBlock)
		}
	}
}

func TestCharacterizationReportProjectsCancelledBudgetRunResult(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now(),
		RunResult: &team.RunResult{
			Outcome:       team.RunOutcomeCancelled,
			GoalSatisfied: false,
			GoalMode:      team.GoalModeOutcome,
			StopReason:    team.StopReasonBudgetExceeded,
			Stats:         team.RunStats{TasksUnresolved: 3},
		},
	}

	report := buildReportMD(data, "", "")
	for _, want := range []string{"`cancelled`", "Goal satisfied:** `false`", "Stop reason:** `budget_exceeded`", "Tasks unresolved:** 3"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing canonical cancelled/budget projection %q:\n%s", want, report)
		}
	}
}

// TestBuildReportMDAggregatesTaskFindings guards against a real reporting
// gap: submit_result's structured findings were persisted to
// session.json/JSONL but never rendered into report.md, which only ever
// showed a boolean "findings are present" line. A reader had no way to see
// what was actually found without opening the raw session/task-output files.
func TestBuildReportMDAggregatesTaskFindings(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now().Add(-1 * time.Minute),
		Todos: []*team.TodoItem{
			{
				ID:     "2",
				Agent:  "go-reviewer",
				Desc:   "Review batch-0000",
				Status: team.TaskDone,
				TypedResult: &team.TaskResult{
					Status:  "completed_with_gaps",
					Summary: "reviewed batch-0000",
					Findings: []team.Finding{
						{Category: "WARNING", Summary: "unsynchronized map access", Detail: "coordinator_eventstore.go:42"},
						{Category: "NOTE", Summary: "missing doc comment"},
					},
				},
			},
			{
				ID:     "1",
				Agent:  "inventory",
				Desc:   "inventory",
				Status: team.TaskDone,
				TypedResult: &team.TaskResult{
					Status:   "success",
					Summary:  "inventory done",
					Findings: []team.Finding{{Summary: "review range covers 32 commits"}},
				},
			},
		},
	}

	report := buildReportMD(data, "demo", "finished")

	if !strings.Contains(report, "## Review Findings") {
		t.Fatalf("report missing findings section:\n%s", report)
	}
	if !strings.Contains(report, "[WARNING]") || !strings.Contains(report, "unsynchronized map access") || !strings.Contains(report, "coordinator_eventstore.go:42") {
		t.Fatalf("report missing detailed finding content:\n%s", report)
	}
	if !strings.Contains(report, "[NOTE]") || !strings.Contains(report, "missing doc comment") {
		t.Fatalf("report missing finding without detail:\n%s", report)
	}
	if strings.Contains(report, "[]") {
		t.Fatalf("report rendered an empty category label:\n%s", report)
	}
	if !strings.Contains(report, "- review range covers 32 commits") {
		t.Fatalf("report missing uncategorized finding:\n%s", report)
	}
}

func TestBuildReportMDOmitsFindingsSectionWhenNoTaskHasFindings(t *testing.T) {
	data := &reportData{
		StartedAt: time.Now().Add(-1 * time.Minute),
		Todos: []*team.TodoItem{
			{ID: "1", Agent: "inventory", Desc: "inventory", Status: team.TaskDone},
		},
	}

	report := buildReportMD(data, "demo", "finished")

	if strings.Contains(report, "## Review Findings") {
		t.Fatalf("report rendered an empty findings section:\n%s", report)
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

func TestLatestRunTodosExcludesOnlyReceiptProvenHistoricalTasks(t *testing.T) {
	current := &team.TodoItem{ID: "current", ExecutionReceipts: []team.ExecutionReceipt{{RunID: "run-current"}}}
	historical := &team.TodoItem{ID: "historical", ExecutionReceipts: []team.ExecutionReceipt{{RunID: "run-old"}}}
	unknown := &team.TodoItem{ID: "unknown"}
	artifact := team.ArtifactRef{ID: "sha256-transcript", RunID: "run-current", TaskID: "current", Attempt: 1}
	manifest := &team.EvidenceManifest{RunID: "run-current", ArtifactRefs: []team.ArtifactRef{artifact}, EvidenceResults: []team.EvidenceResult{{
		RequirementID: "task:current", Status: "passed", ArtifactRefs: []team.ArtifactRef{artifact}, Binding: &team.EvidenceBinding{
			RunID: "run-current", TaskID: "current", Attempt: 1, ModelExecutionID: "model-1", ProducerID: "worker", TranscriptRef: "sha256-transcript", ArtifactIDs: []string{"sha256-transcript"},
		},
	}}}
	got, historicalCount := latestRunTodos([]*team.TodoItem{current, historical, unknown}, manifest)
	if historicalCount != 1 || len(got) != 2 || got[0].ID != "current" || got[1].ID != "unknown" {
		t.Fatalf("latest tasks=%#v historical=%d", got, historicalCount)
	}
	report := buildReportMD(&reportData{StartedAt: time.Now(), Todos: got, HistoricalTodoCount: historicalCount}, "demo", "")
	if !strings.Contains(report, "## Historical Runs") || strings.Contains(report, "historical |") {
		t.Fatalf("latest-run report separation failed:\n%s", report)
	}
}

func TestLatestRunTodosRetainsCurrentFailedTaskWithoutVerifiedBinding(t *testing.T) {
	currentFailed := &team.TodoItem{
		ID:     "failed-current",
		Status: team.TaskError,
		ExecutionReceipts: []team.ExecutionReceipt{{
			RunID: "run-current", TaskID: "failed-current", Attempt: 1,
		}},
	}
	historical := &team.TodoItem{
		ID: "old",
		ExecutionReceipts: []team.ExecutionReceipt{{
			RunID: "run-old", TaskID: "old", Attempt: 1,
		}},
	}
	manifest := &team.EvidenceManifest{RunID: "run-current"}
	got, historicalCount := latestRunTodos([]*team.TodoItem{currentFailed, historical}, manifest)
	if historicalCount != 1 || len(got) != 1 || got[0].ID != currentFailed.ID {
		t.Fatalf("current failed task was filtered: tasks=%#v historical=%d", got, historicalCount)
	}
}

func TestCurrentRunDiagnosticsOmitsAmbiguousWinnerBinding(t *testing.T) {
	item := &team.TodoItem{ID: "task-1", Agent: "worker", ExecutionReceipts: []team.ExecutionReceipt{
		{RunID: "run-current", TaskID: "task-1", Attempt: 1, ModelExecutionID: "model-a", TranscriptRef: "sha256-a"},
		{RunID: "run-current", TaskID: "task-1", Attempt: 1, ModelExecutionID: "model-b", TranscriptRef: "sha256-b"},
	}}
	manifest := &team.EvidenceManifest{RunID: "run-current", EvidenceResults: []team.EvidenceResult{{
		RequirementID: "task:task-1", Status: "passed", Binding: &team.EvidenceBinding{
			RunID: "run-current", TaskID: "task-1", Attempt: 1, ModelExecutionID: "model-a", ProducerID: "worker", TranscriptRef: "sha256-a", ArtifactIDs: []string{"sha256-a"},
		},
	}}}
	if diagnostics := currentRunReportDiagnostics([]*team.TodoItem{item}, manifest); len(diagnostics) != 0 {
		t.Fatalf("ambiguous winner produced diagnostics: %#v", diagnostics)
	}
}

func TestBuildReviewReportMovesDiagnosticsToAppendix(t *testing.T) {
	report := buildReportMD(&reportData{
		StartedAt:   time.Now(),
		STM:         "stale model observation",
		TaskHistory: map[string]string{"reviewer": "transcript"},
		RunResult:   &team.RunResult{CompletedReview: true},
	}, "review", "VERDICT: PASS")
	for _, want := range []string{"## Appendix: Session Context (STM)", "## Appendix: Agent Task Transcripts"} {
		if !strings.Contains(report, want) {
			t.Fatalf("review report missing %q:\n%s", want, report)
		}
	}
}

func TestBuildReviewReportFreshSessionOmitsHistoricalTaskTranscripts(t *testing.T) {
	report := buildReportMD(&reportData{
		StartedAt:       time.Now(),
		TaskHistory:     map[string]string{"reviewer": "old policy repair failure"},
		ResolvedProfile: team.ExecutionProfile{DisableHistoricalTaskReuse: true},
		RunResult:       &team.RunResult{CompletedReview: true},
	}, "review", "VERDICT: PASS")
	if strings.Contains(report, "## Appendix: Agent Task Transcripts") || strings.Contains(report, "old policy repair failure") {
		t.Fatalf("fresh-session report included historical task transcript:\n%s", report)
	}
}

func TestGatherReportDataFreshDoesNotReadTaskMarkdown(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "tasks", "review", "worker"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tasks", "review", "worker", "old.md"), []byte("historical secret diagnostic"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := &team.EvidenceManifest{RunID: "run-current", Status: "accepted"}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	coordinator := &team.Coordinator{}
	coordinator.SetTaskTracker(team.NewTaskTracker())
	coordinator.SetExecutionProfile(team.ExecutionProfile{Name: team.ProfileFreshSession, DisableHistoricalTaskReuse: true})
	coordinator.SetLastRunResult(&team.RunResult{EvidenceManifest: manifest})
	data := gatherReportData(&teamContext{
		teamName:    "review",
		session:     &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "review"}},
		coordinator: coordinator,
	}, "review")
	if len(data.TaskHistory) != 0 || strings.Contains(data.STM, "historical secret diagnostic") {
		t.Fatalf("fresh report read historical task markdown: %#v", data.TaskHistory)
	}
}

func TestGatherReportDataFreshIgnoresUnverifiedManifestRunID(t *testing.T) {
	tests := []struct {
		name     string
		manifest func() *team.EvidenceManifest
	}{
		{
			name: "unsealed",
			manifest: func() *team.EvidenceManifest {
				return &team.EvidenceManifest{RunID: "run-current", Status: "accepted"}
			},
		},
		{
			name: "hash-invalid",
			manifest: func() *team.EvidenceManifest {
				manifest := &team.EvidenceManifest{RunID: "run-current", Status: "accepted"}
				if err := manifest.Seal(); err != nil {
					t.Fatalf("seal manifest: %v", err)
				}
				manifest.ManifestHash = "invalid"
				return manifest
			},
		},
		{
			name: "artifact-invalid",
			manifest: func() *team.EvidenceManifest {
				manifest := &team.EvidenceManifest{
					RunID: "run-current", Status: "accepted",
					ArtifactRefs: []team.ArtifactRef{{ID: "sha256-missing", SHA256: strings.Repeat("0", 64), ByteSize: 1}},
				}
				if err := manifest.Seal(); err != nil {
					t.Fatalf("seal manifest: %v", err)
				}
				return manifest
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			tracker := team.NewTaskTracker()
			items := tracker.TodoList().AddBatch([]team.TodoSpec{
				{Agent: "worker", Desc: "current task"},
				{Agent: "worker", Desc: "historical task"},
			})
			items[0].ExecutionReceipts = []team.ExecutionReceipt{{RunID: "run-current"}}
			items[1].ExecutionReceipts = []team.ExecutionReceipt{{RunID: "run-old"}}

			coordinator := &team.Coordinator{}
			coordinator.SetTaskTracker(tracker)
			coordinator.SetExecutionProfile(team.ExecutionProfile{Name: team.ProfileFreshSession, DisableHistoricalTaskReuse: true})
			coordinator.SetLastRunResult(&team.RunResult{EvidenceManifest: tt.manifest()})
			data := gatherReportData(&teamContext{
				teamName:    "review",
				session:     &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "review"}},
				coordinator: coordinator,
			}, "review")

			if len(data.Todos) != 2 || data.HistoricalTodoCount != 0 {
				t.Fatalf("unverified manifest was used for task filtering: todos=%d historical=%d", len(data.Todos), data.HistoricalTodoCount)
			}
			if len(data.CurrentRunDiagnostics) != 0 {
				t.Fatalf("unverified manifest produced current-run diagnostics: %#v", data.CurrentRunDiagnostics)
			}
		})
	}
}
