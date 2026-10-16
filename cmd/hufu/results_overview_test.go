package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestResultsRunsShowsFinalResponsesRatherThanInternalAttempts(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	value, _, err := executeResultsTest(t, deps, "runs", "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	if value.Matched != 1 || len(value.Results) != 1 || value.Results[0].Kind != "run_response" || value.Results[0].Output != "coordinator final response" {
		t.Fatalf("default view still lists workers: %#v", value)
	}
	_, output, err := executeResultsTest(t, deps, "runs", "--workspace", workspace, "--team", "review")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"Saved final responses: 1", "showing 1", "Original request: 請調查原始連線逾時問題", "Branches (visible in):", "Preview (excerpt)", "coordinator final response", "hufu results list", "Read full response:", "--all-branches"} {
		if !strings.Contains(output, required) {
			t.Fatalf("overview omitted %q: %s", required, output)
		}
	}
	for _, internal := range []string{"調整 deadline", "Task:", "index=", "branch=", "OTHER_TEAM_SENTINEL"} {
		if strings.Contains(output, internal) {
			t.Fatalf("overview leaked internal details %q: %s", internal, output)
		}
	}
	workers, _, err := executeResultsTest(t, deps, "list", "--workspace", workspace, "--team", "review", "--output", "json")
	if err != nil || len(workers.Results) != 3 {
		t.Fatalf("explicit worker list changed: %#v, %v", workers, err)
	}
}

func TestResultsOverviewEmptyBoundedAndScopePreserving(t *testing.T) {
	var output bytes.Buffer
	options := &resultsCLIOptions{team: "review", allBranches: true}
	response := resultsEnvelope{Team: "review"}
	if err := writeResultsOverview(&output, response, options); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No saved final responses") {
		t.Fatal("empty overview gave no explanation")
	}
	output.Reset()
	response.Matched = 1
	response.Results = []inspectpkg.HistoricalResult{{ResultID: "res-" + strings.Repeat("a", 64), Kind: "run_response", Team: "review", Output: strings.Repeat("範", 10000) + "\nsecond\nthird\nSHOULD_NOT_DISPLAY"}}
	if err := writeResultsOverview(&output, response, options); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "SHOULD_NOT_DISPLAY") || len([]rune(output.String())) > 1500 {
		t.Fatal("overview preview is not bounded")
	}
	if !strings.Contains(output.String(), "Read full response: hufu results show "+response.Results[0].ResultID+" --team 'review' --all-branches") {
		t.Fatal("overview reading command lost branch scope")
	}
	if strings.Contains(output.String(), "--workspace") {
		t.Fatal("registry overview exposed an unnecessary workspace argument")
	}
	output.Reset()
	options.workspace, options.branch, options.allBranches = "/custom workspace", "specific branch", false
	if err := writeResultsOverview(&output, response, options); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "--workspace '/custom workspace' --branch 'specific branch'") {
		t.Fatal("explicit scope was lost from the reading command")
	}
}

func TestResultsOverviewCursorContinuesFinalResponsesOnly(t *testing.T) {
	workspace, _ := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	canonical := deps.load
	deps.load = func(ctx context.Context, scope inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error) {
		snapshot, err := canonical(ctx, scope)
		if err != nil {
			return snapshot, err
		}
		for _, row := range snapshot.Results {
			if row.Kind == "run_response" {
				row.ResultID = "res-" + strings.Repeat("b", 64)
				row.CompletedAt = row.CompletedAt.Add(time.Hour)
				snapshot.Results = append(snapshot.Results, row)
				break
			}
		}
		return snapshot, nil
	}
	args := []string{"runs", "--workspace", workspace, "--team", "review", "--limit", "1", "--output", "json"}
	first, _, err := executeResultsTest(t, deps, args...)
	if err != nil || first.Matched != 2 || first.NextCursor == "" || len(first.Results) != 1 {
		t.Fatalf("first overview page: %#v, %v", first, err)
	}
	next, _, err := executeResultsTest(t, deps, append(args, "--cursor", first.NextCursor)...)
	if err != nil || len(next.Results) != 1 || next.Results[0].ResultID == first.Results[0].ResultID || next.Results[0].Kind != "run_response" {
		t.Fatalf("next overview page: %#v, %v", next, err)
	}
}

func TestResultsRequestPreviewAndFullText(t *testing.T) {
	row := inspectpkg.HistoricalResult{OriginalPrompt: strings.Repeat("原", 350) + "END_OF_REQUEST", Branches: []string{"main", "child"}}
	var output bytes.Buffer
	if err := writeResultsRequest(&output, row, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "END_OF_REQUEST") || !strings.Contains(output.String(), "Branches (visible in): main, child") {
		t.Fatalf("preview not bounded or branches missing: %s", output.String())
	}
	output.Reset()
	if err := writeResultsRequest(&output, row, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), row.OriginalPrompt) {
		t.Fatal("full view truncated original request")
	}
	output.Reset()
	if err := writeResultsRequest(&output, inspectpkg.HistoricalResult{}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Not saved") {
		t.Fatal("missing request not explained")
	}
}
