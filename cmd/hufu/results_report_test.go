package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func TestResultsReportUsesSelectedRunAndExportsSavedSnapshot(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	path := filepath.Join(t.TempDir(), "saved.md")
	first, text, err := executeResultsTest(t, deps, "report", ids[0], "--workspace", workspace, "--team", "review", "--export", path, "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if first.Report == nil || first.Report.RunID != "run-review" || first.Report.Team != "review" || len(first.Report.Tasks) != 3 || !strings.Contains(first.ReportMarkdown, "coordinator final response") || !strings.Contains(first.ReportMarkdown, "請調查原始連線逾時問題") || strings.Contains(text, "OTHER_TEAM_SENTINEL") {
		t.Fatalf("wrong report: %s", text)
	}
	if first.Report.MetricsAvailable || first.Report.StatsAvailable || !strings.Contains(first.ReportMarkdown, "metrics were not captured") || strings.Contains(first.ReportMarkdown, "Acceptance criteria passed:** 0") {
		t.Fatal("uncaptured metrics were presented as measured zeros")
	}
	exported, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(exported) != first.ReportMarkdown {
		t.Fatal("export diverged from saved report")
	}
	second, _, err := executeResultsTest(t, deps, "report", ids[1], "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if second.ReportMarkdown != first.ReportMarkdown || second.Report.SourceEventID != first.Report.SourceEventID {
		t.Fatal("same-run report is not stable")
	}
	_, body, err := executeResultsTest(t, deps, "report", "--workspace", workspace, "--team", "review", "--render", "markdown")
	if err != nil || body != first.ReportMarkdown {
		t.Fatalf("text report mismatch: %v, %s", err, body)
	}
	// The default final response stays unchanged.
	_, body, err = executeResultsTest(t, deps, "--workspace", workspace, "--team", "review")
	if err != nil || strings.TrimSpace(body) != "coordinator final response" {
		t.Fatalf("final behavior changed: %v %s", err, body)
	}
}

func TestResultsPickReportUsesScopedSelection(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	deps.pick = func(_ context.Context, _ io.Reader, _ io.Writer, rows []inspectpkg.HistoricalResult, _ string) (int, error) {
		if len(rows) != 1 || rows[0].Kind != "run_response" || rows[0].Team != "review" {
			t.Fatalf("wrong picker candidates: %+v", rows)
		}
		return 0, nil
	}
	response, text, err := executeResultsTest(t, deps, "pick", "--view", "report", "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if response.Report == nil || !strings.Contains(response.ReportMarkdown, "Task Summary") || strings.Contains(text, "OTHER_TEAM_SENTINEL") {
		t.Fatalf("wrong picked report: %s", text)
	}
	for _, args := range [][]string{{"pick", "--tasks", "--view", "report"}, {"pick", "--view", "unknown"}, {"report", "--agent", "helper"}} {
		args = append(args, "--workspace", workspace, "--team", "review")
		if _, _, err := executeResultsTest(t, deps, args...); err == nil {
			t.Fatalf("accepted invalid flags: %v", args)
		}
	}
}

