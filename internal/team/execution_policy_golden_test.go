package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// TestExecutionPolicySnapshotGolden pins the canonical policy snapshot of a
// team that uses no result contract and no execution route. The JSON must stay
// byte-for-byte stable for new sessions. Historical tool sessions are checked
// against the separate pre-collection fixture below. Environment values use
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

func TestExecutionPolicyHistoricalToolTransportRemainsFrozen(t *testing.T) {
	t.Setenv("HOME", "/hufu-golden/home")
	t.Setenv("CODEX_HOME", "/hufu-golden/codex-home")
	t.Setenv("PATH", "/hufu-golden/bin")
	c := newExecutionPolicySnapshotCoordinatorWithOptions(t, t.TempDir(), 4, 3, executionPolicySnapshotCoordinatorOptions{
		codexConfig: defaultExecutionPolicyCodexConfig(), subjectRoot: t.TempDir(),
	})
	data, err := os.ReadFile(filepath.Join("testdata", "execution-policy", "legacy-tool-transport.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var historical ExecutionPolicySnapshot
	if err := json.Unmarshal(data, &historical); err != nil {
		t.Fatal(err)
	}
	current := c.ExecutionPolicySnapshot()
	for i := range historical.ExecutionWorlds {
		historical.ExecutionWorlds[i].ProjectRootHash = current.ExecutionWorlds[i].ProjectRootHash
	}
	historical.ConfigurationHash, err = executionPolicyConfigurationHash(&historical)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(&historical)
	if ok, err := c.executionPolicySnapshotMatchesCurrent(&historical); err != nil || !ok {
		t.Fatalf("legacy policy drifted: %v", err)
	}
	c.SetSessionData(&SessionData{ExecutionPolicySnapshot: &historical})
	c.initEventStore()
	persisted, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(EventExecutionPolicySnapshot), Actor: "coordinator", Payload: before})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ensureExecutionPolicySnapshot(); err != nil {
		t.Fatalf("historical journal admission failed: %v", err)
	}
	projected, err := json.Marshal(c.SessionData().ExecutionPolicySnapshot)
	if err != nil || string(projected) != string(before) {
		t.Fatalf("admission rewrote the historical policy: err=%v", err)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policyEvents := 0
	for _, event := range events {
		if event.Type == string(EventExecutionPolicySnapshot) {
			policyEvents++
			if string(event.Payload) != string(persisted.Payload) {
				t.Fatal("historical policy bytes changed in journal")
			}
		}
	}
	if policyEvents != 1 {
		t.Fatalf("admission appended a new policy: count=%d", policyEvents)
	}
	mode, err := c.resultTransportForTask(c.session.Agents["worker"], TaskDef{Agent: "worker"})
	if err != nil || mode != "tool" {
		t.Fatalf("historical transport=%s err=%v", mode, err)
	}
	after, _ := json.Marshal(&historical)
	if string(before) != string(after) {
		t.Fatal("compatibility mutated the historical snapshot")
	}
	for _, mode := range []agent.ResultTransport{agent.ResultTransportTool, agent.ResultTransportRuntime} {
		c.session.Config.ResultTransport = mode
		if ok, err := c.executionPolicySnapshotMatchesCurrent(&historical); err != nil || ok {
			t.Fatalf("authored transport change admitted: mode=%s err=%v", mode, err)
		}
	}
}
