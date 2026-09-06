package team

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The self-contained V1 decision fixture (plan Stage 7.1).
//
// It is written to disk and loaded through LoadTeam rather than constructed in
// Go, because "the authored configuration produces this behavior" is exactly
// what Stage 7 has to prove. A fixture assembled from structs would skip
// parsing, validation, and defaulting — the layer an operator actually
// touches.

const (
	fixtureProfileLight      = "light"
	fixtureProfileStandard   = "standard"
	fixtureProfileHighStakes = "high-stakes"
	// fixtureProfileCoordinator finalizes through a coordinator rather than
	// the aggregate, which is the only way an override can occur.
	fixtureProfileCoordinator = "coordinator-final"
)

// decisionFixtureTeamYAML declares all three rigor profiles over one team.
// The profiles differ only in rigor, so a test can move a task between them
// and attribute every behavior change to the profile.
func decisionFixtureTeamYAML(judgeModel string) string {
	return fmt.Sprintf(`name: decision-v1
max-rounds: 4
model: %[1]s
judge-model: %[1]s
sidecar-model: %[1]s
decision:
  default-profile: off
  request-contract:
    enabled: true
    objective: keep the bridge reachable while changing it
    success-criteria:
      - id: reachable
        statement: the bridge answers after the change
    constraints:
      - id: no-downtime
        statement: no interruption longer than five seconds
    assumptions:
      - id: service-accepts
        statement: the target service accepts the change
        critical: true
  profiles:
    light:
      independent-judgments: 1
      context-isolation: strict
      score-scale: 0-10
      aggregation:
        method: mean-score
      max-rounds: 1
      finalization:
        mode: aggregate
      discipline:
        stop:
          checkpoint-every: 2
    standard:
      independent-judgments: 3
      context-isolation: strict
      score-scale: 0-10
      criteria:
        - id: impact
          statement: how much this moves the objective
          weight: 1
          direction: higher-is-better
      aggregation:
        method: mean-score
      challenge:
        enabled: true
        count: 1
        trigger:
          dispersion-above: 0.5
      revision:
        enabled: true
      max-rounds: 2
      finalization:
        mode: aggregate
      discipline:
        alternatives:
          require-no-action-option: true
          min-options: 2
        stop:
          checkpoint-every: 2
          require-kill-criteria: true
          kill-criteria:
            - id: tool-calls
              kind: tool_calls
              threshold: 20
            - id: assumption
              kind: assumption_invalid
        commit:
          require-verification: true
          require-reconcile: true
        replan:
          on-critical-assumption-contradicted: replan
    coordinator-final:
      # Identical to standard except that a finalizer, not the aggregate,
      # picks the option. It exists so the override path has a profile.
      independent-judgments: 3
      context-isolation: strict
      score-scale: 0-10
      aggregation:
        method: mean-score
      max-rounds: 1
      finalization:
        mode: coordinator
      discipline:
        stop:
          checkpoint-every: 2
    high-stakes:
      independent-judgments: 3
      context-isolation: strict
      score-scale: 0-10
      criteria:
        - id: impact
          statement: how much this moves the objective
          weight: 1
          direction: higher-is-better
      outside-view:
        required: true
      aggregation:
        method: mean-score
      challenge:
        enabled: true
        count: 1
      revision:
        enabled: true
      premortem:
        enabled: true
        required-before-commit: true
      max-rounds: 2
      finalization:
        mode: aggregate
      discipline:
        alternatives:
          require-no-action-option: true
          require-information-option: true
          min-options: 3
        stop:
          checkpoint-every: 1
          require-kill-criteria: true
          kill-criteria:
            - id: tool-calls
              kind: tool_calls
              threshold: 10
            - id: assumption
              kind: assumption_invalid
        commit:
          require-verification: true
          require-evidence: true
          require-reconcile: true
          require-rollback: true
          require-observability: true
        replan:
          on-critical-assumption-contradicted: replan
`, judgeModel)
}

// decisionFixtureAgentMarkdown declares the worker whose tool recovery the
// high-stakes commit gate reads.
func decisionFixtureAgentMarkdown(model string) string {
	return fmt.Sprintf(`---
name: deployer
role: worker
model: %s
tools: bash,view,delete-bridge
side_effect: infra_mutation
recovery: reconcile
reconcile-tool: probe-bridge
tool-recovery:
  bash:
    compensate-tool: delete-bridge
    reconcile-tool: probe-bridge
---

You change network bridges.
`, model)
}

