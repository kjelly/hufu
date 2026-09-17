package team

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
	if !canonicalMeta.UsedErgonomicAlias || legacyMeta.UsedErgonomicAlias {
		t.Fatalf("ergonomic alias provenance = %#v / %#v", canonicalMeta, legacyMeta)
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

func TestNormalizeDecisionAuthoringSeparatesPrimaryAndAuxiliaryProfiles(t *testing.T) {
	dir := writeDecisionAuthoringManifest(t, `decision:
  profile: standard
  primary-profile: primary-standard
  profiles:
    primary-standard:
      preset: builtin/standard@v2
  routing:
    constraints:
      required-capabilities:
        judge: [security-review]
      diversity:
        min-distinct-providers: 1
      fallback: forbid
      candidate-limit: 8
`)
	cfg, metadata, err := parseTeamYMLWithAuthoring(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Decision.DefaultProfile != "standard" || metadata.ResolvedProfileRef != agent.DecisionProfileBuiltinStandardV1 {
		t.Fatalf("auxiliary profile changed: %#v / %#v", cfg.Decision, metadata)
	}
	ref, origin, err := agent.ResolvePrimaryDecisionProfileRef(cfg.Decision, cfg.Decision.PrimaryProfile)
	if err != nil {
		t.Fatal(err)
	}
	if ref != agent.DecisionProfileBuiltinStandardV2 || origin != agent.DecisionProfileOriginTeamInline {
		t.Fatalf("primary profile = %q/%q", ref, origin)
	}
	constraints := cfg.Decision.RoleConstraints
	if constraints.Fallback != "forbid" || constraints.CandidateLimit != 8 || constraints.Diversity.MinDistinctProviders != 1 || !slices.Equal(constraints.RequiredCapabilities.Judge, []string{"security-review"}) {
		t.Fatalf("role constraints = %#v", constraints)
	}
}

func TestNormalizeDecisionAuthoringRejectsInvalidPrimaryConfiguration(t *testing.T) {
	for name, content := range map[string]string{
		"blank":              "decision:\n  primary-profile: '   '\n",
		"v1 local":           "decision:\n  primary-profile: old\n  profiles:\n    old:\n      preset: builtin/standard@v1\n",
		"null constraints":   "decision:\n  routing:\n    constraints: null\n",
		"unknown constraint": "decision:\n  routing:\n    constraints:\n      invented: true\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeDecisionAuthoringManifest(t, content)
			if _, _, err := parseTeamYMLWithAuthoring(dir, nil); err == nil {
				t.Fatal("invalid primary configuration was accepted")
			}
		})
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

func TestNormalizeDecisionAuthoringTopLevelRequestMaterializesContract(t *testing.T) {
	dir := writeDecisionAuthoringManifest(t, `request:
  objective: ship the migration
  success-criteria:
    - id: tests
      statement: all tests pass
  constraints:
    - id: api
      statement: preserve the public API
  assumptions:
    - id: target
      statement: target accepts the migration
      critical: true
`)
	cfg, metadata, err := parseTeamYMLWithAuthoring(dir, nil)
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	want := agent.RequestContractConfig{
		Enabled: true, Objective: "ship the migration",
		SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "tests", Statement: "all tests pass"}},
		Constraints:     []agent.RequestConstraint{{ID: "api", Statement: "preserve the public API"}},
		Assumptions:     []agent.RequestContractAssumption{{ID: "target", Statement: "target accepts the migration", Critical: true}},
	}
	if !reflect.DeepEqual(cfg.RequestContract, want) {
		t.Fatalf("request contract = %#v, want %#v", cfg.RequestContract, want)
	}
	if metadata.RequestSource != "request" || metadata.UsedLegacyContract || len(metadata.Deprecations) != 0 {
		t.Fatalf("request provenance = %#v", metadata)
	}
}

func TestNormalizeDecisionAuthoringRequestPresenceAndCompatibility(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "absent", content: "name: no-request\n", wantErr: ""},
		{name: "null", content: "request: null\n", wantErr: "non-null mapping"},
		{name: "empty", content: "request: {}\n", wantErr: "requires an objective"},
		{name: "conflict", content: "request:\n  objective: new\n  success-criteria:\n    - id: ok\n      statement: done\ndecision:\n  request-contract:\n    enabled: true\n    objective: old\n    success-criteria:\n      - id: ok\n        statement: done\n", wantErr: "request_authoring_conflict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeDecisionAuthoringManifest(t, tc.content)
			cfg, metadata, err := parseTeamYMLWithAuthoring(dir, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RequestContract.Enabled || metadata.RequestSource != "" {
				t.Fatalf("absent request materialized unexpectedly: cfg=%#v metadata=%#v", cfg.RequestContract, metadata)
			}
		})
	}
}

func TestNormalizeDecisionAuthoringLegacyRequestMatchesCanonicalIdentity(t *testing.T) {
	canonicalDir := writeDecisionAuthoringManifest(t, `request:
  objective: ship safely
  success-criteria:
    - id: tests
      statement: tests pass
`)
	legacyDir := writeDecisionAuthoringManifest(t, `decision:
  request-contract:
    enabled: true
    objective: ship safely
    success-criteria:
      - id: tests
        statement: tests pass
`)
	canonical, canonicalMeta, err := parseTeamYMLWithAuthoring(canonicalDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, legacyMeta, err := parseTeamYMLWithAuthoring(legacyDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(canonical.RequestContract, legacy.RequestContract) {
		t.Fatalf("canonical/legacy request contracts differ: %#v != %#v", canonical.RequestContract, legacy.RequestContract)
	}
	first, _, err := BuildRequestContract("same request", "same question", canonical.RequestContract, 3, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := BuildRequestContract("same request", "same question", legacy.RequestContract, 3, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.MaterialHash != second.MaterialHash {
		t.Fatalf("canonical/legacy contract identity differs: %#v != %#v", first, second)
	}
	if canonicalMeta.RequestSource != "request" || legacyMeta.RequestSource != "decision.request-contract" || !legacyMeta.UsedLegacyContract {
		t.Fatalf("request provenance = %#v / %#v", canonicalMeta, legacyMeta)
	}
}

func TestCoordinatorRequestContractConfigUsesTeamConfigOwnership(t *testing.T) {
	want := dispatchRequestContract()
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{RequestContract: want}}}
	if got := c.requestContractConfig(); !reflect.DeepEqual(got, want) {
		t.Fatalf("requestContractConfig = %#v, want %#v", got, want)
	}
	if got := (&Coordinator{}).requestContractConfig(); got.Enabled {
		t.Fatal("nil session returned an enabled request contract")
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
