package inspect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
	"github.com/kjelly/hufu/internal/team"
)

func beliefInspectPolicy(t *testing.T, model belief.Model) *team.BoundBeliefPolicy {
	t.Helper()
	dir := t.TempDir()
	manifest := `name: synthetic-policy-inspection
action-providers:
  diagnostics:
    command: [/bin/true]
action-catalog:
  check-b:
    description: Synthetic advisory candidate; never executed by inspection.
    capability: diagnostics
    type: check
    agent: worker
    side-effect: none
    input-schema:
      type: object
      additional-properties: false
    access:
      discover: [worker]
      propose: [worker]
`
	for name, data := range map[string]string{"team.yaml": manifest, "worker.md": "---\nname: worker\ndescription: Synthetic fixture worker.\ntools: view\n---\nRead only.\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	session, err := team.LoadTeam(dir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	policy := belief.Policy{SchemaVersion: 1, Version: "synthetic-v1", Algorithm: belief.PolicyAlgorithm, Mode: "advisory", Choices: []belief.PolicyChoice{{HypothesisID: "h1", CandidateID: "check-b"}, {HypothesisID: "h2", CandidateID: "check-b"}}}
	bound, err := team.CompileBeliefPolicy(model, policy, session.ActionCatalog)
	if err != nil {
		t.Fatal(err)
	}
	return &bound
}

func TestBeliefPolicyInspectReadOnlyAndMissingPolicy(t *testing.T) {
	workspace, digest, _ := beliefInspectFixture(t, true)
	before := snapshotInspectWorkspace(t, workspace)
	query := InspectQuery{Workspace: workspace, RunID: "run-belief", TaskID: "task-1"}
	result, err := InspectBeliefs(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(BeliefsData)
	if len(data.Policies) != 1 || data.Policies[0].Unavailable != "" || len(data.Suggestions) != 1 || data.Suggestions[0].Validation != "verified" {
		t.Fatalf("policy inspection=%+v", data)
	}
	record := data.Suggestions[0].Record
	if record.StateDigest != digest || record.SelectedCandidateID != "check-b" || record.DecisionGeneration != 1 || record.SourceCursorHash == "" || record.SnapshotDigest == "" || record.PolicyDigest != data.Policies[0].Digest {
		t.Fatalf("advisory provenance incomplete: %+v", record)
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, workspace)) {
		t.Fatal("policy inspection changed workspace bytes, modes or timestamps")
	}
	fork, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-belief", BranchID: "early-fork"})
	if err != nil || len(fork.Data.(BeliefsData).Suggestions) != 0 {
		t.Fatal("post-fork advice leaked into child")
	}
	if err := os.Remove(filepath.Join(workspace, "logs", "artifacts", "data", data.Policies[0].ArtifactID)); err != nil {
		t.Fatal(err)
	}
	missing, err := InspectBeliefs(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := missing.Data.(BeliefsData)
	if missing.Integrity.Projection != "unavailable" || unavailable.Policies[0].Unavailable != "policy_unavailable" || unavailable.Suggestions[0].Validation != "unavailable" || !reflect.DeepEqual(record, unavailable.Suggestions[0].Record) || unavailable.Epochs[0].State.Digest != digest {
		t.Fatalf("missing policy rewrote belief or advice: %+v", unavailable)
	}
}
