package improve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBenchmarkAndSnapshotsCreateImmutableArtifacts(t *testing.T) {
	workspace := t.TempDir()
	baselineTeam := createExperimentTeam(t, "dev", "Follow the baseline workflow.")
	candidateTeam := createExperimentTeam(t, "dev", "Follow the candidate workflow with verification.")
	patchPath := filepath.Join(t.TempDir(), "candidate.patch")
	if err := os.WriteFile(patchPath, []byte("diff --git a/developer.md b/developer.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := BenchmarkFixture{
		Name: "smoke", Team: "dev", Category: "development",
		Cases: []BenchmarkCase{
			{ID: "happy", Type: "happy", Prompt: "Implement the feature."},
			{ID: "edge", Type: "edge", Prompt: "Handle invalid input."},
		},
	}
	benchmarkPath, revision, err := CreateBenchmark(workspace, fixture)
	if err != nil {
		t.Fatal(err)
	}
	loadedFixture, err := LoadBenchmark(benchmarkPath)
	if err != nil {
		t.Fatal(err)
	}
	if loadedFixture.Version != benchmarkVersion || BenchmarkRevision(loadedFixture) != revision {
		t.Fatalf("benchmark = %+v, revision = %s", loadedFixture, revision)
	}
	if _, _, err := CreateBenchmark(workspace, fixture); err == nil {
		t.Fatal("expected existing benchmark to be rejected")
	}

	baseline, baselineDir, err := CreateBaselineSnapshot(workspace, "base-1", baselineTeam)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Kind != baselineSnapshotKind || baseline.DefinitionRevision == "" || baseline.ContentRevision == "" {
		t.Fatalf("baseline = %+v", baseline)
	}
	if _, err := os.Stat(filepath.Join(baselineDir, "team", "developer.md")); err != nil {
		t.Fatal(err)
	}
	candidate, candidateDir, err := CreateCandidateSnapshot(workspace, "candidate-1", "base-1", candidateTeam, patchPath)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Kind != candidateSnapshotKind || candidate.BaselineID != baseline.ID || candidate.PatchRevision == "" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.ContentRevision == baseline.ContentRevision {
		t.Fatal("candidate must have different content revision")
	}
	if _, err := os.Stat(filepath.Join(candidateDir, "candidate.patch")); err != nil {
		t.Fatal(err)
	}
	loadedBaseline, _, err := LoadBaselineSnapshot(workspace, "base-1")
	if err != nil || loadedBaseline.ContentRevision != baseline.ContentRevision {
		t.Fatalf("loaded baseline = %+v, err = %v", loadedBaseline, err)
	}
}

func TestEvaluateExperimentUsesHardGatesAndWritesReviewOnlyReport(t *testing.T) {
	fixture := BenchmarkFixture{
		Name: "smoke", Team: "dev", Category: "development",
		Cases: []BenchmarkCase{
			{ID: "happy", Type: "happy", Prompt: "Implement the feature."},
			{ID: "edge", Type: "edge", Prompt: "Handle invalid input."},
		},
	}
	baseline := TeamSnapshot{Version: snapshotVersion, ID: "base-1", Kind: baselineSnapshotKind, Team: "dev", DefinitionRevision: "base-rev", ContentRevision: "base-content"}
	candidate := TeamSnapshot{Version: snapshotVersion, ID: "candidate-1", Kind: candidateSnapshotKind, Team: "dev", DefinitionRevision: "candidate-rev", ContentRevision: "candidate-content", BaselineID: "base-1"}
	baselineReport := &Report{Team: "dev", RunIDs: []string{"base-run"}, TeamRevisions: []string{"base-rev"}, Metrics: Metrics{TotalTasks: 2, Done: 2, TotalTokens: 100}}
	candidateReport := &Report{Team: "dev", RunIDs: []string{"candidate-run"}, TeamRevisions: []string{"candidate-rev"}, Metrics: Metrics{TotalTasks: 2, Done: 2, TotalTokens: 90}}

	report, err := EvaluateExperiment("exp-1", fixture,
		ExperimentInput{Snapshot: baseline, Report: baselineReport, AcceptancePassed: true},
		ExperimentInput{Snapshot: candidate, Report: candidateReport, AcceptancePassed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "passed" || report.Decision != "eligible_for_review" {
		t.Fatalf("report = %+v", report)
	}
	markdown := ExperimentMarkdown(report)
	if !strings.Contains(markdown, "does not apply a candidate") {
		t.Fatal("markdown must state that the experiment cannot apply a candidate")
	}
	if strings.Contains(markdown, "Implement the feature") {
		t.Fatal("experiment report must not include benchmark prompts")
	}

	failed, err := EvaluateExperiment("exp-2", fixture,
		ExperimentInput{Snapshot: baseline, Report: baselineReport, AcceptancePassed: true},
		ExperimentInput{Snapshot: candidate, Report: candidateReport, AcceptancePassed: true, SafetyViolations: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != "failed" || failed.Decision != "reject" {
		t.Fatalf("safety failure = %+v", failed)
	}

	workspace := t.TempDir()
	path, err := WriteExperimentReport(workspace, report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteExperimentReport(workspace, report); err == nil {
		t.Fatal("expected immutable experiment report to reject overwrite")
	}
}

func createExperimentTeam(t *testing.T, name, prompt string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "---\nname: developer\ntools: view,edit\n---\n" + prompt + "\n"
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
