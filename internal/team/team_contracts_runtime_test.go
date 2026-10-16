package team

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Exercise real authored contracts, including action payloads and fan-out,
// beyond the fields retained by the existing compatibility golden fixtures.
func TestBundledContractAliasIdentityAndWorkflowParity(t *testing.T) {
	for _, name := range []string{"hufu-coding", "hufu-code-review", "web-research", "article-debate"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(bundledTeamsDir(t), name)
			data, filename, found, err := readTeamManifestSource(dir, nil)
			if err != nil || !found {
				t.Fatalf("read manifest: %v", err)
			}
			canonical, _, err := decodeTeamManifestYAML(filename, data)
			if err != nil {
				t.Fatal(err)
			}
			legacyData := strings.ReplaceAll(strings.ReplaceAll(string(data), "\n  contracts:\n", "\n  tasks:\n"), "\ncontracts:\n", "\ntasks:\n")
			legacy, _, err := decodeTeamManifestYAML(filename, []byte(legacyData))
			if err != nil {
				t.Fatal(err)
			}
			if len(canonical.Contracts) == 0 || !reflect.DeepEqual(canonical.Contracts, legacy.Contracts) {
				t.Fatal("bundled typed contracts changed")
			}
			for i, contract := range canonical.Contracts {
				before, err := migrationContractHash(legacy.Contracts[i])
				if err != nil {
					t.Fatal(err)
				}
				after, err := migrationContractHash(contract)
				if err != nil {
					t.Fatal(err)
				}
				if before != after {
					t.Fatalf("contract %d identity changed", i)
				}
			}
			session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := BuildCompatSnapshot(session)
			if err != nil {
				t.Fatal(err)
			}
			legacyDir := t.TempDir()
			if err := os.CopyFS(legacyDir, os.DirFS(dir)); err != nil {
				t.Fatal(err)
			}
			unrendered, err := os.ReadFile(filepath.Join(dir, filename))
			if err != nil {
				t.Fatal(err)
			}
			legacySource := strings.ReplaceAll(strings.ReplaceAll(string(unrendered), "\n  contracts:\n", "\n  tasks:\n"), "\ncontracts:\n", "\ntasks:\n")
			writeTeamManifest(t, legacyDir, legacySource)
			legacySession, err := LoadTeam(legacyDir, nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := BuildCompatSnapshot(legacySession)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(baseline, snapshot) {
				t.Fatal("bundled compatibility snapshot changed")
			}
			legacySession.Workspace = t.TempDir()
			before, err := newRuntimeWorkflow(legacySession)
			if err != nil {
				t.Fatal(err)
			}
			session.Workspace = t.TempDir()
			after, err := newRuntimeWorkflow(session)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.phaseContracts, after.phaseContracts) || !reflect.DeepEqual(before.phaseOptionalContracts, after.phaseOptionalContracts) || !reflect.DeepEqual(before.phases, after.phases) {
				t.Fatal("workflow phase admission changed")
			}
		})
	}
}

func TestContractAliasRestoresWorkflowCheckpoint(t *testing.T) {
	session := workflowTestSession(t)
	session.ContractTasks = append(session.ContractTasks, TaskDef{ID: "extra", Agent: "preparer", Phase: PhasePrepare, Optional: true})
	tasks, err := yaml.Marshal(session.ContractTasks)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _, err := decodeTeamManifestYAML("team.yaml", append([]byte("tasks:\n"), tasks...))
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, err := decodeTeamManifestYAML("team.yaml", append([]byte("contracts:\n"), tasks...))
	if err != nil {
		t.Fatal(err)
	}
	session.ContractTasks = legacy.Contracts
	before, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Start(); err != nil {
		t.Fatal(err)
	}
	if err := before.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	state, results, root, retry := before.snapshot()
	session.ContractTasks = canonical.Contracts
	after, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := after.restore(state, results, root, retry); err != nil {
		t.Fatal(err)
	}
	if after.State() != before.State() || !reflect.DeepEqual(before.phaseOptionalContracts, after.phaseOptionalContracts) {
		t.Fatal("alias rename changed restored workflow")
	}
}

func TestContractMigrationDetectsFieldsMissingFromCompatSnapshot(t *testing.T) {
	original := TaskDef{ID: "inspect", Agent: "worker", Action: &Action{Capability: "structured-actions", Type: "inspect", Payload: `{"target":"safe"}`}, Verify: "test -f result.txt"}
	for _, mutate := range []func(*TaskDef){
		func(task *TaskDef) { task.Verify = "true" },
		func(task *TaskDef) {
			task.Action = &Action{Capability: "structured-actions", Type: "inspect", Payload: `{"target":"changed"}`}
		},
		func(task *TaskDef) { task.Execution.RequiresResult = true },
	} {
		changed := original
		mutate(&changed)
		if err := verifyContractMigration([]TaskDef{original}, []TaskDef{changed}); err == nil {
			t.Fatal("migration accepted a semantic change")
		}
	}
}

func TestContractAliasKeepsSemanticSelectorForbidden(t *testing.T) {
	for _, key := range []string{"tasks", "contracts"} {
		manifest := key + ":\n  - id: inspect\n    agent: preparer\n    when-goal-contains: inspect\n"
		parsed, _, err := decodeTeamManifestYAML("team.yaml", []byte(manifest))
		if err != nil {
			t.Fatal(err)
		}
		session := workflowTestSession(t)
		session.ContractTasks = parsed.Contracts
		found := false
		for _, finding := range ValidateTeamTaskContracts(session) {
			if finding.Code == "semantic_contract_selector_forbidden" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s revived semantic contract selection", key)
		}
	}
}