func TestResultsReportMissingSnapshotFailsWithoutExport(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	deps.report = func(context.Context, string, string) (*team.RunReport, error) {
		return nil, team.ErrRunReportUnavailable
	}
	target := filepath.Join(t.TempDir(), "report.md")
	_, out, err := executeResultsTest(t, deps, "report", "--workspace", workspace, "--team", "review", "--export", target)
	if !errors.Is(err, team.ErrRunReportUnavailable) || out != "" {
		t.Fatalf("missing report fallback: %v %s", err, out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("missing snapshot produced export: %v", err)
	}
}

func TestResultsReportRejectsMismatchedSnapshotAndDatabaseExport(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	deps.report = func(ctx context.Context, w, event string) (*team.RunReport, error) {
		report, err := team.LoadRunReport(ctx, w, event)
		if err == nil {
			report.RunID = "wrong-run"
		}
		return report, err
	}
	if _, _, err := executeResultsTest(t, deps, "report", "--workspace", workspace, "--team", "review"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("accepted mismatched report: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(workspace, runtimestore.File))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(workspace, runtimestore.File), filepath.Join(t.TempDir(), "context.sqlite-wal")} {
		if _, _, err := executeResultsTest(t, resultsTestDeps(t), "report", "--workspace", workspace, "--team", "review", "--export", path); err == nil {
			t.Fatal("allowed database overwrite")
		}
	}
	after, err := os.ReadFile(filepath.Join(workspace, runtimestore.File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read/export changed database")
	}
}

func TestRequestedReportsExportEachCurrentRunWithoutCombinedResponse(t *testing.T) {
	original := opts.reportMode
	opts.reportMode = false
	t.Cleanup(func() { opts.reportMode = original })
	contexts := make(map[string]*teamContext)
	for _, name := range []string{"auto", "manual"} {
		workspace := t.TempDir()
		session := &team.TeamSession{Dir: workspace, Workspace: workspace, Config: agent.TeamConfig{Name: name, AutoReport: name == "auto"}}
		coordinator, err := team.NewCoordinator(session, "", "", nil, nil, team.RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = coordinator.Close() })
		coordinator.SetSessionData(team.NewSession())
		coordinator.SetInvocationID("inv-export-" + name)
		// A real direct invocation that fails before provider execution still has
		// one terminal run. No model, server fixture, or credential is needed.
		if _, err := coordinator.RunDirectAgent(t.Context(), "missing-"+name, "request for "+name); err == nil {
			t.Fatal("missing agent unexpectedly ran")
		}
		if result := coordinator.LastRunResult(); result == nil || !coordinator.TerminalLifecycleConfirmed() {
			t.Fatal("failure did not finish canonically")
		}
		contexts[name] = &teamContext{teamName: name, session: session, coordinator: coordinator}
	}
	generateRequestedReports(contexts, "COMBINED_RESPONSE_MUST_NOT_APPEAR")
	if _, err := os.Stat(filepath.Join(contexts["manual"].session.Workspace, "report.md")); !os.IsNotExist(err) {
		t.Fatalf("auto-report exported manual team: %v", err)
	}
	for _, name := range []string{"auto", "manual"} {
		opts.reportMode = true
		generateRequestedReports(contexts, "COMBINED_RESPONSE_MUST_NOT_APPEAR")
		data, err := os.ReadFile(filepath.Join(contexts[name].session.Workspace, "report.md"))
		if err != nil {
			t.Fatal(err)
		}
		own := contexts[name].coordinator.LastRunResult()
		if !strings.Contains(string(data), own.RunID) || !strings.Contains(string(data), "# Execution Report — "+name) || strings.Contains(string(data), "COMBINED_RESPONSE_MUST_NOT_APPEAR") {
			t.Fatalf("wrong team export: %s", data)
		}
		other := "auto"
		if name == "auto" {
			other = "manual"
		}
		if strings.Contains(string(data), contexts[other].coordinator.LastRunResult().RunID) {
			t.Fatal("cross-team run leaked into export")
		}
	}
}

func TestResultsReportFollowsBranchLineage(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	parent, _, err := executeResultsTest(t, deps, "report", ids[0], "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(workspace, "run-child", "session-review")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	tree := team.NewSessionTree()
	child, err := tree.CreateBranch("report-child", parent.Report.SourceEventID, store)
	if err != nil {
		t.Fatal(err)
	}
	tree.ActiveBranch = "main"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	var finished team.RunEvent
	for _, kind := range []string{"run_started", "run_finished"} {
		payload := map[string]any{"team": "review"}
		if kind == "run_finished" {
			payload["outcome"], payload["response"] = "partial", "CHILD_REPORT_SENTINEL"
		}
		finished, err = store.AppendPersistedContext(t.Context(), team.RunEvent{Type: kind, RunID: "run-child", BranchID: child.ID, Actor: "coordinator", Timestamp: "2026-10-14T12:00:00Z", Payload: resultsTestJSON(t, payload)})
		if err != nil {
			t.Fatal(err)
		}
	}
	childID := team.HistoricalResultID(finished)
	if _, _, err := executeResultsTest(t, deps, "report", childID, "--workspace", workspace, "--team", "review", "--branch", "main"); err == nil {
		t.Fatal("sibling report escaped selected lineage")
	}
	for _, scope := range [][]string{{"--branch", child.ID}, {"--all-branches"}} {
		args := []string{"report", childID, "--workspace", workspace, "--team", "review", "--output", "json"}
		args = append(args, scope...)
		report, _, err := executeResultsTest(t, deps, args...)
		if err != nil || report.Report == nil || report.Report.BranchID != child.ID || !strings.Contains(report.ReportMarkdown, "CHILD_REPORT_SENTINEL") {
			t.Fatalf("child report: %+v, %v", report, err)
		}
	}
	inherited, _, err := executeResultsTest(t, deps, "report", ids[0], "--workspace", workspace, "--team", "review", "--branch", child.ID, "--output", "json")
	if err != nil || inherited.Report == nil || inherited.Report.SourceEventID != parent.Report.SourceEventID || inherited.ReportMarkdown != parent.ReportMarkdown {
		t.Fatalf("inherited report changed: %+v %v", inherited, err)
	}
}
