package team

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestDependencyResultViewPolicyVersionAndDrift(t *testing.T) {
	c := newTestCoordinator(t)
	manager, err := agent.NewProviderManager("http://127.0.0.1:1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	session := dependencyViewSession()
	c.session.ContractTasks = session.ContractTasks
	c.session.Config.Delegation = session.Config.Delegation
	c.session.Agents = session.Agents
	c.session.Agents["producer"].Generation.Model = "test"
	selected := c.session.ContractTasks[1].DependencyResultView
	c.session.ContractTasks[1].DependencyResultView = nil
	full, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if full.snapshot.Version != executionPolicySnapshotVersion || full.snapshot.DependencyResultViewsHash != "" {
		t.Fatalf("full policy changed shape: %+v", full.snapshot)
	}
	fullBytes, err := json.Marshal(full.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	c.session.ContractTasks[1].DependencyResultView = &DependencyResultViewSpec{Mode: "full"}
	explicit, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	explicitBytes, err := json.Marshal(explicit.snapshot)
	if err != nil || string(fullBytes) != string(explicitBytes) {
		t.Fatalf("explicit full changed frozen policy: %v", err)
	}
	c.session.ContractTasks[1].DependencyResultView = selected
	current, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := current.snapshot
	if snapshot.Version != selectedDependencyPolicyVersion || !validFullContentHash(snapshot.DependencyResultViewsHash) || snapshot.ConfigurationHash == full.snapshot.ConfigurationHash {
		t.Fatalf("selected policy not pinned: %+v", snapshot)
	}
	if err := ValidateExecutionPolicySnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{3, 4, 5} {
		if _, err := newExecutionPolicyStateForVersion(c, version); err == nil {
			t.Fatalf("historical v%d admitted selected", version)
		}
	}
	c.executionPolicy = current
	c.SetSessionData(&SessionData{})
	if err := c.FreezeExecutionPolicyAtStartup(); !errors.Is(err, errDependencyResultViewUnavailable) {
		t.Fatalf("selected startup did not reject before provider probes: %v", err)
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read frozen selected policy: %v, %v", err, closeErr)
	}
	durable, err := executionPolicySnapshotFromEvents(events)
	if err != nil || durable == nil || durable.ConfigurationHash != snapshot.ConfigurationHash {
		t.Fatalf("startup did not seal policy before refusing execution: %v", err)
	}
	selected.Fields = []string{"summary", "verification"}
	reordered, err := newExecutionPolicyState(c)
	if err != nil || reordered.snapshot.ConfigurationHash != snapshot.ConfigurationHash {
		t.Fatalf("author ordering changed policy: %v", err)
	}
	selected.Fields = []string{"summary"}
	if same, err := c.executionPolicySnapshotMatchesCurrent(snapshot); err == nil && same {
		t.Fatal("selected policy drift accepted")
	}
	c.session.ContractTasks[1].DependencyResultView = nil
	if same, err := c.executionPolicySnapshotMatchesCurrent(snapshot); err == nil && same {
		t.Fatal("selected policy removal accepted")
	}
	corrupt := cloneExecutionPolicySnapshot(snapshot)
	corrupt.DependencyResultViewsHash = strings.Repeat("a", 64)
	if err := ValidateExecutionPolicySnapshot(corrupt); err == nil {
		t.Fatal("tampered policy accepted")
	}
	corrupt = cloneExecutionPolicySnapshot(snapshot)
	corrupt.Version = executionPolicySnapshotVersion
	corrupt.ConfigurationHash, err = executionPolicyConfigurationHash(corrupt)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExecutionPolicySnapshot(corrupt); err == nil {
		t.Fatal("selected policy was downgraded to legacy version")
	}
}

func TestDependencyResultViewManifestPolicyPresence(t *testing.T) {
	for _, input := range []string{
		"contracts: [{id: review, agent: worker, dependency-result-view: null}]",
		"vars: {missing: &missing null}\ncontracts: &contracts [{id: review, agent: worker, dependency-result-view: *missing}]",
		"vars: {contracts: &contracts [{id: review, agent: worker, dependency-result-view: null}]}\ncontracts: *contracts",
		"tasks: [{id: review, agent: worker, dependency-result-view: null}]",
		"advanced: {tasks: [{id: review, agent: worker, dependency-result-view: null}]}",
		"contracts: [{id: review, agent: worker, dependency-result-view: {mode: selected, fields: [summary], other: true}}]",
	} {
		if _, _, err := decodeTeamManifestYAML("team.yaml", []byte(input)); err == nil {
			t.Fatalf("invalid manifest accepted: %s", input)
		}
	}
	input := `contracts:
 - id: implement
   agent: producer
 - id: review
   agent: worker
   depends-on-contracts: [implement]
   dependency-result-view:
     mode: selected
     fields: [verification, summary]
`
	cfg, _, err := decodeTeamManifestYAML("team.yaml", []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Contracts) != 2 || cfg.Contracts[1].DependencyResultView == nil || strings.Join(cfg.Contracts[1].DependencyResultView.Fields, ",") != "summary,verification" {
		t.Fatalf("manifest lost canonical view: %+v", cfg.Contracts)
	}
}

func TestDependencyResultViewManifestMigration(t *testing.T) {
	source := `name: dependency-view
tasks:
  - id: produce
    agent: producer
  - id: consume
    agent: worker
    depends-on-contracts: [produce]
    dependency-result-view:
      mode: selected
      fields: [verification, summary]
`
	dir := t.TempDir()
	writeTeamManifest(t, dir, source)
	original, _, err := decodeTeamManifestYAML("team.yaml", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, canonical := range []bool{false, true} {
		output, _, _, err := MigrateTeamManifestToV1Alpha1WithOptions(dir, TeamManifestMigrationOptions{CanonicalContracts: canonical})
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
		if len(migrated.Contracts) != 2 || migrated.Contracts[1].DependencyResultView == nil {
			t.Fatal("migration lost selected policy")
		}
	}
}
