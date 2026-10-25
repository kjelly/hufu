package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExecutionPolicySnapshotGolden pins the canonical policy snapshot of a
// team that uses no result contract and no execution route. The JSON must stay
// byte-for-byte stable so ConfigurationHash (a sha256 of the same JSON) does
// not drift for teams that do not opt in; new fields must be omitted when
// unused. Path- and environment-dependent values are pinned or replaced by
// placeholders so the golden file is machine-independent.
// Set UPDATE_EXECUTION_POLICY_GOLDEN=1 to regenerate the golden file.
func TestExecutionPolicySnapshotGolden(t *testing.T) {
	t.Setenv("HOME", "/hufu-golden/home")
	t.Setenv("CODEX_HOME", "/hufu-golden/codex-home")
	t.Setenv("PATH", "/hufu-golden/bin")
	c := newExecutionPolicySnapshotCoordinatorWithOptions(t, t.TempDir(), 4, 3, executionPolicySnapshotCoordinatorOptions{
		codexConfig: defaultExecutionPolicyCodexConfig(),
		subjectRoot: t.TempDir(),
	})
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := cloneExecutionPolicySnapshot(state.snapshot)
	for i := range snapshot.ExecutionWorlds {
		if snapshot.ExecutionWorlds[i].ProjectRootHash != "" {
			snapshot.ExecutionWorlds[i].ProjectRootHash = "<project-root-hash>"
		}
	}
	snapshot.ConfigurationHash = "<configuration-hash>"
	got, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	goldenPath := filepath.Join("testdata", "execution-policy", "no-contracts-no-routes.golden.json")
	if os.Getenv("UPDATE_EXECUTION_POLICY_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_EXECUTION_POLICY_GOLDEN=1 to create it): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("policy snapshot for a team without contracts or routes drifted from %s\n--- got ---\n%s", goldenPath, got)
	}
}
