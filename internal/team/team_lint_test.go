package team

import (
	"os"
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

func TestTeamLintUnknownTool(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Use `definitely_missing` tool.")
	assertLintCode(t, dir, FindingPromptUnknownTool)
}

func TestTeamLintDeniedTool(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: [view, bash]", "Use `bash` tool.")
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\ntools:\n  denied: [bash]\n")
	assertLintCode(t, dir, FindingPromptDeniedTool)
}

func TestTeamLintToolNotGranted(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Call `bash` tool.")
	assertLintCode(t, dir, FindingPromptToolNotGranted)
}

func TestTeamLintKnownToolNoFinding(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Use the `view` tool.")
	assertLintNoCode(t, dir, FindingPromptUnknownTool, FindingPromptToolNotGranted, FindingPromptDeniedTool)
}

func TestTeamLintDeclaredToolMissing(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: imaginary_tool", "Work.")
	assertLintCode(t, dir, FindingDeclaredToolMissing)
}

func TestTeamLintToolAliasUsesRuntimeNormalization(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: read", "Use `read` tool.")
	assertLintNoCode(t, dir, FindingPromptUnknownTool, FindingPromptToolNotGranted, FindingDeclaredToolMissing)
}

func TestTeamLintMCPMissingServer(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Invoke `ghost__search` tool.")
	assertLintCode(t, dir, FindingMCPToolMissing)
}

func TestTeamLintMCPMetadataUnknown(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Invoke `catalog__search` tool.")
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\nmcp-servers:\n  catalog:\n    type: remote\n    url: https://invalid.example.test/mcp\n")
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Findings, func(f TeamLintFinding) bool {
		return f.Code == FindingMCPToolUnknown && f.Resolution == "unknown"
	}) {
		t.Fatalf("findings = %#v", result.Findings)
	}
}

func TestTeamLintDoesNotStartMCPOrNetwork(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Invoke `probe__ping` tool.")
	sentinel := filepath.Join(dir, "mcp-started")
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\nmcp-servers:\n  probe:\n    type: local\n    command: [sh, -c, 'touch "+sentinel+"']\n")
	if _, err := LintTeam(dir, nil, nil, DefaultProviderRegistry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("MCP command started during lint: %v", err)
	}
}

func TestTeamLintUnknownSkill(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "Load `missing-skill-xyz` skill.")
	assertLintCode(t, dir, FindingPromptUnknownSkill)
}

func TestTeamLintDraftSkillIsNotProductionRequirement(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view\nskills: draft-skill", "Work.")
	writeSkillFixture(t, filepath.Join(dir, "skills", "drafts", "draft-skill"), "draft-skill", "Draft.")
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(result.Findings, func(f TeamLintFinding) bool {
		return f.Code == FindingRequiredSkillMissing && f.Resolution == "draft_only"
	}) {
		t.Fatalf("findings = %#v", result.Findings)
	}
}

func TestTeamLintSkillOutOfScope(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view\nskills: hidden-skill", "Work.")
	writeSkillFixture(t, filepath.Join(dir, "skills", "hidden-skill"), "hidden-skill", "Hidden.")
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\nskills: another-skill\n")
	assertLintCode(t, dir, FindingSkillNotAvailable)
}

func TestTeamLintSkillDependencyUsesRuntimeExpansion(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view\nskills: root-skill", "Use `dep-skill` skill.")
	writeSkillFixture(t, filepath.Join(dir, "skills", "dep-skill"), "dep-skill", "Dependency.")
	writeSkillFixture(t, filepath.Join(dir, "skills", "root-skill"), "root-skill", "Read skills/dep-skill/SKILL.md.")
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\nskills: root-skill\n")
	assertLintNoCode(t, dir, FindingPromptUnknownSkill, FindingSkillNotAvailable, FindingRequiredSkillMissing)
}

func TestPromptExampleDoesNotTriggerFalseUnknownTool(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "`kubectl` is an example command.")
	assertLintNoCode(t, dir, FindingPromptUnknownTool)
}

func TestPromptFencedCodeDoesNotTrigger(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "```text\nuse `ghost-tool` tool\n```")
	assertLintNoCode(t, dir, FindingPromptUnknownTool)
}

func TestPromptDirectiveSourceLine(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: view", "First line.\nUse `ghost-tool` tool.")
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findLintCode(result.Findings, FindingPromptUnknownTool)
	if !ok || finding.File != "worker.md" || finding.Line != 7 || finding.Column != 6 || finding.LocationStatus != LocationExact {
		t.Fatalf("finding = %#v", finding)
	}
}

