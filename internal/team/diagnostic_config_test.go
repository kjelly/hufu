package team

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/belief"
)

func diagnosticConfigFixture(t *testing.T) *TeamSession {
	t.Helper()
	dir := t.TempDir()
	writeBeliefModel(t, dir)
	data, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		t.Fatal(err)
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		t.Fatal(err)
	}
	second := model.Experiments[0]
	second.ID, second.CorrelationGroup = "second", "second"
	model.Experiments = append(model.Experiments, second)
	policy := belief.DiagnosticPolicy{SchemaVersion: 1, Version: "1", Algorithm: belief.DiagnosticGiniAlgorithm, Threshold: 900_000_000,
		Limits:     belief.DiagnosticLimits{MaxRounds: 2, MaxAttempts: 2, MaxCostUnits: 2, ActionTimeoutSeconds: 5, EpisodeTimeoutSeconds: 20, MaxEvents: 32, MaxObservationBytes: 16384, MaxFenceRetries: 3},
		Candidates: []belief.DiagnosticCandidate{{ID: "check-a", ExperimentID: "comparison", CostUnits: 1}, {ID: "check-b", ExperimentID: "second", CostUnits: 1}}}
	adapter := diagnosticAdapter{SchemaVersion: 1, Version: "diagnostic_measurement_v1", Source: "verification", Experiments: []string{"comparison", "second"}, Outcomes: []string{"pass", "fail"}}
	for name, value := range map[string]any{"model.json": model, "policy.json": policy, "adapter.json": adapter} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &TeamSession{Dir: dir, Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "diagnostic-test", GoalMode: "exploratory", DiagnosticLoop: &agent.DiagnosticLoopConfig{Mode: "off", ModelFile: "model.json", PolicyFile: "policy.json", AdapterFile: "adapter.json", Actor: "worker"}}, Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker"}}, ActionCatalog: beliefTestCatalog(t, "worker")}
}

func TestDiagnosticConfigurationPreparationAndLegacyHash(t *testing.T) {
	session := diagnosticConfigFixture(t)
	if err := compileTeamDiagnosticLoop(session); err != nil {
		t.Fatal(err)
	}
	snapshot := executionPolicyDiagnostic(session)
	if err := validateExecutionDiagnosticSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Mode != "off" || session.Belief != nil {
		t.Fatal("preparation enabled a runtime adapter")
	}
	if !session.DiagnosticLoop.model.Synthetic || session.DiagnosticLoop.policy.Algorithm != belief.DiagnosticGiniAlgorithm || session.DiagnosticLoop.adapter.Version != belief.DiagnosticAdapterVersion {
		t.Fatal("compiled inputs were not retained for preparation")
	}
	snapshot.Actor = "other"
	if executionPolicyDiagnostic(session).Actor != "worker" || validateExecutionDiagnosticSnapshot(snapshot) == nil {
		t.Fatal("snapshot was mutable or binding was not validated")
	}
	// Reproduce the historical JSON bytes by removing the absent additive field.
	legacy := &ExecutionPolicySnapshot{Version: 5}
	before, err := executionPolicyConfigurationHash(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// The historical clone canonicalizes nil execution_worlds to an empty array.
	const legacyHash = "21ea2221289f8f71e23f3b4cac7cd66912bcb06abce7206c8cc63be23473b5b6"
	if before != legacyHash {
		t.Fatalf("legacy policy hash changed: %s", before)
	}
	data, err := json.Marshal(legacy)
	if err != nil || strings.Contains(string(data), "diagnostic_loop") {
		t.Fatalf("absent configuration changed legacy wire shape: %s %v", data, err)
	}
	legacy.DiagnosticLoop = executionPolicyDiagnostic(session)
	after, err := executionPolicyConfigurationHash(legacy)
	if err != nil || after == before {
		t.Fatal("prepared contract was not fingerprinted")
	}
	cloned := cloneExecutionPolicySnapshot(legacy)
	cloned.DiagnosticLoop.ModelDigest = "mutated"
	if legacy.DiagnosticLoop.ModelDigest == "mutated" {
		t.Fatal("snapshot clone shares diagnostic binding")
	}
	legacy.DiagnosticLoop = nil
	restored, err := executionPolicyConfigurationHash(legacy)
	if err != nil || restored != before {
		t.Fatal("disabled legacy policy hash changed")
	}
}

func TestDiagnosticConfigurationFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*TeamSession){
		"shadow unavailable":    func(s *TeamSession) { s.Config.DiagnosticLoop.Mode = "shadow" },
		"executor unavailable":  func(s *TeamSession) { s.Config.DiagnosticLoop.Mode = "bounded_read_only" },
		"missing explicit mode": func(s *TeamSession) { s.Config.DiagnosticLoop.Mode = "" },
		"actor unknown":         func(s *TeamSession) { s.Config.DiagnosticLoop.Actor = "unknown" },
		"actor case":            func(s *TeamSession) { s.Config.DiagnosticLoop.Actor = "Worker" },
		"model traversal":       func(s *TeamSession) { s.Config.DiagnosticLoop.ModelFile = "../model.json" },
		"policy missing":        func(s *TeamSession) { s.Config.DiagnosticLoop.PolicyFile = "missing" },
		"adapter absolute":      func(s *TeamSession) { s.Config.DiagnosticLoop.AdapterFile = "/adapter.json" },
		"catalog missing":       func(s *TeamSession) { s.ActionCatalog = nil },
		"write action":          func(s *TeamSession) { s.ActionCatalog.Entries[0].SideEffect = SideEffectWorkspaceWrite },
		"missing grant":         func(s *TeamSession) { s.ActionCatalog.Entries[0].Propose = nil },
	} {
		t.Run(name, func(t *testing.T) {
			session := diagnosticConfigFixture(t)
			mutate(session)
			if err := compileTeamDiagnosticLoop(session); err == nil {
				t.Fatal("accepted unsafe or unsupported configuration")
			}
		})
	}
	for _, content := range []string{`{}`, `{"schema_version":1,"version":"unknown","source":"verification","experiments":["comparison","second"],"outcomes":["pass","fail"]}`, `{"schema_version":1,"version":"diagnostic_measurement_v1","source":"verification","experiments":["comparison","comparison"],"outcomes":["pass","fail"]}`} {
		session := diagnosticConfigFixture(t)
		if err := os.WriteFile(filepath.Join(session.Dir, "adapter.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := compileTeamDiagnosticLoop(session); err == nil {
			t.Fatal("accepted invalid adapter contract")
		}
	}
	session := diagnosticConfigFixture(t)
	if err := os.Symlink("model.json", filepath.Join(session.Dir, "alias.json")); err != nil {
		t.Fatal(err)
	}
	session.Config.DiagnosticLoop.ModelFile = "alias.json"
	if err := compileTeamDiagnosticLoop(session); err == nil {
		t.Fatal("accepted symlinked resource")
	}
}

