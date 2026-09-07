package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Parse round-trip and validation tests for the capability-routing config
// surfaces (outside-view.role, judge-role, challenge-role, pin, diversity
// extensions, adaptive-capabilities, routing-policy scoring weights,
// routing-hints). Split out of decision_types_test.go to keep both files
// under the project's 800-line-per-file guideline
// (TestDecisionRuntimeFilesRespectTheSizeLimit).

// outside-view.role (plan.md Stage 8 follow-up) must round-trip through the
// strict team.yaml decoder like every other discipline field.
func TestParseTeamYMLOutsideViewRole(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      outside-view:
        required: true
        reference-evidence: true
        role:
          required-capabilities:
            - evidence-research
          preferred-capabilities:
            - domain:storage
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.OutsideView.Role == nil {
		t.Fatalf("policy.OutsideView.Role = %#v, want it set", policy.OutsideView)
	}
	if len(policy.OutsideView.Role.RequiredCapabilities) != 1 || policy.OutsideView.Role.RequiredCapabilities[0] != "evidence-research" {
		t.Fatalf("RequiredCapabilities = %#v", policy.OutsideView.Role.RequiredCapabilities)
	}
}

// judge-role (spec2.md PR-3) must round-trip through the strict team.yaml
// decoder like every other discipline field.
func TestParseTeamYMLJudgeRole(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      judge-role:
        required-capabilities:
          - decision-analysis
        preferred-capabilities:
          - domain:storage
        min-distinct-agents: 2
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.JudgeRole == nil {
		t.Fatalf("policy.JudgeRole = %#v, want it set", policy.JudgeRole)
	}
	if policy.JudgeRole.MinDistinctAgents != 2 {
		t.Fatalf("MinDistinctAgents = %d, want 2", policy.JudgeRole.MinDistinctAgents)
	}
}

// DiversityPolicy extensions (spec.md v2 §13) round-trip through YAML.
func TestParseTeamYMLJudgeRoleDiversityExtensions(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      judge-role:
        required-capabilities:
          - decision-analysis
        min-distinct-models: 2
        min-distinct-providers: 2
        prefer-distinct-models: true
        prefer-distinct-providers: true
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.JudgeRole == nil {
		t.Fatalf("policy.JudgeRole = %#v, want it set", policy.JudgeRole)
	}
	role := policy.JudgeRole
	if role.MinDistinctModels != 2 || role.MinDistinctProviders != 2 || !role.PreferDistinctModels || !role.PreferDistinctProviders {
		t.Fatalf("JudgeRole diversity fields = %#v, want all set", role)
	}
}

// allow-repeated-agent-definition conflicting with min-distinct-agents > 1
// must fail team load.
func TestParseTeamYMLRejectsAllowRepeatedAgentDefinitionConflict(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      judge-role:
        required-capabilities:
          - decision-analysis
        min-distinct-agents: 2
        allow-repeated-agent-definition: true
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil {
		t.Fatal("want an error for allow-repeated-agent-definition conflicting with min-distinct-agents > 1")
	}
}

// A pin (spec.md v2 §34) round-trips through YAML and requires the
// profile's discipline.routing.allow-pinned-binding gate.
func TestParseTeamYMLJudgeRolePin(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      discipline:
        routing:
          allow-pinned-binding: true
      judge-role:
        required-capabilities:
          - decision-analysis
        pin:
          agent: security-reviewer
          reason: required by security acceptance policy
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.JudgeRole == nil || policy.JudgeRole.Pin == nil {
		t.Fatalf("policy.JudgeRole.Pin = %#v, want it set", policy.JudgeRole)
	}
	if policy.JudgeRole.Pin.Agent != "security-reviewer" || policy.JudgeRole.Pin.Reason != "required by security acceptance policy" {
		t.Fatalf("Pin = %#v, want agent security-reviewer with the configured reason", policy.JudgeRole.Pin)
	}
}

// A pin without the profile's allow-pinned-binding gate must fail team
// load, not fail at dispatch time.
func TestParseTeamYMLRejectsPinWithoutProfileGate(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      judge-role:
        required-capabilities:
          - decision-analysis
        pin:
          agent: security-reviewer
          reason: required by security acceptance policy
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil {
		t.Fatal("want an error for a pin without discipline.routing.allow-pinned-binding")
	}
}

