package main

import (
	"strings"
	"testing"
	"time"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/resultquery"
)

func TestResultsFinalFollowsExactRunAndBranchWithoutGuessing(t *testing.T) {
	id := "res-" + strings.Repeat("a", 64)
	anchor := inspectpkg.HistoricalResult{ResultID: id, Kind: "task", Workspace: "/selected", Team: "review", SessionID: "session", RunID: "run", Branches: []string{"main"}}
	response := anchor
	response.ResultID, response.Kind, response.Output = "res-"+strings.Repeat("b", 64), "run_response", "saved report"
	otherRun, otherTeam, otherWorkspace, otherSession, otherBranch := response, response, response, response, response
	otherRun.RunID = "newer-run"
	otherRun.CompletedAt = time.Now()
	otherTeam.Team = "other"
	otherWorkspace.Workspace = "/other"
	otherSession.SessionID = "other"
	otherBranch.Branches = []string{"sibling"}
	unrelated := []inspectpkg.HistoricalResult{anchor, otherRun, otherTeam, otherWorkspace, otherSession, otherBranch}
	if _, err := selectFinalResults(unrelated, resultquery.Request{}, id); err == nil {
		t.Fatal("unfinished run used an unrelated final response")
	}
	rows, err := selectFinalResults(append(unrelated, response), resultquery.Request{}, id)
	if err != nil || len(rows) != 1 || rows[0].Output != "saved report" || rows[0].RunID != "run" {
		t.Fatalf("wrong final response: %#v, %v", rows, err)
	}
	rows, err = selectFinalResults([]inspectpkg.HistoricalResult{response, otherRun}, resultquery.Request{}, "")
	if err != nil || len(rows) != 2 || rows[0].RunID != "newer-run" {
		t.Fatalf("latest final response ordering: %#v, %v", rows, err)
	}
	if _, err := selectFinalResults([]inspectpkg.HistoricalResult{anchor, response}, resultquery.Request{RunID: "different-run"}, id); err == nil {
		t.Fatal("anchor overrode explicit run filter")
	}
	rows, err = selectFinalResults([]inspectpkg.HistoricalResult{response}, resultquery.Request{}, response.ResultID)
	if err != nil || len(rows) != 1 || rows[0].ResultID != response.ResultID {
		t.Fatalf("final response locator did not retain exact identity: %#v, %v", rows, err)
	}
	anchor.Branches = []string{"main", "sibling"}
	otherBranch.ResultID = "res-" + strings.Repeat("c", 64)
	if _, err := selectFinalResults([]inspectpkg.HistoricalResult{anchor, response, otherBranch}, resultquery.Request{}, id); err == nil {
		t.Fatal("ambiguous final responses across branches were silently chosen")
	}
}
