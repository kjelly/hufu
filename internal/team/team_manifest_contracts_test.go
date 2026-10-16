package team

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestCanonicalContractAliasParity(t *testing.T) {
	for _, phases := range []bool{false, true} {
		var baseline []TaskDef
		var compiledBaseline []EffectiveTaskContract
		var snapshotBaseline *CompatSnapshot
		for _, format := range []struct {
			key                 string
			versioned, advanced bool
		}{
			{"tasks", false, false}, {"tasks", false, true}, {"tasks", true, false}, {"contracts", false, false}, {"contracts", true, false},
		} {
			dir := t.TempDir()
			writeTeamManifest(t, dir, contractManifestFixture(format.key, format.versioned, format.advanced, phases))
			tasks, err := loadTeamContractTasks(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := parseTeamYML(dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Delegation = agent.DelegationPolicy{BindTaskGoalContracts: true}
			session := &TeamSession{Config: cfg, ContractTasks: tasks}
			_, compiled, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "worker", ContractID: "inspect", Goal: "inspect"}})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := BuildCompatSnapshot(session)
			if err != nil {
				t.Fatal(err)
			}
			if baseline == nil {
				baseline, compiledBaseline, snapshotBaseline = tasks, compiled, snapshot
			}
			if !reflect.DeepEqual(tasks, baseline) || !reflect.DeepEqual(compiled, compiledBaseline) || !reflect.DeepEqual(snapshot, snapshotBaseline) {
				t.Fatalf("alias %s (versioned=%v, advanced=%v) changed semantics", format.key, format.versioned, format.advanced)
			}
		}
	}
}

func TestManifestContractDeclarationsFailClosed(t *testing.T) {
	for _, input := range []string{
		"contracts: []\ntasks: []\n", "contracts: null\ntasks: null\n",
		"contracts: []\ntasks: null\n", "contracts: null\ntasks: []\n",
		"tasks: []\nadvanced:\n  tasks: null\n", "contracts: []\nadvanced:\n  tasks: []\n",
		"contracts: []\ntasks:\n" + indentContractFixture(contractAliasFixture, 2),
		"contracts: []\ncontracts: []\n", "advanced:\n  contracts: []\n",
		"apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata: {name: test}\nspec:\n  contracts: []\n  tasks: null\n",
		"apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata: {name: test}\nspec:\n  advanced: {tasks: []}\n",
		"vars:\n  base: &base {contracts: []}\n<<: *base\ntasks: null\n",
		"vars:\n  base: &base {tasks: []}\nadvanced:\n  <<: *base\ncontracts: null\n",
		"vars:\n  first: &first {contracts: []}\n  second: &second {tasks: null}\n<<: [*first, *second]\n",
	} {
		if _, _, err := decodeTeamManifestYAML("team.yaml", []byte(input)); err == nil {
			t.Errorf("accepted conflicting/unsupported declarations:\n%s", input)
		}
	}
	for _, input := range []string{"tasks: null\n", "contracts: null\n", "tasks: []\n", "advanced: {tasks: null}\n", "name: empty\n"} {
		yc, _, err := decodeTeamManifestYAML("team.yaml", []byte(input))
		if err != nil || len(yc.Contracts) != 0 {
			t.Errorf("empty legacy declaration: %+v, %v", yc.Contracts, err)
		}
	}
	_, _, err := decodeTeamManifestYAML("team.yaml", []byte("contracts: []\nadvanced: {tasks: null}\n"))
	if err == nil || !strings.Contains(err.Error(), "contracts") || !strings.Contains(err.Error(), "advanced.tasks") {
		t.Fatalf("missing conflict paths: %v", err)
	}
}

func TestContractCollectionsYAMLMergeAndAliases(t *testing.T) {
	for _, input := range []string{
		"vars:\n  base: &base\n    contracts:\n" + indentContractFixture(contractAliasFixture, 6) + "<<: *base\n",
		"vars:\n  list: &list\n" + indentContractFixture(contractAliasFixture, 4) + "contracts: *list\n",
		// Explicitly overriding the same key is standard YAML, not two aliases.
		"vars:\n  base: &base {tasks: null}\n<<: *base\ntasks:\n" + indentContractFixture(contractAliasFixture, 2),
	} {
		yc, _, err := decodeTeamManifestYAML("team.yaml", []byte(input))
		if err != nil || len(yc.Contracts) != 1 || yc.Contracts[0].ID != "inspect" {
			t.Fatalf("merged contract: %+v, %v", yc.Contracts, err)
		}
	}
}

