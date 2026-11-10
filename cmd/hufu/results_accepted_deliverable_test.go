package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func acceptedDeliverableFixture(t *testing.T) (string, resultsDeps, []inspectpkg.HistoricalResult) {
	t.Helper()
	workspace := t.TempDir()
	rows := []inspectpkg.HistoricalResult{
		{Kind: "run_response", ResultID: "res-run-current", Workspace: workspace, Team: "review", RunID: "current", SessionID: "session", Branches: []string{"main"}, Status: "partial", CompletedAt: time.Unix(20, 0)},
		{Kind: "task", ResultID: "res-report-current", Workspace: workspace, Team: "review", RunID: "current", SessionID: "session", Branches: []string{"main"}, Agent: "arbitrary-author", Status: "done", CurrentStatus: "done", Validity: "accepted", CompletedAt: time.Unix(19, 0), TypedResult: &team.TaskResult{Status: team.TaskResultStatusCompletedWithGaps, StructuredPayload: &team.ResultPayload{}}},
	}
	deps := resultsTestDeps(t)
	deps.load = func(_ context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		return inspectpkg.HistoricalSnapshot{Coverage: inspectpkg.HistoricalCoverage{Scope: scope}, Results: rows}, nil
	}
	deps.deliverable = func(_ context.Context, row inspectpkg.HistoricalResult) (string, bool, error) {
		if row.Agent != "arbitrary-author" {
			t.Fatalf("wrong report task: %+v", row)
		}
		return "# Qualified answer\n\nUncertainty and complete evidence.\n", true, nil
	}
	return workspace, deps, rows
}

func TestResultsDeliverableRendersAcceptedTaskAndPreservesRunOutcome(t *testing.T) {
	workspace, deps, rows := acceptedDeliverableFixture(t)
	before := resultsTestJSON(t, rows)
	path := filepath.Join(t.TempDir(), "deliverable.md")
	for _, locator := range []string{"", rows[0].ResultID, rows[1].ResultID} {
		args := []string{"deliverable"}
		if locator != "" {
			args = append(args, locator)
		}
		args = append(args, "--workspace", workspace, "--team", "review", "--run", "current", "--output", "json", "--export", path)
		response, _, err := executeResultsTest(t, deps, args...)
		if err != nil {
			t.Fatal(err)
		}
		if response.SourceRunStatus != "partial" || len(response.Results) != 1 || response.Results[0].ResultID != rows[1].ResultID || !strings.Contains(response.Deliverable, "complete evidence") {
			t.Fatalf("wrong deliverable: %+v", response)
		}
		data, err := os.ReadFile(path)
		if err != nil || string(data) != response.Deliverable {
			t.Fatalf("export differs: %v", err)
		}
	}
	_, plain, err := executeResultsTest(t, deps, "deliverable", "--workspace", workspace, "--team", "review", "--render", "markdown")
	if err != nil || plain != "# Qualified answer\n\nUncertainty and complete evidence.\n" {
		t.Fatalf("unexpected plain report %q: %v", plain, err)
	}
	if !bytes.Equal(before, resultsTestJSON(t, rows)) {
		t.Fatal("presentation mutated historical results")
	}
}

