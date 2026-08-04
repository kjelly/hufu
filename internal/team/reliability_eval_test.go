package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestReliabilityScenarioCorpusCoversPhase5Faults(t *testing.T) {
	report := RunReliabilityEvalSuite(nil)
	if len(report.Scenarios) != 8 || report.Metrics.PassedScenarios != 8 {
		t.Fatalf("eval report = %#v, want 8 passing scenarios", report)
	}
	if report.Metrics.DiagnosticDeterminism != 1 {
		t.Fatalf("diagnostic determinism = %v, want 1", report.Metrics.DiagnosticDeterminism)
	}
	for _, scenario := range DefaultReliabilityScenarioCorpus() {
		if scenario.Injection.Detail == "" || scenario.Injection.FailureClass == "" {
			t.Fatalf("scenario %q is missing fault-injection evidence: %#v", scenario.ID, scenario.Injection)
		}
		result, err := ReplayReliabilityScenario(scenario)
		if err != nil || !result.BoundaryReplayed || result.BoundaryEvidence == nil {
			t.Fatalf("scenario %q did not cross a replay boundary: result=%#v err=%v", scenario.ID, result, err)
		}
		if scenario.Fault == FaultSecretLeakage && !result.BoundaryEvidence.BoundaryRedacted {
			t.Fatalf("secret scenario lacked boundary redaction evidence: %#v", result.BoundaryEvidence)
		}
	}
}

func TestReliabilityMetricsSafetyRates(t *testing.T) {
	metrics := ComputeReliabilityMetrics([]ReliabilityObservation{
		{Accepted: true, AcceptancePassed: true, EvidenceComplete: true},
		{Accepted: true, AcceptancePassed: false, EvidenceComplete: false, UnsafeReplay: true},
		{RepairAttempted: true, RepairAccepted: true},
		{RepairAttempted: true},
	})
	if metrics.FalseCompletionRate != 0.25 || metrics.EvidenceCoverage != 0.5 || metrics.RepairConvergence != 0.5 || metrics.UnsafeReplayRate != 0.25 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestReliabilityMetricsUseExplicitRepairAndReplayFacts(t *testing.T) {
	metrics := ComputeReliabilityMetrics([]ReliabilityObservation{{
		RepairAttempted: true, RepairAttempts: 2, RepairSuccesses: 2,
		ReplayAttempts: 4, UnsafeReplay: true, UnsafeReplayCount: 1,
	}})
	if metrics.RepairConvergence != 1 || metrics.UnsafeReplayRate != 0.25 {
		t.Fatalf("explicit metrics = %#v", metrics)
	}
}

func TestReliabilityRolloutRequiresAcceptanceForStrictModes(t *testing.T) {
	if got, err := ResolveReliabilityRollout("", false); err != nil || got != RolloutShadow {
		t.Fatalf("default without acceptance = %q, %v", got, err)
	}
	if got, err := ResolveReliabilityRollout(RolloutStrictOptIn, false); err == nil || got != RolloutShadow {
		t.Fatalf("strict without acceptance = %q, %v", got, err)
	}
	if got, err := ResolveReliabilityRollout(RolloutStrictDefault, true); err != nil || got != RolloutStrictDefault {
		t.Fatalf("strict with acceptance = %q, %v", got, err)
	}
}

func TestSaveReliabilityEvalReportIsAtomicAndDurable(t *testing.T) {
	workspace := t.TempDir()
	report := RunReliabilityEvalSuite(nil)
	if err := SaveReliabilityEvalReport(workspace, report); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, logsDir, "reliability_eval.json")
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		t.Fatalf("saved eval report: err=%v bytes=%d", err, len(b))
	}
}

func TestPersistReliabilityEvaluationUsesProductionObservation(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
		Reliability: agent.ReliabilityConfig{Rollout: string(RolloutShadow)},
	}}}
	result := &RunResult{
		Outcome:          RunOutcomeCompleted,
		GoalSatisfied:    true,
		Acceptance:       &AcceptanceResult{State: AcceptancePassed, Passed: true},
		EvidenceManifest: &EvidenceManifest{Status: "accepted", ManifestHash: "manifest-real"},
	}
	report, err := c.PersistReliabilityEvaluation(result)
	if err != nil {
		t.Fatal(err)
	}
	if report.ProductionObservation == nil || !report.ProductionObservation.Accepted {
		t.Fatalf("production observation = %#v", report.ProductionObservation)
	}
	if report.Metrics.FalseCompletionRate != 0 || report.Metrics.EvidenceCoverage != 1 {
		t.Fatalf("production metrics = %#v", report.Metrics)
	}
	data, err := os.ReadFile(filepath.Join(workspace, logsDir, "reliability_eval.json"))
	if err != nil || len(data) == 0 {
		t.Fatalf("production report: err=%v bytes=%d", err, len(data))
	}
}