func TestContractLintSourceProjectionAndDeprecation(t *testing.T) {
	for _, format := range []struct {
		key, path           string
		versioned, advanced bool
	}{
		{"contracts", "contracts", false, false}, {"contracts", "spec.contracts", true, false},
		{"tasks", "tasks", false, false}, {"tasks", "spec.tasks", true, false}, {"tasks", "advanced.tasks", false, true},
	} {
		dir := t.TempDir()
		source := contractManifestFixture(format.key, format.versioned, format.advanced, false)
		writeTeamManifest(t, dir, source)
		index, err := BuildTeamSourceIndex(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		findings := ProjectContractFindings([]ContractFinding{{Code: "test", Severity: FindingSeverityError, Field: "tasks[0].agent"}}, index, nil)
		wantLine := 1 + strings.Count(source[:strings.Index(source, "agent: worker")], "\n")
		if len(findings) != 1 || findings[0].FieldPath != format.path+"[0].agent" || findings[0].Line != wantLine || findings[0].File != "team.yaml" {
			t.Fatalf("projection = %+v", findings)
		}
		legacy := lintLegacyTasksKey(index)
		if format.key == "contracts" {
			if len(legacy) != 0 {
				t.Fatal("canonical contracts warned")
			}
			continue
		}
		if len(legacy) != 1 || legacy[0].Code != FindingLegacyTasksKeyDeprecated || legacy[0].Field != format.path {
			t.Fatalf("legacy warning = %+v", legacy)
		}
	}
	dir := lintTeamWithCoordinator(t, "tasks: []\n")
	normal, err := LintTeamWithOptions(dir, TeamLintOptions{})
	if err != nil {
		t.Fatal(err)
	}
	strict, err := LintTeamWithOptions(dir, TeamLintOptions{RejectLegacyTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	warning, ok := findLintCode(normal.Findings, FindingLegacyTasksKeyDeprecated)
	if !ok || warning.Severity != FindingSeverityWarning || warning.Line == 0 || warning.Suggestion == "" {
		t.Fatalf("warning = %+v", warning)
	}
	failure, ok := findLintCode(strict.Findings, FindingLegacyTasksKeyDeprecated)
	if !ok || failure.Severity != FindingSeverityError {
		t.Fatalf("strict warning = %+v", failure)
	}
}

func TestCanonicalContractsMigrationPreview(t *testing.T) {
	for _, source := range []string{
		contractManifestFixture("tasks", false, false, false), contractManifestFixture("tasks", false, true, true),
		contractManifestFixture("tasks", true, false, true), contractManifestFixture("contracts", true, false, false),
		"name: empty\ntasks: []\n", "name: empty\ntasks: null\n",
		"name: templated\ntasks:\n  - id: inspect\n    agent: worker\n    goal: '{@ .scope @} {{ downstream }}'\n",
		"name: anchored\nvars:\n  base: &base\n    tasks:\n" + indentContractFixture(contractAliasFixture, 6) + "<<: *base\n",
	} {
		dir := t.TempDir()
		writeTeamManifest(t, dir, source)
		path := filepath.Join(dir, "team.yaml")
		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		output, _, _, err := MigrateTeamManifestToV1Alpha1WithOptions(dir, TeamManifestMigrationOptions{CanonicalContracts: true})
		if err != nil {
			t.Fatal(err)
		}
		original, _, err := decodeTeamManifestYAML(path, []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		migrated, _, err := decodeTeamManifestYAML("team.yaml", output)
		if err != nil {
			t.Fatal(err)
		}
		if err := verifyContractMigration(original.Contracts, migrated.Contracts); err != nil {
			t.Fatal(err)
		}
		if len(original.Contracts) > 0 && !bytes.Contains(output, []byte("contracts:")) {
			t.Fatalf("not canonical:\n%s", output)
		}
		unchanged, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(unchanged) != source || !before.ModTime().Equal(after.ModTime()) {
			t.Fatal("preview modified input")
		}
		legacy, _, err := MigrateTeamManifestToV1Alpha1(dir)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(legacy, []byte("\n    contracts:")) {
			t.Fatal("default migration changed spelling")
		}
		roundTrip := t.TempDir()
		writeTeamManifest(t, roundTrip, string(output))
		second, _, _, err := MigrateTeamManifestToV1Alpha1WithOptions(roundTrip, TeamManifestMigrationOptions{CanonicalContracts: true})
		if err != nil || !bytes.Equal(output, second) {
			t.Fatalf("migration not idempotent: %v", err)
		}
	}
}

func TestCanonicalContractsMigrationRejectsUnrenderedTypedTemplate(t *testing.T) {
	dir := t.TempDir()
	source := "name: templated\ntasks:\n  - agent: worker\n    max_retries: '{@ .retries @}'\n"
	writeTeamManifest(t, dir, source)
	if _, _, _, err := MigrateTeamManifestToV1Alpha1WithOptions(dir, TeamManifestMigrationOptions{CanonicalContracts: true}); err == nil || !strings.Contains(err.Error(), "unrendered manifest") {
		t.Fatalf("typed template error = %v", err)
	}
}