func TestResultsDeliverableRejectsMissingAmbiguousOrStaleReports(t *testing.T) {
	for _, scenario := range []string{"newer empty run", "other run", "other team", "other session", "other branch", "other workspace", "superseded", "branch dependent", "not done", "no template", "schema drift", "ambiguous", "empty output"} {
		t.Run(scenario, func(t *testing.T) {
			workspace, deps, rows := acceptedDeliverableFixture(t)
			switch scenario {
			case "newer empty run":
				rows = append(rows, inspectpkg.HistoricalResult{Kind: "run_response", ResultID: "res-newer-run", Workspace: workspace, Team: "review", SessionID: "session", RunID: "newer", Branches: []string{"main"}, Status: "error", CompletedAt: time.Unix(30, 0)})
			case "other run":
				rows[1].RunID = "elsewhere"
			case "other team":
				rows[1].Team = "elsewhere"
			case "other session":
				rows[1].SessionID = "elsewhere"
			case "other branch":
				rows[1].Branches = []string{"elsewhere"}
			case "other workspace":
				rows[1].Workspace = "elsewhere"
			case "superseded":
				rows[1].Validity = "superseded"
			case "branch dependent":
				rows[1].Validity = "branch_dependent"
			case "not done":
				rows[1].CurrentStatus = "blocked"
			case "no template":
				deps.deliverable = func(context.Context, inspectpkg.HistoricalResult) (string, bool, error) { return "", false, nil }
			case "schema drift":
				deps.deliverable = func(context.Context, inspectpkg.HistoricalResult) (string, bool, error) {
					return "", false, errors.New("schema hash changed")
				}
			case "ambiguous":
				other := rows[1]
				other.ResultID = "res-another-report"
				rows = append(rows, other)
			case "empty output":
				deps.deliverable = func(context.Context, inspectpkg.HistoricalResult) (string, bool, error) { return " ", true, nil }
			}
			deps.load = func(_ context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
				return inspectpkg.HistoricalSnapshot{Coverage: inspectpkg.HistoricalCoverage{Scope: scope}, Results: rows}, nil
			}
			path := filepath.Join(t.TempDir(), "should-not-exist.md")
			run := "current"
			if scenario == "newer empty run" {
				run = "newer"
			}
			_, output, err := executeResultsTest(t, deps, "deliverable", "--workspace", workspace, "--team", "review", "--run", run, "--export", path)
			if err == nil || output != "" {
				t.Fatalf("invalid report was printed: %q %v", output, err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid report created an export")
			}
		})
	}
}

func TestResultsDeliverableFindsLatestAcceptedReportAfterEmptyRun(t *testing.T) {
	workspace, deps, rows := acceptedDeliverableFixture(t)
	rows = append(rows, inspectpkg.HistoricalResult{Kind: "run_response", ResultID: "res-latest-empty", Workspace: workspace, Team: "review", RunID: "empty", SessionID: "session", Branches: []string{"main"}, Status: "failed", CompletedAt: time.Unix(30, 0)})
	deps.load = func(_ context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		return inspectpkg.HistoricalSnapshot{Coverage: inspectpkg.HistoricalCoverage{Scope: scope}, Results: rows}, nil
	}
	response, _, err := executeResultsTest(t, deps, "deliverable", "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil || response.SourceRunStatus != "partial" || len(response.Results) != 1 || response.Results[0].RunID != "current" {
		t.Fatalf("wrong report source: %+v %v", response, err)
	}
}

func TestResultsDeliverablePreservesTerminalAndExportSafety(t *testing.T) {
	workspace, deps, _ := acceptedDeliverableFixture(t)
	deps.deliverable = func(context.Context, inspectpkg.HistoricalResult) (string, bool, error) {
		return "\x1b]52;c;clipboard\a## Safe\n", true, nil
	}
	path := filepath.Join(t.TempDir(), "safe.md")
	_, text, err := executeResultsTest(t, deps, "deliverable", "--workspace", workspace, "--team", "review", "--export", path)
	data, readErr := os.ReadFile(path)
	if err != nil || readErr != nil || text != "## Safe\n" || string(data) != text {
		t.Fatalf("unsafe presentation: %q %v %v", text, err, readErr)
	}
	for _, flag := range [][]string{{"--agent", "arbitrary-author"}, {"--status", "done"}, {"--cursor", "x"}, {"--export", filepath.Join(workspace, "runtime.sqlite")}} {
		args := append([]string{"deliverable", "--workspace", workspace, "--team", "review"}, flag...)
		if _, _, err := executeResultsTest(t, deps, args...); err == nil {
			t.Fatalf("invalid flag accepted: %v", flag)
		}
	}
}