func TestPersistReliabilityEvaluationAggregatesHistoricalRuns(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
		Reliability: agent.ReliabilityConfig{Rollout: string(RolloutShadow)},
	}}}
	good := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Acceptance:       &AcceptanceResult{State: AcceptancePassed, Passed: true},
		EvidenceManifest: &EvidenceManifest{Status: "accepted", ManifestHash: "good"}}
	if _, err := c.PersistReliabilityEvaluation(good); err != nil {
		t.Fatal(err)
	}
	bad := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Acceptance:       &AcceptanceResult{State: AcceptanceFailed, Passed: false},
		EvidenceManifest: &EvidenceManifest{Status: "failed", ManifestHash: "bad"}}
	report, err := c.PersistReliabilityEvaluation(bad)
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.ProductionRuns != 2 || len(report.ProductionObservations) != 2 {
		t.Fatalf("historical production observations = %#v metrics=%#v", report.ProductionObservations, report.Metrics)
	}
	if report.Metrics.FalseCompletionRate != 0.5 || report.Metrics.EvidenceCoverage != 0.5 {
		t.Fatalf("historical rates = %#v", report.Metrics)
	}
}

func TestReliabilityRolloutConfigurationAndStrictEnforcement(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
		Reliability: agent.ReliabilityConfig{Rollout: string(RolloutStrictOptIn)},
	}}}
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Acceptance:       &AcceptanceResult{State: AcceptancePassed, Passed: true},
		EvidenceManifest: &EvidenceManifest{Status: "accepted", ManifestHash: "hash"},
		Metrics:          RunMetrics{UnsafeReplaysDetected: 1, ReplayAttempts: 1}}
	report, err := c.PersistReliabilityEvaluation(result)
	if report.Rollout != RolloutStrictOptIn {
		t.Fatalf("rollout = %q, want strict-opt-in", report.Rollout)
	}
	if _, ok := err.(*ReliabilityRolloutViolation); !ok {
		t.Fatalf("strict rollout error = %v, want ReliabilityRolloutViolation", err)
	}
	if result.Outcome != RunOutcomeBlocked || result.GoalSatisfied {
		t.Fatalf("strict result = %#v, want blocked downgrade", result)
	}
	data, readErr := os.ReadFile(filepath.Join(workspace, logsDir, "reliability_eval.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var saved ReliabilityEvalReport
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ProductionObservation == nil || saved.ProductionObservation.Accepted {
		t.Fatalf("strict rejected observation persisted as accepted: %#v", saved.ProductionObservation)
	}
	if strings.Contains(string(data), `"accepted":true`) {
		t.Fatalf("strict report contains accepted observation: %s", data)
	}
}

func TestStrictRolloutRequiresConfiguredAcceptanceContract(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
		Reliability: agent.ReliabilityConfig{Rollout: string(RolloutStrictOptIn)},
	}}}
	_, err := c.PersistReliabilityEvaluation(&RunResult{
		Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Acceptance: &AcceptanceResult{State: AcceptanceNotConfigured},
	})
	if err == nil || !strings.Contains(err.Error(), "requires an acceptance contract") {
		t.Fatalf("strict rollout without contract error = %v", err)
	}
}

func TestTeamYAMLRolloutFlowsIntoStrictRuntime(t *testing.T) {
	teamDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: yaml-rollout\nacceptance: 'true'\nreliability:\n  rollout: strict-opt-in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(teamDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Reliability.Rollout != string(RolloutStrictOptIn) || cfg.Acceptance == "" {
		t.Fatalf("parsed runtime config = %#v", cfg)
	}
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir(), Config: cfg}}
	result := &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true,
		Acceptance:       &AcceptanceResult{State: AcceptancePassed, Passed: true},
		EvidenceManifest: &EvidenceManifest{Status: "accepted", ManifestHash: "yaml-runtime"},
		Metrics:          RunMetrics{UnsafeReplaysDetected: 1, ReplayAttempts: 1}}
	if _, err := c.PersistReliabilityEvaluation(result); err == nil {
		t.Fatal("YAML-configured strict rollout did not enforce at runtime")
	}
	if result.Outcome != RunOutcomeBlocked {
		t.Fatalf("runtime result = %#v, want blocked", result)
	}
}

func TestCorruptHistoricalReliabilityReportFailsClosed(t *testing.T) {
	workspace := t.TempDir()
	logs := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "reliability_eval.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}}
	_, err := c.PersistReliabilityEvaluation(&RunResult{Outcome: RunOutcomePartial})
	if err == nil || !strings.Contains(err.Error(), "load historical reliability report") {
		t.Fatalf("corrupt history error = %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "{not-json" {
		t.Fatalf("corrupt history was overwritten: err=%v data=%q", readErr, data)
	}
}

func TestTerminalCorruptHistoricalReportDowngradesRunAndPublishesBlocked(t *testing.T) {
	workspace := t.TempDir()
	logs := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "reliability_eval.json"), []byte("{malformed"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, sessionData: NewSession()}
	end := c.beginExecutionRun()
	c.SetLastRunResult(&RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true})
	end()
	if result := c.LastRunResult(); result == nil || result.Outcome != RunOutcomeBlocked || result.GoalSatisfied {
		t.Fatalf("terminal result = %#v, want blocked after malformed history", result)
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var runFinished bool
	for _, event := range events {
		if event.Type != "run_finished" {
			continue
		}
		var payload struct {
			Outcome       RunOutcome `json:"outcome"`
			GoalSatisfied bool       `json:"goal_satisfied"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		runFinished = true
		if payload.Outcome != RunOutcomeBlocked || payload.GoalSatisfied {
			t.Fatalf("run_finished payload = %#v, want blocked", payload)
		}
	}
	if !runFinished {
		t.Fatal("run_finished event missing")
	}
}
