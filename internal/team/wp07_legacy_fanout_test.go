package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyTSVFanOutIsDeprecatedButStillCompatible(t *testing.T) {
	session := &TeamSession{ContractTasks: []TaskDef{{
		ID:     "legacy",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}}}
	var deprecated bool
	for _, finding := range ValidateTeamPolicyContracts(session) {
		if finding.Code == FindingLegacyFanOutDeprecated {
			deprecated = finding.Severity == FindingSeverityWarning
		}
	}
	if !deprecated {
		t.Fatal("legacy TSV fan-out did not emit a warning-only deprecation finding")
	}

	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "inputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "inputs/items.tsv"), []byte("item\nalpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	tasks, err := c.expandFanOutTasks([]TaskDef{{
		ID:     "legacy",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}})
	if err != nil {
		t.Fatalf("legacy TSV compatibility path failed: %v", err)
	}
	if len(tasks) != 3 || tasks[0].Goal != "process alpha" || tasks[2].Goal != "process gamma" {
		t.Fatalf("legacy expansion = %#v", tasks)
	}
}

func TestLegacyAndArtifactFanOutPreserveOrderingAndTemplateSubstitution(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "inputs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "inputs/items.tsv"), []byte("item\nalpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestData, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{
		{Key: "alpha", Bindings: map[string]string{"item": "alpha"}},
		{Key: "beta", Bindings: map[string]string{"item": "beta"}},
		{Key: "gamma", Bindings: map[string]string{"item": "gamma"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "inputs/items.json"), manifestData, 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	legacy, err := c.expandFanOutTasks([]TaskDef{{ID: "legacy", FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"}}})
	if err != nil {
		t.Fatal(err)
	}
	artifactBacked, err := c.expandFanOutTasks([]TaskDef{{ID: "artifact", FanOut: &FanOutSpec{Source: "inputs/items.json", GoalTemplate: "process {item}"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != len(artifactBacked) {
		t.Fatalf("item count mismatch: legacy=%d artifact=%d", len(legacy), len(artifactBacked))
	}
	for index := range legacy {
		if legacy[index].Goal != artifactBacked[index].Goal {
			t.Fatalf("template parity at %d: legacy=%q artifact=%q", index, legacy[index].Goal, artifactBacked[index].Goal)
		}
		if artifactBacked[index].WorksetBinding == nil || artifactBacked[index].WorksetBinding.ItemKey != []string{"alpha", "beta", "gamma"}[index] {
			t.Fatalf("artifact-backed item identity missing at %d: %#v", index, artifactBacked[index].WorksetBinding)
		}
	}
}
