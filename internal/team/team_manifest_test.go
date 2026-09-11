package team

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTeamManifest(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write team.yaml: %v", err)
	}
}

func TestLegacyFlatTeamStillLoads(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `name: legacy-team
description: A legacy flat manifest with no apiVersion envelope
max-rounds: 7
`)
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.Name != "legacy-team" {
		t.Errorf("Name = %q, want %q", cfg.Name, "legacy-team")
	}
	if cfg.MaxRounds != 7 {
		t.Errorf("MaxRounds = %d, want 7", cfg.MaxRounds)
	}

	version, err := DetectTeamSchemaVersion(dir, nil)
	if err != nil {
		t.Fatalf("DetectTeamSchemaVersion: %v", err)
	}
	if version != SchemaVersionLegacyFlatV0 {
		t.Errorf("schema version = %q, want %q", version, SchemaVersionLegacyFlatV0)
	}
}

func TestV1Alpha1TeamLoads(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: v1alpha1-team
spec:
  description: A v1alpha1 envelope manifest
  max-rounds: 7
`)
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.Name != "v1alpha1-team" {
		t.Errorf("Name = %q, want %q", cfg.Name, "v1alpha1-team")
	}
	if cfg.Description != "A v1alpha1 envelope manifest" {
		t.Errorf("Description = %q", cfg.Description)
	}
	if cfg.MaxRounds != 7 {
		t.Errorf("MaxRounds = %d, want 7", cfg.MaxRounds)
	}

	version, err := DetectTeamSchemaVersion(dir, nil)
	if err != nil {
		t.Fatalf("DetectTeamSchemaVersion: %v", err)
	}
	if version != SchemaVersionV1Alpha1 {
		t.Errorf("schema version = %q, want %q", version, SchemaVersionV1Alpha1)
	}
}

func TestUnknownAPIVersionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v99
kind: AgentTeam
metadata:
  name: future-team
spec:
  description: from the future
`)
	_, err := parseTeamYML(dir, nil)
	if err == nil {
		t.Fatal("expected an error for an unsupported apiVersion, got nil")
	}
	var target *UnsupportedSchemaVersionError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *UnsupportedSchemaVersionError", err)
	}
	if target.APIVersion != "hufu.io/v99" {
		t.Errorf("APIVersion = %q, want %q", target.APIVersion, "hufu.io/v99")
	}
}

func TestUnknownKindFails(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v1alpha1
kind: NotAgentTeam
metadata:
  name: wrong-kind-team
spec:
  description: wrong kind
`)
	_, err := parseTeamYML(dir, nil)
	if err == nil {
		t.Fatal("expected an error for an unsupported kind, got nil")
	}
	var target *UnsupportedManifestKindError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *UnsupportedManifestKindError", err)
	}
	if target.Kind != "NotAgentTeam" {
		t.Errorf("Kind = %q, want %q", target.Kind, "NotAgentTeam")
	}
}

func TestV1Alpha1RejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: unknown-field-team
spec:
  description: has an unknown field
  totally-made-up-field: true
`)
	_, err := parseTeamYML(dir, nil)
	if err == nil {
		t.Fatal("expected an error for an unknown field under spec:, got nil")
	}
	if !strings.Contains(err.Error(), "totally-made-up-field") {
		t.Errorf("error = %v, want it to mention the unknown field", err)
	}
}

func TestV1Alpha1RejectsAdvancedNamespace(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: advanced-alias-team
spec:
  description: tries to use the legacy advanced alias
  advanced:
    workflow:
      phases: []
`)
	_, err := parseTeamYML(dir, nil)
	if err == nil {
		t.Fatal("expected an error: v1alpha1 spec: does not support the advanced: alias namespace")
	}
	if !strings.Contains(err.Error(), "advanced") {
		t.Errorf("error = %v, want it to mention the unsupported advanced field", err)
	}
}

func TestManifestMetadataNameMismatchFails(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: metadata-name
spec:
  name: different-spec-name
  description: metadata.name and spec.name disagree
`)
	_, err := parseTeamYML(dir, nil)
	if err == nil {
		t.Fatal("expected an error when metadata.name and spec.name disagree")
	}
	var target *ManifestMetadataNameMismatchError
	if !errors.As(err, &target) {
		t.Fatalf("error = %v, want *ManifestMetadataNameMismatchError", err)
	}
}