func TestDiagnosticManifestStrictDecoding(t *testing.T) {
	for _, manifest := range []string{
		"name: test\ndiagnostic-loop:\n  mode: off\n  model-file: model.json\n  policy-file: policy.json\n  adapter-file: adapter.json\n  actor: worker\n",
		"apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: test\nspec:\n  diagnostic-loop:\n    mode: off\n    model-file: model.json\n    policy-file: policy.json\n    adapter-file: adapter.json\n    actor: worker\n",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "team.yaml")
		if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := parseTeamYML(dir, nil)
		if err != nil || cfg.DiagnosticLoop == nil || cfg.DiagnosticLoop.Mode != "off" {
			t.Fatalf("manifest parse=%+v %v", cfg.DiagnosticLoop, err)
		}
		indent := "  "
		if strings.Contains(manifest, "    actor: worker") {
			indent = "    "
		}
		bad := strings.Replace(manifest, "actor: worker", "actor: worker\n"+indent+"unknown-option: true", 1)
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseTeamYML(dir, nil); err == nil {
			t.Fatal("unknown nested option accepted")
		}
	}
}

func TestDiagnosticPreparedSnapshotSurvivesAdmissionAndRejectsDrift(t *testing.T) {
	session := diagnosticConfigFixture(t)
	c, err := newCoordinator(coordinatorParams{Session: session, MaxConcurrent: 1}, RuntimeServices{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.initEventStore()
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	snapshot := c.ExecutionPolicySnapshot()
	if snapshot.DiagnosticLoop == nil || !reflect.DeepEqual(snapshot.DiagnosticLoop, executionPolicyDiagnostic(session)) {
		t.Fatal("execution policy did not freeze prepared contracts")
	}
	for _, item := range c.TaskTracker().TodoList().Items() {
		if item.CatalogAction != nil {
			t.Fatal("preparation created an action task")
		}
	}
	data, err := os.ReadFile(filepath.Join(session.Dir, "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"version":"1"`, `"version":"2"`, 1))
	if err := os.WriteFile(filepath.Join(session.Dir, "policy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compileTeamDiagnosticLoop(session); err != nil {
		t.Fatal(err)
	}
	changed := executionPolicyDiagnostic(session)
	if changed.BindingDigest == snapshot.DiagnosticLoop.BindingDigest {
		t.Fatal("changed contract did not affect binding")
	}
	if !reflect.DeepEqual(c.ExecutionPolicySnapshot().DiagnosticLoop, snapshot.DiagnosticLoop) {
		t.Fatal("current file rewrote a frozen runtime snapshot")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := newCoordinator(coordinatorParams{Session: session, MaxConcurrent: 1}, RuntimeServices{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close() })
	if saved := LoadSession(session.Workspace); saved != nil {
		resumed.SetSessionData(saved)
	}
	resumed.initEventStore()
	if err := resumed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("changed contract did not reject resume: %v", err)
	}
}