func TestTemplatedFieldLocationFallback(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - id: templated\n    agent: worker\n    goal: '{@ .goal @}'\n")
	result, err := LintTeam(dir, map[string]string{"goal": "Use `ghost-template-tool` tool."}, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findLintCode(result.Findings, FindingPromptUnknownTool)
	if !ok || finding.LocationStatus != LocationRendered || finding.Line == 0 {
		t.Fatalf("finding = %#v", finding)
	}
}

func TestTeamLintProfileMatchesRuntimePolicyProjection(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: fetch", "Use `fetch` tool.")
	noNet := true
	result, err := LintTeamWithOptions(dir, TeamLintOptions{Registry: DefaultProviderRegistry, NoNet: &noNet})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findLintCode(result.Findings, FindingPromptDeniedTool); !ok {
		t.Fatalf("findings = %#v", result.Findings)
	}
}

func TestTeamLintSideEffectRetryNeedsRecovery(t *testing.T) {
	dir := lintTeamWithAgentBody(t, "tools: ssh\nside_effect: external_write\nrecovery: retry", "Work.")
	assertLintCode(t, dir, FindingSideEffectRetryWithoutReconcile)
}

func TestTeamLintStrictTaskWithoutTypedResult(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "execution-profile: strict-verification\nacceptance:\n  mode: blocking\n  require-no-unresolved-tasks: true\ntasks:\n  - agent: worker\n    goal: verify carefully\n")
	assertLintCode(t, dir, FindingStrictTaskWithoutTypedResult)
}

func TestTeamLintAcceptanceMissingForUnattended(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "unattended: true\n")
	assertLintCode(t, dir, FindingAcceptanceMissingUnattended)
}

func TestTeamLintVerifierMissingReusesExistingCode(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - agent: worker\n    goal: verify\n    execution:\n      requires-verification: true\n")
	assertLintCode(t, dir, FindingVerifierMissing)
}

func TestTeamLintStructuredVerifierMissingReusesExistingCode(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - agent: worker\n    goal: execute\n    execution:\n      kind: process\n      requires-verification: true\n      steps:\n        - id: inspect\n          tool: view\n          effect: read\n")
	assertLintCode(t, dir, FindingExecutionStepsVerifierMissing)
}

func TestTeamLintResourceClaimConflict(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - agent: worker\n    goal: first\n    resources: [{resource: repo, mode: write}]\n  - agent: worker\n    goal: second\n    resources: [{resource: repo, mode: read}]\n")
	assertLintCode(t, dir, FindingResourceClaimConflict)
}

func TestTeamLintTimeoutImpossible(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "timeout: 60\nverify-timeout: 60\ntasks:\n  - agent: worker\n    goal: verify\n    verify: test -f result.txt\n")
	assertLintCode(t, dir, FindingTimeoutImpossible)
}

func TestTeamLintLegacyFanoutReusesExistingCode(t *testing.T) {
	dir := lintTeamWithCoordinator(t, "tasks:\n  - id: fanout\n    agent: worker\n    goal: fan out\n    fan_out:\n      source: items.tsv\n      goal-template: process {key}\n")
	assertLintCode(t, dir, FindingLegacyFanOutDeprecated)
}

func TestBundledTeamsHaveNoLintErrors(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".agent-teams"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			result, err := LintTeam(filepath.Join(root, entry.Name()), nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			var blocking []TeamLintFinding
			for _, finding := range result.Findings {
				if finding.Severity == FindingSeverityError && !finding.Ignored {
					blocking = append(blocking, finding)
				}
			}
			if len(blocking) > 0 {
				t.Fatalf("blocking findings = %#v", blocking)
			}
		})
	}
}

func lintTeamWithAgentBody(t *testing.T, frontmatter, body string) string {
	t.Helper()
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "name: lint-test\n")
	writeLintTestFile(t, filepath.Join(dir, "coordinator.md"), lintAgent("coordinator", "coordinator"))
	writeLintTestFile(t, filepath.Join(dir, "worker.md"), "---\nname: worker\nrole: worker\n"+frontmatter+"\n---\n"+body+"\n")
	return dir
}

func writeSkillFixture(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeLintTestFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: "+name+"\ndescription: test skill\n---\n"+body+"\n")
}

func assertLintNoCode(t *testing.T, dir string, codes ...string) {
	t.Helper()
	result, err := LintTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range codes {
		if finding, ok := findLintCode(result.Findings, code); ok {
			t.Fatalf("unexpected %s finding: %#v; all=%#v", code, finding, result.Findings)
		}
	}
}

func findLintCode(findings []TeamLintFinding, code string) (TeamLintFinding, bool) {
	for _, finding := range findings {
		if finding.Code == code {
			return finding, true
		}
	}
	return TeamLintFinding{}, false
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
