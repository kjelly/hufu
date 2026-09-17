package team

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestNormalizeDecisionAuthoringCanonicalAliasMatchesLegacyPreset(t *testing.T) {
	canonicalDir := writeDecisionAuthoringManifest(t, `decision:
  profile: standard
`)
	legacyDir := writeDecisionAuthoringManifest(t, `decision:
  default-profile: standard
  profiles:
    standard:
      preset: builtin/standard@v1
`)

	canonical, canonicalMeta, err := parseTeamYMLWithAuthoring(canonicalDir, nil)
	if err != nil {
		t.Fatalf("canonical parse: %v", err)
	}
	legacy, legacyMeta, err := parseTeamYMLWithAuthoring(legacyDir, nil)
	if err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	left, _, ok, err := agent.ResolveDecisionProfileSpec(canonical.Decision, "standard", agent.BuiltInDecisionProfileCatalog())
	if err != nil || !ok {
		t.Fatalf("resolve canonical profile: ok=%v err=%v", ok, err)
	}
	right, _, ok, err := agent.ResolveDecisionProfileSpec(legacy.Decision, "standard", agent.BuiltInDecisionProfileCatalog())
	if err != nil || !ok {
		t.Fatalf("resolve legacy profile: ok=%v err=%v", ok, err)
	}
	if !agent.EqualMaterializedDecisionPolicies(left, right) {
		t.Fatalf("canonical and legacy policies differ")
	}
	leftDigest, err := agent.DecisionPolicyDigest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := agent.DecisionPolicyDigest(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("canonical/legacy digest = %q/%q", leftDigest, rightDigest)
	}
	leftPlan, err := CompileDecisionExecutionPlan(left)
	if err != nil {
		t.Fatal(err)
	}
	rightPlan, err := CompileDecisionExecutionPlan(right)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(leftPlan, rightPlan) {
		t.Fatalf("canonical/legacy plans differ: %#v != %#v", leftPlan, rightPlan)
	}
	if canonicalMeta.ProfileSource != "decision.profile" || legacyMeta.ProfileSource != "decision.default-profile" || !legacyMeta.UsedLegacyDefault {
		t.Fatalf("authoring provenance = %#v / %#v", canonicalMeta, legacyMeta)
	}
	if canonicalMeta.ResolvedProfileRef != agent.DecisionProfileBuiltinStandardV1 || legacyMeta.ResolvedProfileRef != agent.DecisionProfileBuiltinStandardV1 {
		t.Fatalf("resolved refs = %q / %q", canonicalMeta.ResolvedProfileRef, legacyMeta.ResolvedProfileRef)
	}
}

func TestNormalizeDecisionAuthoringLocalProfileWinsAlias(t *testing.T) {
	dir := writeDecisionAuthoringManifest(t, `decision:
  profile: standard
  profiles:
    standard:
      independent-judgments: 2
`)
	cfg, _, err := parseTeamYMLWithAuthoring(dir, nil)
	if err != nil {
		t.Fatalf("parse local profile: %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || policy.IndependentJudgments != 2 {
		t.Fatalf("local standard policy = %#v, ok=%v", policy, ok)
	}
}

func TestNormalizeDecisionAuthoringAcceptsExactBuiltinReference(t *testing.T) {
	dir := writeDecisionAuthoringManifest(t, `decision:
  profile: builtin/standard@v1
`)
	cfg, metadata, err := parseTeamYMLWithAuthoring(dir, nil)
	if err != nil {
		t.Fatalf("parse exact builtin: %v", err)
	}
	if cfg.Decision.DefaultProfile != agent.DecisionProfileBuiltinStandardV1 || metadata.ResolvedProfileRef != agent.DecisionProfileBuiltinStandardV1 {
		t.Fatalf("exact builtin = %#v / %#v", cfg.Decision, metadata)
	}
}

func TestNormalizeDecisionAuthoringRejectsBlankUnknownAndConflicts(t *testing.T) {
	tests := map[string]string{
		"blank": `decision:
  profile: "   "
`,
		"unknown": `decision:
  profile: nonexistent
`,
		"profile conflict": `decision:
  profile: standard
  default-profile: standard
`,
		"routing conflict": `decision:
  routing:
    hints: []
  routing-hints:
    - when-goal-contains: storage
      preferred-capabilities: [architecture]
`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := writeDecisionAuthoringManifest(t, content)
			_, _, err := parseTeamYMLWithAuthoring(dir, nil)
			if err == nil {
				t.Fatal("invalid authoring was accepted")
			}
			if name == "profile conflict" && !strings.Contains(err.Error(), "decision_authoring_conflict") {
				t.Fatalf("error = %v, want decision_authoring_conflict", err)
			}
			if name == "routing conflict" && !strings.Contains(err.Error(), "decision.routing.hints conflicts") {
				t.Fatalf("error = %v, want routing conflict", err)
			}
		})
	}
}

func writeDecisionAuthoringManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