// writeDecisionFixture materializes the fixture team and returns its
// directory.
func writeDecisionFixture(t *testing.T, judgeModel string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"),
		[]byte(decisionFixtureTeamYAML(judgeModel)), 0o644); err != nil {
		t.Fatalf("write team.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deployer.md"),
		[]byte(decisionFixtureAgentMarkdown(judgeModel)), 0o644); err != nil {
		t.Fatalf("write deployer.md: %v", err)
	}
	return dir
}

// The fixture loads through the real team loader, and every rigor profile it
// declares is resolvable. A fixture that only parsed in a struct literal would
// prove nothing about authored configuration.
func TestDecisionV1FixtureLoadsAllProfiles(t *testing.T) {
	dir := writeDecisionFixture(t, "fixture-judge-model")
	session, err := LoadTeam(dir, nil, nil, NewProviderRegistry())
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	config := session.Config.Decision
	for _, profile := range []string{fixtureProfileLight, fixtureProfileStandard, fixtureProfileCoordinator, fixtureProfileHighStakes} {
		policy, ok := DecisionPolicyFor(config, profile)
		if !ok {
			t.Fatalf("profile %q did not resolve", profile)
		}
		resolution, err := ResolveDecisionProfile(config, "", TaskDef{DecisionProfile: profile})
		if err != nil {
			t.Fatalf("resolve %q: %v", profile, err)
		}
		if !resolution.Enabled() || resolution.Profile != profile {
			t.Fatalf("resolution for %q = %#v", profile, resolution)
		}
		if policy.IndependentJudgments < 1 {
			t.Fatalf("profile %q has no judges", profile)
		}
	}
	if !config.RequestContract.Enabled {
		t.Fatal("fixture request contract is not enabled")
	}

	// The declared rigor must actually increase across the three profiles.
	light, _ := DecisionPolicyFor(config, fixtureProfileLight)
	standard, _ := DecisionPolicyFor(config, fixtureProfileStandard)
	high, _ := DecisionPolicyFor(config, fixtureProfileHighStakes)
	if !(light.IndependentJudgments < standard.IndependentJudgments ||
		light.IndependentJudgments == standard.IndependentJudgments) {
		t.Fatalf("light has more judges than standard")
	}
	if standard.Challenge.Trigger == nil {
		t.Fatal("standard profile lost its dispersion trigger")
	}
	if high.Challenge.Trigger != nil {
		t.Fatal("high-stakes must challenge unconditionally, not on a threshold")
	}
	if !high.OutsideView.Required || !high.Premortem.RequiredBeforeCommit {
		t.Fatalf("high-stakes lost its required gates: %#v", high)
	}
	if !high.Discipline.Commit.RequireRollback || standard.Discipline.Commit.RequireRollback {
		t.Fatal("require-rollback must separate high-stakes from standard")
	}
}

// The fixture agent's per-tool recovery contract survives the loader, so the
// commit gate reads what the operator authored.
func TestDecisionV1FixtureAgentDeclaresToolRecovery(t *testing.T) {
	dir := writeDecisionFixture(t, "fixture-judge-model")
	session, err := LoadTeam(dir, nil, nil, NewProviderRegistry())
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	def := session.Agents["deployer"]
	if def == nil {
		t.Fatalf("fixture agent did not load: %v", session.Agents)
	}
	spec := resolveToolRecovery(def, TaskDef{Agent: "deployer"}, "bash")
	if spec.CompensateTool != "delete-bridge" || spec.ReconcileTool != "probe-bridge" {
		t.Fatalf("resolved tool recovery = %#v", spec)
	}
	if !agentCanInvoke(def, spec.CompensateTool) {
		t.Fatal("the fixture declares a compensate tool its agent cannot invoke")
	}
	// The high-stakes commit gate is satisfiable by this contract and blocked
	// without it: the fixture proves both directions.
	high, _ := DecisionPolicyFor(session.Config.Decision, fixtureProfileHighStakes)
	task := mutatingTask()
	task.Agent = "deployer"
	if decision := EvaluateCommitGate(CommitGateInput{
		Task: task, Policy: high.Discipline.Commit, ToolRecovery: spec,
	}); !decision.Allowed {
		t.Fatalf("fixture contract did not satisfy the high-stakes gate: %#v", decision)
	}
	if decision := EvaluateCommitGate(CommitGateInput{
		Task: task, Policy: high.Discipline.Commit,
	}); decision.Allowed {
		t.Fatal("the high-stakes gate admitted a mutation with no compensating operation")
	}
}
