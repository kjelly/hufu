package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const migrateFixtureTeamYAML = `name: migrate-fixture
description: Legacy team exercised by the migrator's round-trip tests
max-rounds: 6
timeout: 900
verify-timeout: 45
max-retries: 3
no-net: true
tools:
  allowed:
    - view
    - edit
    - grep
required-resources:
  - name: project-rules
    kind: project_rules
    path: PROJECT_RULES.txt
    inject-into:
      - developer
    required: true
advanced:
  verification:
    required: true
`

const migrateFixtureDeveloperMD = `---
name: developer
description: Implements production code changes under the migrate fixture's contract
role: worker
tools: view,edit,write,grep,glob,ls
---
Implement the requested change using only the tools available to this team.
`

const migrateFixtureProjectRules = `# Project rules

Fixture content; not read by these tests, only declared.
`

// writeMigrateFixtureTeam materializes migrateFixtureTeamYAML plus its
// supporting files under dir, so both LoadTeam and the migrator see a
// realistic multi-file team directory, not just a bare team.yaml.
func writeMigrateFixtureTeam(t *testing.T, dir string) {
	t.Helper()
	writeTeamManifest(t, dir, migrateFixtureTeamYAML)
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte(migrateFixtureDeveloperMD), 0o644); err != nil {
		t.Fatalf("write developer.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "PROJECT_RULES.txt"), []byte(migrateFixtureProjectRules), 0o644); err != nil {
		t.Fatalf("write PROJECT_RULES.txt: %v", err)
	}
}

// compatSnapshotJSON loads teamDir and renders its CompatSnapshot as JSON,
// for the semantic-equivalence comparisons the migrator tests below need.
func compatSnapshotJSON(t *testing.T, teamDir string) []byte {
	t.Helper()
	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(%s): %v", teamDir, err)
	}
	snapshot, err := BuildCompatSnapshot(session)
	if err != nil {
		t.Fatalf("BuildCompatSnapshot(%s): %v", teamDir, err)
	}
	out, err := snapshot.JSON()
	if err != nil {
		t.Fatalf("snapshot.JSON(%s): %v", teamDir, err)
	}
	return out
}

func TestTeamMigrateDryRunRoundTrips(t *testing.T) {
	dir := t.TempDir()
	writeMigrateFixtureTeam(t, dir)

	migrated, already, err := MigrateTeamManifestToV1Alpha1(dir)
	if err != nil {
		t.Fatalf("MigrateTeamManifestToV1Alpha1: %v", err)
	}
	if already {
		t.Fatal("already = true for a legacy source, want false")
	}

	// The migrated output must decode as a well-formed v1alpha1 manifest
	// (parseTeamYML round-trips through decodeTeamManifestYAML) and must
	// normalize to the exact same effective config the original legacy
	// source did (§11 PR-3 "round-trip"). The legacy source's own decode
	// already expanded its `advanced:` block via mergeAdvancedNamespace, so
	// nothing legacy-only should survive into the round-tripped side.
	originalCfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML(original): %v", err)
	}

	roundTripDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(roundTripDir, "team.yaml"), migrated, 0o644); err != nil {
		t.Fatalf("write round-tripped team.yaml: %v", err)
	}
	roundTrippedCfg, err := parseTeamYML(roundTripDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML(round-tripped): %v\n--- migrated ---\n%s", err, migrated)
	}
	version, err := DetectTeamSchemaVersion(roundTripDir, nil)
	if err != nil {
		t.Fatalf("DetectTeamSchemaVersion(round-tripped): %v", err)
	}
	if version != SchemaVersionV1Alpha1 {
		t.Errorf("round-tripped schema version = %q, want %q", version, SchemaVersionV1Alpha1)
	}

	originalSnapshot, err := BuildCompatSnapshot(&TeamSession{Config: originalCfg})
	if err != nil {
		t.Fatalf("BuildCompatSnapshot(original): %v", err)
	}
	roundTrippedSnapshot, err := BuildCompatSnapshot(&TeamSession{Config: roundTrippedCfg})
	if err != nil {
		t.Fatalf("BuildCompatSnapshot(round-tripped): %v", err)
	}
	originalJSON, err := originalSnapshot.JSON()
	if err != nil {
		t.Fatalf("original JSON: %v", err)
	}
	roundTrippedJSON, err := roundTrippedSnapshot.JSON()
	if err != nil {
		t.Fatalf("round-tripped JSON: %v", err)
	}
	if !jsonEqual(t, originalJSON, roundTrippedJSON) {
		t.Errorf("round-tripped decode does not match the original:\n--- original ---\n%s\n--- round-tripped ---\n%s", originalJSON, roundTrippedJSON)
	}

	// Migrating an already-v1alpha1 source is idempotent: already reports
	// true, and re-marshaling its own decode is byte-for-byte stable (§3
	// principle 5: migration pure/deterministic).
	migratedAgain, already2, err := MigrateTeamManifestToV1Alpha1(roundTripDir)
	if err != nil {
		t.Fatalf("MigrateTeamManifestToV1Alpha1(already v1alpha1): %v", err)
	}
	if !already2 {
		t.Error("already = false for a hufu.io/v1alpha1 source, want true")
	}
	if string(migrated) != string(migratedAgain) {
		t.Errorf("re-migrating an already-v1alpha1 manifest is not stable:\n--- first ---\n%s\n--- second ---\n%s", migrated, migratedAgain)
	}
}