// A judge-role that could never be satisfied by the configured judge count
// must fail team load, not fail at dispatch time.
func TestParseTeamYMLRejectsImpossibleJudgeRole(t *testing.T) {
	dir := t.TempDir()
	content := `decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 2
      judge-role:
        required-capabilities:
          - decision-analysis
        min-distinct-agents: 3
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "judge-role") {
		t.Fatalf("parseTeamYML error = %v, want judge-role validation failure", err)
	}
}

// challenge-role (spec2.md PR-4) must round-trip through the strict
// team.yaml decoder like every other discipline field.
func TestParseTeamYMLChallengeRole(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      challenge:
        enabled: true
        count: 2
      challenge-role:
        required-capabilities:
          - adversarial-analysis
        min-distinct-agents: 2
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.ChallengeRole == nil {
		t.Fatalf("policy.ChallengeRole = %#v, want it set", policy.ChallengeRole)
	}
	if policy.ChallengeRole.MinDistinctAgents != 2 {
		t.Fatalf("MinDistinctAgents = %d, want 2", policy.ChallengeRole.MinDistinctAgents)
	}
}

// adaptive-capabilities (spec.md v2 §39-40) round-trips through YAML and
// validates against the profile's own configured criteria.
func TestParseTeamYMLChallengeRoleAdaptiveCapabilities(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      criteria:
        - id: operability
          weight: 1
      challenge:
        enabled: true
        count: 1
      challenge-role:
        required-capabilities:
          - adversarial-analysis
        adaptive-capabilities:
          operability:
            - operations
            - reliability
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.ChallengeRole == nil {
		t.Fatalf("policy.ChallengeRole = %#v, want it set", policy.ChallengeRole)
	}
	got := policy.ChallengeRole.AdaptiveCapabilities["operability"]
	if len(got) != 2 || got[0] != "operations" || got[1] != "reliability" {
		t.Fatalf("AdaptiveCapabilities[operability] = %#v, want [operations reliability]", got)
	}
}

// A challenge-role.adaptive-capabilities key naming no configured criterion
// must fail team load (a typo would otherwise silently never fire).
func TestParseTeamYMLRejectsUnknownAdaptiveCapabilitiesCriterion(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      challenge:
        enabled: true
        count: 1
      challenge-role:
        required-capabilities:
          - adversarial-analysis
        adaptive-capabilities:
          data-integrity:
            - database
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil {
		t.Fatal("want an error for adaptive-capabilities referencing an unconfigured criterion")
	}
}

// A challenge-role that could never be satisfied by the configured
// challenge count must fail team load, not fail at dispatch time.
func TestParseTeamYMLRejectsImpossibleChallengeRole(t *testing.T) {
	dir := t.TempDir()
	content := `decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      challenge:
        enabled: true
        count: 1
      challenge-role:
        required-capabilities:
          - adversarial-analysis
        min-distinct-agents: 2
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "challenge-role") {
		t.Fatalf("parseTeamYML error = %v, want challenge-role validation failure", err)
	}
}

// A role declared with no required capabilities must fail team load.
func TestParseTeamYMLRejectsEmptyOutsideViewRole(t *testing.T) {
	dir := t.TempDir()
	content := `decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      outside-view:
        required: true
        reference-evidence: true
        role: {}
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "outside-view.role") {
		t.Fatalf("parseTeamYML error = %v, want outside-view.role validation failure", err)
	}
}

// routing-policy.scoring.weights (spec.md v2 §12) must round-trip through
// the strict team.yaml decoder.
func TestParseTeamYMLRoutingPolicyScoringWeights(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
routing-policy:
  scoring:
    weights:
      required-match: 2
      preferred-match: 1
      cost: 0.5
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	weights := cfg.RoutingPolicy.Scoring.Weights
	if weights.RequiredMatch != 2 || weights.PreferredMatch != 1 || weights.Cost != 0.5 {
		t.Fatalf("weights = %#v", weights)
	}
}

// A negative weight must fail team load, not silently invert ranking at
// dispatch time.
func TestParseTeamYMLRejectsNegativeScoringWeight(t *testing.T) {
	dir := t.TempDir()
	content := `routing-policy:
  scoring:
    weights:
      cost: -0.1
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "routing-policy.scoring.weights") {
		t.Fatalf("parseTeamYML error = %v, want routing-policy.scoring.weights validation failure", err)
	}
}

// decision.routing-hints (spec.md v2 §16) must round-trip through the
// strict team.yaml decoder.
func TestParseTeamYMLRoutingHints(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  routing-hints:
    - when-goal-contains: kubernetes
      preferred-capabilities:
        - kubernetes
        - platform-engineering
  profiles:
    standard:
      independent-judgments: 3
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	if len(cfg.Decision.RoutingHints) != 1 || cfg.Decision.RoutingHints[0].WhenGoalContains != "kubernetes" {
		t.Fatalf("RoutingHints = %#v", cfg.Decision.RoutingHints)
	}
}

// A malformed routing hint must fail team load.
func TestParseTeamYMLRejectsInvalidRoutingHint(t *testing.T) {
	dir := t.TempDir()
	content := `decision:
  default-profile: standard
  routing-hints:
    - when-goal-contains: kubernetes
  profiles:
    standard:
      independent-judgments: 3
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "routing-hints") {
		t.Fatalf("parseTeamYML error = %v, want routing-hints validation failure", err)
	}
}
