package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 2 (spec.md Specification 05): filename-based agent name/role
// inference. Explicit frontmatter always overrides inference; a filename
// only fills in a name/role that frontmatter left unset (or omitted
// entirely, including a file with no frontmatter block at all).

func writeInferenceAgentFile(t *testing.T, dir, filename, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentIdentityInference_FilenameInfersName(t *testing.T) {
	dir := t.TempDir()
	writeInferenceAgentFile(t, dir, "developer.md", "---\nrole: worker\n---\nImplement the change.\n")

	def, err := parseAgentFile(filepath.Join(dir, "developer.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.Name != "developer" {
		t.Fatalf("Name = %q, want inferred %q", def.Name, "developer")
	}
	if def.Role != "worker" {
		t.Fatalf("Role = %q, want %q", def.Role, "worker")
	}
}

func TestAgentIdentityInference_CoordinatorFilenameInfersCoordinatorRole(t *testing.T) {
	dir := t.TempDir()
	writeInferenceAgentFile(t, dir, "coordinator.md", "---\ndescription: leads the team\n---\nDelegate work and synthesize a final answer.\n")

	def, err := parseAgentFile(filepath.Join(dir, "coordinator.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.Name != "coordinator" || def.Role != "coordinator" {
		t.Fatalf("Name/Role = %q/%q, want %q/%q", def.Name, def.Role, "coordinator", "coordinator")
	}
}

func TestAgentIdentityInference_ExplicitNameOverridesFilename(t *testing.T) {
	dir := t.TempDir()
	writeInferenceAgentFile(t, dir, "developer.md", "---\nname: coder\nrole: worker\n---\nImplement the change.\n")

	def, err := parseAgentFile(filepath.Join(dir, "developer.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.Name != "coder" {
		t.Fatalf("Name = %q, want explicit %q (not filename-inferred %q)", def.Name, "coder", "developer")
	}
}

func TestAgentIdentityInference_ExplicitRoleOverridesCoordinatorFilename(t *testing.T) {
	dir := t.TempDir()
	writeInferenceAgentFile(t, dir, "coordinator.md", "---\nrole: worker\n---\nA worker that happens to be named coordinator.\n")

	def, err := parseAgentFile(filepath.Join(dir, "coordinator.md"), nil)
	if err != nil {
		t.Fatalf("parseAgentFile: %v", err)
	}
	if def.Role != "worker" {
		t.Fatalf("Role = %q, want explicit %q (not inferred %q)", def.Role, "worker", "coordinator")
	}
}

func TestAgentIdentityInference_DuplicateInferredNameCollision(t *testing.T) {
	dir := t.TempDir()
	// Two files that both omit an explicit name: one infers "reviewer" from
	// its own filename, the other explicitly claims that same name.
	writeInferenceAgentFile(t, dir, "reviewer.md", "---\nrole: worker\n---\nReview the change.\n")
	writeInferenceAgentFile(t, dir, "second-reviewer.md", "---\nname: reviewer\nrole: worker\n---\nAlso review the change.\n")

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil || !strings.Contains(err.Error(), "reviewer.md") || !strings.Contains(err.Error(), "second-reviewer.md") {
		t.Fatalf("LoadTeam error = %v, want collision naming both source files", err)
	}
}

func TestAgentIdentityInference_MultipleCoordinatorsRejected(t *testing.T) {
	dir := t.TempDir()
	writeInferenceAgentFile(t, dir, "coordinator.md", "Delegate work to workers.\n")
	writeInferenceAgentFile(t, dir, "lead.md", "---\nrole: coordinator\n---\nAlso leads the team.\n")

	_, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err == nil || !strings.Contains(err.Error(), "more than one coordinator") {
		t.Fatalf("LoadTeam error = %v, want more-than-one-coordinator rejection", err)
	}
}

func TestAgentIdentityInference_InvalidFilenameForInferenceRequiresExplicitName(t *testing.T) {
	dir := t.TempDir()
	writeInferenceAgentFile(t, dir, "2fast.md", "---\nrole: worker\n---\nContent.\n")

	_, err := parseAgentFile(filepath.Join(dir, "2fast.md"), nil)
	if err == nil {
		t.Fatal("expected error for a filename that cannot be used as an inferred agent name")
	}

	writeInferenceAgentFile(t, dir, "2fast-explicit.md", "---\nname: fast-agent\nrole: worker\n---\nContent.\n")
	def, err := parseAgentFile(filepath.Join(dir, "2fast-explicit.md"), nil)
	if err != nil {
		t.Fatalf("explicit name should bypass filename inference entirely: %v", err)
	}
	if def.Name != "fast-agent" {
		t.Fatalf("Name = %q, want %q", def.Name, "fast-agent")
	}
}