// TestTeamMigrateExpandsAdvancedNamespaceAlias pins §7's migrator
// requirement directly: a legacy source using the `advanced:` alias
// namespace must never produce an `advanced:` block in the migrated
// v1alpha1 output (v1alpha1 has no such alias — §7), and the value must
// still be present under its canonical field name.
func TestTeamMigrateExpandsAdvancedNamespaceAlias(t *testing.T) {
	dir := t.TempDir()
	// verification only takes effect alongside a phased workflow
	// (parseTeamYML gates Verification/Policies/Capabilities/Retry on
	// len(yc.Workflow.Phases) > 0), so this fixture needs one to actually
	// observe the expanded value downstream, not just in the raw YAML text.
	writeTeamManifest(t, dir, `name: advanced-alias-migrate-fixture
workflow:
  phases: [plan, implement]
advanced:
  verification:
    required: true
`)

	migrated, _, err := MigrateTeamManifestToV1Alpha1(dir)
	if err != nil {
		t.Fatalf("MigrateTeamManifestToV1Alpha1: %v", err)
	}

	if strings.Contains(string(migrated), "advanced:") {
		t.Errorf("migrated output still contains an advanced: block:\n%s", migrated)
	}
	if !strings.Contains(string(migrated), "verification:") {
		t.Errorf("migrated output is missing the expanded verification: field:\n%s", migrated)
	}

	roundTripDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(roundTripDir, "team.yaml"), migrated, 0o644); err != nil {
		t.Fatalf("write round-tripped team.yaml: %v", err)
	}
	cfg, err := parseTeamYML(roundTripDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML(round-tripped): %v", err)
	}
	if !cfg.Verification.Required {
		t.Errorf("cfg.Verification.Required = false after migration, want true (expanded from advanced.verification.required)")
	}
}

func TestTeamMigrateDoesNotChangeEffectiveSpec(t *testing.T) {
	legacyDir := t.TempDir()
	writeMigrateFixtureTeam(t, legacyDir)

	migrated, _, err := MigrateTeamManifestToV1Alpha1(legacyDir)
	if err != nil {
		t.Fatalf("MigrateTeamManifestToV1Alpha1: %v", err)
	}

	v1Dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(v1Dir, "team.yaml"), migrated, 0o644); err != nil {
		t.Fatalf("write migrated team.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v1Dir, "developer.md"), []byte(migrateFixtureDeveloperMD), 0o644); err != nil {
		t.Fatalf("write developer.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(v1Dir, "PROJECT_RULES.txt"), []byte(migrateFixtureProjectRules), 0o644); err != nil {
		t.Fatalf("write PROJECT_RULES.txt: %v", err)
	}

	legacyJSON := compatSnapshotJSON(t, legacyDir)
	v1JSON := compatSnapshotJSON(t, v1Dir)
	if !jsonEqual(t, legacyJSON, v1JSON) {
		t.Errorf("migrating to v1alpha1 changed the effective spec:\n--- legacy ---\n%s\n--- v1alpha1 ---\n%s", legacyJSON, v1JSON)
	}
}
