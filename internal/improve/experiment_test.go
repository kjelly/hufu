package improve

import (
	"encoding/json"
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
	if err := os.WriteFile(filepath.Join(candidateDir, "team", "developer.md"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadCandidateSnapshot(workspace, candidate.ID); err == nil {
		t.Fatal("candidate snapshot with edited team content was accepted")
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
	baselineReport := &Report{Team: "dev", RunIDs: []string{"base-run"}, TeamRevisions: []string{"base-rev"}, MemoryPolicyVersions: []string{"base-policy"}, Metrics: Metrics{TotalTasks: 2, Done: 2, TotalTokens: 100}}
	candidateReport := &Report{Team: "dev", RunIDs: []string{"candidate-run"}, TeamRevisions: []string{"candidate-rev"}, MemoryPolicyVersions: []string{"candidate-policy"}, Metrics: Metrics{TotalTasks: 2, Done: 2, TotalTokens: 90}}

	report, err := EvaluateExperiment("exp-1", fixture,
		ExperimentInput{Snapshot: baseline, Report: baselineReport, MemoryPolicy: &ArtifactRef{Kind: "memory_policy_snapshot", ID: "base-policy", Revision: "base-policy-rev"}, AcceptancePassed: true},
		ExperimentInput{Snapshot: candidate, Report: candidateReport, MemoryPolicy: &ArtifactRef{Kind: "memory_policy_snapshot", ID: "candidate-policy", Revision: "candidate-policy-rev"}, AcceptancePassed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "passed" || report.Decision != "eligible_for_review" {
		t.Fatalf("report = %+v", report)
	}
	if err := ValidateMemoryPolicyExperimentReport(report, *report.Baseline.MemoryPolicy, *report.Candidate.MemoryPolicy); err != nil {
		t.Fatal(err)
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
	tampered := report
	tampered.Status = "failed"
	data, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadExperimentReport(workspace, report.ID); err == nil {
		t.Fatal("tampered experiment outcome was accepted")
	}
}

func TestEvaluateExperimentBindsAppliedContextCandidate(t *testing.T) {
	fixture := BenchmarkFixture{Name: "context-smoke", Team: "dev", Category: "context", Cases: []BenchmarkCase{{ID: "case", Type: "happy", Prompt: "Use confirmed context."}}}
	baseline := TeamSnapshot{Version: snapshotVersion, ID: "base", Kind: baselineSnapshotKind, Team: "dev", DefinitionRevision: "base-rev", ContentRevision: "base-content"}
	candidate := TeamSnapshot{Version: snapshotVersion, ID: "candidate", Kind: candidateSnapshotKind, Team: "dev", DefinitionRevision: "candidate-rev", ContentRevision: "candidate-content", BaselineID: baseline.ID}
	contextRef := ArtifactRef{Kind: "context_item", ID: "context-candidate", Revision: "context-revision"}
	baselineReport := &Report{Team: "dev", RunIDs: []string{"base-run"}, TeamRevisions: []string{baseline.DefinitionRevision}, Metrics: Metrics{TotalTasks: 1, Done: 1}}
	candidateReport := &Report{Team: "dev", RunIDs: []string{"candidate-run"}, TeamRevisions: []string{candidate.DefinitionRevision}, AppliedContextItemIDs: []string{contextRef.ID}, Metrics: Metrics{TotalTasks: 1, Done: 1}}
	report, err := EvaluateExperiment("context-experiment", fixture,
		ExperimentInput{Snapshot: baseline, Report: baselineReport, AcceptancePassed: true},
		ExperimentInput{Snapshot: candidate, Report: candidateReport, ContextCandidate: &contextRef, AcceptancePassed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateContextCandidateExperimentReport(report, contextRef); err != nil {
		t.Fatal(err)
	}
	candidateReport.AppliedContextItemIDs = nil
	if _, err := EvaluateExperiment("missing-context-evidence", fixture,
		ExperimentInput{Snapshot: baseline, Report: baselineReport, AcceptancePassed: true},
		ExperimentInput{Snapshot: candidate, Report: candidateReport, ContextCandidate: &contextRef, AcceptancePassed: true},
	); err == nil {
		t.Fatal("context candidate without runtime usage evidence was accepted")
	}
}

func TestCreateSkillCandidateSnapshotIsolatedAndImmutable(t *testing.T) {
	workspace := t.TempDir()
	baselineTeam := createExperimentTeam(t, "dev", "Follow the baseline workflow.")
	baseline, baselineDir, err := CreateBaselineSnapshot(workspace, "base-skill", baselineTeam)
	if err != nil {
		t.Fatal(err)
	}
	draft := "---\nname: verification\ndescription: Verify changes.\n---\n# Verification\n\n## Steps\n- Run the check.\n- Record the result.\n"
	candidate, candidateDir, err := CreateSkillCandidateSnapshot(workspace, "skill-candidate", "base-skill", "verification", draft)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Kind != candidateSnapshotKind || candidate.BaselineID != baseline.ID || candidate.PatchRevision == "" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.ContentRevision == baseline.ContentRevision {
		t.Fatal("skill candidate must differ from baseline")
	}
	if _, err := os.Stat(filepath.Join(candidateDir, "team", "skills", "verification", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(baselineDir, "team", "skills", "verification", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("baseline skill target was modified: %v", err)
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