func TestAdvancedNamespaceConflictStillFails(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `name: legacy-advanced-conflict
workflow:
  phases: [plan]
advanced:
  workflow:
    phases: [plan, verify]
`)
	_, err := parseTeamYML(dir, nil)
	if err == nil {
		t.Fatal("expected an error: workflow is defined both at the top level and under advanced")
	}
	if !strings.Contains(err.Error(), "advanced") {
		t.Errorf("error = %v, want it to mention the advanced/top-level conflict", err)
	}
}

func TestLegacyAndV1Alpha1NormalizeIdentically(t *testing.T) {
	legacyDir := t.TempDir()
	writeTeamManifest(t, legacyDir, `name: equivalence-team
description: same content, two schemas
max-rounds: 6
timeout: 900
verify-timeout: 45
max-retries: 3
no-net: true
required-resources:
  - name: project-rules
    kind: project_rules
    path: PROJECT_RULES.txt
    required: true
`)

	v1Dir := t.TempDir()
	writeTeamManifest(t, v1Dir, `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: equivalence-team
spec:
  description: same content, two schemas
  max-rounds: 6
  timeout: 900
  verify-timeout: 45
  max-retries: 3
  no-net: true
  required-resources:
    - name: project-rules
      kind: project_rules
      path: PROJECT_RULES.txt
      required: true
`)

	legacyCfg, err := parseTeamYML(legacyDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML(legacy): %v", err)
	}
	v1Cfg, err := parseTeamYML(v1Dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML(v1alpha1): %v", err)
	}

	legacySnapshot, err := BuildCompatSnapshot(&TeamSession{Config: legacyCfg})
	if err != nil {
		t.Fatalf("BuildCompatSnapshot(legacy): %v", err)
	}
	v1Snapshot, err := BuildCompatSnapshot(&TeamSession{Config: v1Cfg})
	if err != nil {
		t.Fatalf("BuildCompatSnapshot(v1alpha1): %v", err)
	}

	legacyJSON, err := legacySnapshot.JSON()
	if err != nil {
		t.Fatalf("legacy JSON: %v", err)
	}
	v1JSON, err := v1Snapshot.JSON()
	if err != nil {
		t.Fatalf("v1alpha1 JSON: %v", err)
	}
	if !jsonEqual(t, legacyJSON, v1JSON) {
		t.Errorf("legacy and v1alpha1 normalize differently:\n--- legacy ---\n%s\n--- v1alpha1 ---\n%s", legacyJSON, v1JSON)
	}
}

// TestV1Alpha1TeamTasksResolve pins a regression found while canary-migrating
// a real bundled team: session.ContractTasks (and therefore
// CompatSnapshot.Tasks) comes from loadTeamContractTasks, a second,
// independent decode of team.yaml separate from parseTeamYML/cfg. Before
// this fix it always looked for a bare top-level `tasks:` key, so a
// hufu.io/v1alpha1 team's `spec.tasks:` silently resolved to zero tasks
// instead of failing loudly or resolving correctly.
func TestV1Alpha1TeamTasksResolve(t *testing.T) {
	dir := t.TempDir()
	writeTeamManifest(t, dir, `apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: v1alpha1-tasks-team
spec:
  tasks:
    - agent: developer
      when-goal-contains: IMPLEMENT
      side_effect: workspace_write
      recovery: retry
`)
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte("---\nname: developer\nrole: worker\ntools: view,edit,write,grep,glob,ls\n---\nImplement the change.\n"), 0o644); err != nil {
		t.Fatalf("write developer.md: %v", err)
	}

	tasks, err := loadTeamContractTasks(dir, nil)
	if err != nil {
		t.Fatalf("loadTeamContractTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (spec.tasks: must resolve for a v1alpha1 team)", len(tasks))
	}
	if tasks[0].Agent != "developer" || tasks[0].WhenGoalContains != "IMPLEMENT" {
		t.Errorf("tasks[0] = %+v, want agent=developer when-goal-contains=IMPLEMENT", tasks[0])
	}

	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if len(session.ContractTasks) != 1 {
		t.Fatalf("len(session.ContractTasks) = %d, want 1", len(session.ContractTasks))
	}
}
