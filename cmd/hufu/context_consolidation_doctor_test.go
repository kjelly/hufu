package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContextDoctorRequiresExactlyOneCheck(t *testing.T) {
	workspace := t.TempDir()
	for _, args := range [][]string{
		{"context", "doctor", "--workspace", workspace},
		{"context", "doctor", "--workspace", workspace, "--learning", "--consolidation", "--project", "proj1"},
	} {
		contextLearningCheck, contextConsolidationCheck = false, false
		if _, err := helperRunConsolidateCLI(args...); err == nil || !strings.Contains(err.Error(), "one of --learning or --consolidation is required") {
			t.Fatalf("%v err = %v", args, err)
		}
	}
	contextLearningCheck, contextConsolidationCheck = false, false
}

func TestSupersedingAConsolidationSourceRemovesItFromProjectionAndDoctorReportsIt(t *testing.T) {
	workspace, _ := consolidationDraftFixture(t)
	t.Cleanup(func() { contextLearningCheck, contextConsolidationCheck = false, false })
	if _, err := helperRunConsolidateCLI(manualConsolidationArgs(workspace)...); err != nil {
		t.Fatal(err)
	}
	proposal, candidate := pendingProposal(t, workspace)
	if _, err := helperRunConsolidateCLI("context", "consolidation", "approve", proposal.ID, "--workspace", workspace, "--project", "proj1"); err != nil {
		t.Fatal(err)
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildProjection(context.Background(), contextstore.Scope{ProjectID: "proj1", TeamID: "demo"}); err != nil {
		t.Fatal(err)
	}
	_ = repo.Close()
	projection := filepath.Join(workspace, "context-ltm.md")
	if data, err := os.ReadFile(projection); err != nil || !strings.Contains(string(data), candidate.Content) {
		t.Fatalf("approved consolidation missing from projection: %q err=%v", data, err)
	}

	out, err := helperRunConsolidateCLI("context", "doctor", "--consolidation", "--workspace", workspace, "--project", "proj1", "--team", "demo")
	if err != nil || !strings.Contains(out, "ok fresh=1") {
		t.Fatalf("doctor before supersede = %q err=%v", out, err)
	}
	if _, err := helperRunConsolidateCLI("context", "supersede", "src-b", "--with", "src-a", "--workspace", workspace, "--project", "proj1", "--team", "demo"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(projection); err != nil || strings.Contains(string(data), candidate.Content) {
		t.Fatalf("demoted consolidation still projected: %q err=%v", data, err)
	}
	if got := candidateLifecycle(t, workspace, candidate.ID); got != contextstore.LifecycleCandidate {
		t.Fatalf("candidate lifecycle = %s, want candidate", got)
	}

	contextLearningCheck, contextConsolidationCheck = false, false
	out, err = helperRunConsolidateCLI("context", "doctor", "--consolidation", "--workspace", workspace, "--project", "proj1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Status    string                                `json:"status"`
		Counts    map[string]int                        `json:"counts"`
		Proposals []contextstore.ConsolidationFreshness `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("doctor json %q: %v", out, err)
	}
	if report.Status != "attention" || report.Counts["stale"] != 1 || len(report.Proposals) != 1 || report.Proposals[0].Status != contextstore.ConsolidationStatusStale {
		t.Fatalf("doctor report = %+v", report)
	}
	if strings.Contains(out, candidate.Content) {
		t.Fatalf("doctor leaked memory content: %s", out)
	}

	out, err = helperRunConsolidateCLI("context", "consolidation", "show", proposal.ID, "--workspace", workspace, "--project", "proj1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		ID        string                              `json:"id"`
		Status    string                              `json:"status"`
		SourceIDs []string                            `json:"source_ids"`
		Freshness contextstore.ConsolidationFreshness `json:"freshness"`
	}
	if err := json.Unmarshal([]byte(out), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.ID != proposal.ID || len(shown.SourceIDs) != 2 || shown.Freshness.State != contextstore.ConsolidationStale {
		t.Fatalf("show = %+v", shown)
	}
}
