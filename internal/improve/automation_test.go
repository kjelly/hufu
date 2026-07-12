package improve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPullRequestPlanAndRunnerNeverMerge(t *testing.T) {
	workspace, report, candidate := createAutomationExperiment(t)
	repository := t.TempDir()
	plan, err := PreparePullRequest(workspace, report.ID, repository, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Branch != "hufu/improve/"+report.ID || plan.CandidateSnapshotID != candidate.ID {
		t.Fatalf("plan = %+v", plan)
	}
	runner := &fakeCommandRunner{outputs: map[string]string{"gh pr create": "https://github.com/acme/repo/pull/42\n"}}
	record, err := CreatePullRequestWithRunner(plan, runner)
	if err != nil {
		t.Fatal(err)
	}
	if record.PullRequestURL != "https://github.com/acme/repo/pull/42" {
		t.Fatalf("record = %+v", record)
	}
	joined := strings.Join(runner.calls, "\n")
	if !strings.Contains(joined, "git switch -c") || !strings.Contains(joined, "git push --set-upstream") || !strings.Contains(joined, "gh pr create") {
		t.Fatalf("expected branch and PR commands, got:\n%s", joined)
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "git merge ") || strings.HasPrefix(call, "git push --force") {
			t.Fatalf("PR automation must not merge or force push:\n%s", joined)
		}
	}
	if _, err := WritePromotionRecord(workspace, record); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePromotionRecord(workspace, record); err == nil {
		t.Fatal("expected promotion record overwrite to be rejected")
	}
}

func TestAdoptionStoresKnowledgeAndMonitoringSuggestsRollback(t *testing.T) {
	workspace, experiment, candidate := createAutomationExperiment(t)
	adoption, _, err := CreateAdoption(workspace, "adopt-1", experiment.ID, "https://github.com/acme/repo/pull/42", "Require focused verification before reporting completion.", []string{"task-type:refactor"})
	if err != nil {
		t.Fatal(err)
	}
	if adoption.RollbackRevision != experiment.Baseline.DefinitionRevision || adoption.CandidateRevision != candidate.DefinitionRevision {
		t.Fatalf("adoption = %+v", adoption)
	}
	knowledge, err := ListKnowledge(workspace, "refactor")
	if err != nil {
		t.Fatal(err)
	}
	if len(knowledge) != 1 || knowledge[0].EffectiveChange != adoption.ChangeSummary || knowledge[0].Outcome != "adopted" {
		t.Fatalf("knowledge = %+v", knowledge)
	}
	if !strings.Contains(KnowledgeMarkdown(knowledge), "focused verification") {
		t.Fatal("knowledge markdown should include the effective change")
	}

	production := &Report{
		Team: "dev", RunIDs: []string{"production-1"}, TeamRevisions: []string{candidate.DefinitionRevision},
		Metrics: Metrics{TotalTasks: 2, Done: 1, Error: 1, TotalTokens: 120},
	}
	monitoring, err := EvaluateMonitoring(adoption, production, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if monitoring.Status != "degraded" || monitoring.RollbackSuggestion == nil {
		t.Fatalf("monitoring = %+v", monitoring)
	}
	if monitoring.RollbackSuggestion.RollbackRevision != adoption.RollbackRevision {
		t.Fatalf("rollback suggestion = %+v", monitoring.RollbackSuggestion)
	}
	path, err := WriteMonitoringReport(workspace, monitoring)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(MonitoringMarkdown(monitoring), "Implement the feature") {
		t.Fatal("monitoring report must not include benchmark prompts")
	}
}

func createAutomationExperiment(t *testing.T) (string, ExperimentReport, TeamSnapshot) {
	t.Helper()
	workspace := t.TempDir()
	baselineTeam := createExperimentTeam(t, "dev", "Follow the baseline workflow.")
	candidateTeam := createExperimentTeam(t, "dev", "Follow the candidate workflow with verification.")
	patchPath := filepath.Join(t.TempDir(), "candidate.patch")
	if err := os.WriteFile(patchPath, []byte("diff --git a/developer.md b/developer.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := CreateBaselineSnapshot(workspace, "base-1", baselineTeam)
	if err != nil {
		t.Fatal(err)
	}
	candidate, _, err := CreateCandidateSnapshot(workspace, "candidate-1", "base-1", candidateTeam, patchPath)
	if err != nil {
		t.Fatal(err)
	}
	experiment := ExperimentReport{
		Version: experimentVersion, ID: "exp-1", Benchmark: BenchmarkRef{Name: "refactor-smoke", Revision: "fixture-revision", Category: "refactor", Cases: 2},
		Baseline:  ExperimentArm{SnapshotID: baseline.ID, DefinitionRevision: baseline.DefinitionRevision, Metrics: Metrics{TotalTasks: 2, Done: 2, TotalTokens: 100}},
		Candidate: ExperimentArm{SnapshotID: candidate.ID, DefinitionRevision: candidate.DefinitionRevision, Metrics: Metrics{TotalTasks: 2, Done: 2, TotalTokens: 90}},
		Status:    "passed", Decision: "eligible_for_review",
	}
	if _, err := WriteExperimentReport(workspace, experiment); err != nil {
		t.Fatal(err)
	}
	return workspace, experiment, candidate
}

type fakeCommandRunner struct {
	calls   []string
	outputs map[string]string
}

func (r *fakeCommandRunner) Run(_ string, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	for prefix, output := range r.outputs {
		if strings.HasPrefix(call, prefix) {
			return output, nil
		}
	}
	return "", nil
}
