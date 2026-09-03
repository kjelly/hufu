package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 4 (spec.md Specification 05 / Specification 02): EffectiveTeamSpec
// compiler. These tests exercise CompileTeam/ValidateEffectiveTeam as a
// provenance-tracking wrapper around the existing LoadTeam pipeline.

func writeEffectiveSpecFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCompileTeam_BuiltinDefaultsCarryBuiltinProvenance(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\nrole: worker\n---\nImplement the change.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	if spec.MaxRounds.Source != SourceBuiltin {
		t.Errorf("MaxRounds.Source = %q, want %q", spec.MaxRounds.Source, SourceBuiltin)
	}
	if spec.MaxRounds.Value != 10 {
		t.Errorf("MaxRounds.Value = %d, want 10", spec.MaxRounds.Value)
	}
	if spec.Name.Source != SourceFilename {
		t.Errorf("Name.Source = %q, want %q (no team.yaml written)", spec.Name.Source, SourceFilename)
	}
}

func TestCompileTeam_ExplicitTeamValueCarriesTeamProvenance(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "team.yaml", "name: custom-team\nmax-rounds: 5\n")
	writeEffectiveSpecFile(t, dir, "developer.md", "---\nrole: worker\n---\nImplement the change.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	if spec.Name.Source != SourceTeam || spec.Name.Value != "custom-team" {
		t.Errorf("Name = %+v, want {custom-team team ...}", spec.Name)
	}
	if spec.MaxRounds.Source != SourceTeam || spec.MaxRounds.Value != 5 {
		t.Errorf("MaxRounds = %+v, want {5 team ...}", spec.MaxRounds)
	}
	// timeout was never authored, so it must still show builtin provenance.
	if spec.Timeout.Source != SourceBuiltin {
		t.Errorf("Timeout.Source = %q, want %q", spec.Timeout.Source, SourceBuiltin)
	}
}

func TestCompileTeam_AgentIdentityProvenance(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\nname: coder\nrole: worker\n---\nImplement the change.\n")
	writeEffectiveSpecFile(t, dir, "coordinator.md", "Delegate work to the workers.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	dev := spec.Agents["developer"]
	if dev.Name.Source != SourceAgent || dev.Name.Value != "coder" {
		t.Errorf("developer Name = %+v, want explicit {coder agent ...}", dev.Name)
	}
	coord := spec.Agents["coordinator"]
	if coord.Name.Source != SourceFilename || coord.Role.Source != SourceFilename {
		t.Errorf("coordinator Name/Role = %+v/%+v, want filename-inferred", coord.Name, coord.Role)
	}
	if coord.Role.Value != "coordinator" {
		t.Errorf("coordinator Role.Value = %q, want %q", coord.Role.Value, "coordinator")
	}
}

func TestCompileTeam_DeniedToolProvenanceIsVisible(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\npreset: coding\ntools:\n  denied: [bash]\n---\nImplement the change.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	dev := spec.Agents["developer"]
	if !strings.Contains(dev.Tools.Detail, "denied by agent: bash") {
		t.Errorf("developer Tools.Detail = %q, want it to mention the agent-level bash denial", dev.Tools.Detail)
	}
}

func TestCompileTeam_PresetToolsProvenance(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\npreset: coding\n---\nImplement the change.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	dev := spec.Agents["developer"]
	if dev.Tools.Source != SourcePreset || dev.Tools.Detail != "preset:coding" {
		t.Errorf("developer Tools = %+v, want source preset detail preset:coding", dev.Tools)
	}
	if dev.SideEffect.Source != SourcePreset || dev.SideEffect.Value != "workspace_write" {
		t.Errorf("developer SideEffect = %+v, want preset-sourced workspace_write", dev.SideEffect)
	}
}

func TestCompileTeam_ExplicitSideEffectOverridesPresetProvenance(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "reviewer.md", "---\npreset: review\nside_effect: workspace_write\n---\nReview the change.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	rev := spec.Agents["reviewer"]
	if rev.SideEffect.Source != SourceAgent || rev.SideEffect.Value != "workspace_write" {
		t.Errorf("reviewer SideEffect = %+v, want explicit agent-sourced workspace_write", rev.SideEffect)
	}
}

func TestCompileTeam_CompileFailurePropagatesLoadTeamError(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\npreset: not-a-real-preset\n---\nImplement the change.\n")

	if _, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry); err == nil {
		t.Fatal("expected CompileTeam to fail for an unknown preset, got nil")
	}
}

func TestCompileTeam_IsImmutableAcrossRepeatedCalls(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\nrole: worker\n---\nImplement the change.\n")

	first, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	second, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	if first.MaxRounds != second.MaxRounds {
		t.Fatalf("repeated CompileTeam calls diverged: %+v vs %+v", first.MaxRounds, second.MaxRounds)
	}
	if first.RuntimeSession() == second.RuntimeSession() {
		t.Fatal("each CompileTeam call should produce its own session, not share mutable state")
	}
}

func TestValidateEffectiveTeam_MatchesCompileTeamPipeline(t *testing.T) {
	dir := t.TempDir()
	writeEffectiveSpecFile(t, dir, "developer.md", "---\nrole: worker\ntools: view,write,edit,grep,glob,ls,bash\n---\nImplement the change.\n")

	spec, err := CompileTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("CompileTeam: %v", err)
	}
	// A clean team should have no error-severity findings; ValidateEffectiveTeam
	// must not fabricate one.
	for _, finding := range ValidateEffectiveTeam(spec) {
		if finding.Severity == FindingSeverityError {
			t.Errorf("unexpected error finding on a clean team: %+v", finding)
		}
	}
}

func TestValidateEffectiveTeam_NilSpecReturnsNoFindings(t *testing.T) {
	if findings := ValidateEffectiveTeam(nil); findings != nil {
		t.Fatalf("ValidateEffectiveTeam(nil) = %v, want nil", findings)
	}
}
