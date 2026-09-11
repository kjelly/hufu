package team

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestTeamLintUnsupportedSchemaIsFinding(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "apiVersion: hufu.io/v9\nkind: AgentTeam\nmetadata:\n  name: future\nspec: {}\n")
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Findings, func(f TeamLintFinding) bool {
		return f.Code == FindingUnsupportedSchemaVersion && f.File == "team.yaml"
	}) {
		t.Fatalf("findings = %#v", result.Findings)
	}
}

func TestTeamLintMalformedYAMLIsExit2(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: [unterminated\n")
	if _, err := LintTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
		t.Fatal("malformed YAML did not return a load error")
	}
}

func TestTeamLintDuplicateAgentIsStructuredFinding(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "coordinator.md"), lintAgent("coordinator", "coordinator"))
	writeLintTestFile(t, filepath.Join(dir, "one.md"), lintAgent("duplicate", "worker"))
	writeLintTestFile(t, filepath.Join(dir, "two.md"), lintAgent("duplicate", "worker"))
	assertLintCode(t, dir, FindingDuplicateAgent)
}

func TestTeamLintMissingCoordinator(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "worker.md"), lintAgent("worker", "worker"))
	assertLintCode(t, dir, FindingMissingCoordinator)
}

func TestTeamLintMultipleCoordinators(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "one.md"), lintAgent("one", "coordinator"))
	writeLintTestFile(t, filepath.Join(dir, "two.md"), lintAgent("two", "coordinator"))
	assertLintCode(t, dir, FindingMultipleCoordinators)
}

func TestTeamLintUnknownTaskAgent(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - id: missing\n    agent: ghost\n    goal: test\n")
	assertLintCode(t, dir, FindingUnknownTaskAgent)
}

func TestTeamLintDependencyCycle(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - id: first\n    agent: worker\n    depends-on: [1]\n  - id: second\n    agent: worker\n    depends-on: [0]\n")
	assertLintCode(t, dir, FindingDependencyCycle)
}

func TestTeamLintFindingsSorted(t *testing.T) {
	findings := []TeamLintFinding{
		{File: "b.md", Line: 1, Code: "b"},
		{File: "a.md", Line: 2, Code: "a"},
		{File: "a.md", Line: 1, Code: "z"},
	}
	SortTeamLintFindings(findings)
	if got := []string{findings[0].Code, findings[1].Code, findings[2].Code}; !slices.Equal(got, []string{"z", "a", "b"}) {
		t.Fatalf("sorted codes = %v", got)
	}
}

func TestTeamLintFailThreshold(t *testing.T) {
	findings := []TeamLintFinding{{Severity: FindingSeverityWarning}}
	if TeamLintReachesThreshold(findings, FindingSeverityError) {
		t.Fatal("warning reached error threshold")
	}
	if !TeamLintReachesThreshold(findings, FindingSeverityWarning) {
		t.Fatal("warning did not reach warning threshold")
	}
	if TeamLintReachesThreshold(findings, "none") {
		t.Fatal("finding reached none threshold")
	}
}

func lintTeamWithCoordinator(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\n"+manifest)
	writeLintTestFile(t, filepath.Join(dir, "coordinator.md"), lintAgent("coordinator", "coordinator"))
	writeLintTestFile(t, filepath.Join(dir, "worker.md"), lintAgent("worker", "worker"))
	return dir
}

func lintAgent(name, role string) string {
	return "---\nname: " + name + "\nrole: " + role + "\ntools: view\n---\nWork.\n"
}

func assertLintCode(t *testing.T, dir, code string) {
	t.Helper()
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Findings, func(f TeamLintFinding) bool { return f.Code == code }) {
		t.Fatalf("findings = %#v, want code %q", result.Findings, code)
	}
}
