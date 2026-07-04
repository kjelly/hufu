package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestNormalizeList(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"string", "a,b,c", "a,b,c"},
		{"string trimmed", "  a,b ", "a,b"},
		{"list", []interface{}{"a", "b", "c"}, "a, b, c"},
		{"list ints", []interface{}{1, 2}, "1, 2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeList(tc.in); got != tc.want {
				t.Errorf("normalizeList(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReadAgentFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.md")
	content := `---
name: developer
description: writes code
role: worker
model: ollama/qwen3:8b
tools: view,edit,bash
skills:
  - code-review
  - tdd
---
You are a developer.
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := readAgentFrontmatter(path)
	if fm.Name != "developer" {
		t.Errorf("Name = %q", fm.Name)
	}
	if fm.Role != "worker" {
		t.Errorf("Role = %q", fm.Role)
	}
	if fm.Model != "ollama/qwen3:8b" {
		t.Errorf("Model = %q", fm.Model)
	}
	if got := normalizeList(fm.Tools); got != "view,edit,bash" {
		t.Errorf("Tools = %q", got)
	}
	if got := normalizeList(fm.Skills); got != "code-review, tdd" {
		t.Errorf("Skills = %q", got)
	}
}

func TestReadAgentFrontmatter_NoFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.md")
	if err := os.WriteFile(path, []byte("# Just markdown\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := readAgentFrontmatter(path)
	if fm.Name != "" || fm.Role != "" {
		t.Errorf("expected zero-value frontmatter, got %+v", fm)
	}
}

func TestWriteIfAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")

	created, err := writeIfAbsent(path, "first")
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Error("expected first write to create the file")
	}

	created, err = writeIfAbsent(path, "second")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("expected second write to be a no-op")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "first" {
		t.Errorf("file was overwritten: %q", string(data))
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Errorf("got %q, want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestModelAvailable(t *testing.T) {
	avail := []string{"qwen3:8b", "ollama/llama3"}
	if !modelAvailable("qwen3:8b", avail) {
		t.Error("qwen3:8b should be available")
	}
	if !modelAvailable("llama3", avail) {
		t.Error("llama3 should match ollama/llama3")
	}
	if modelAvailable("gpt-4", avail) {
		t.Error("gpt-4 should not be available")
	}
}

// newProfileTestCmd builds a minimal command with the flags applyProfile cares about.
func newProfileTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().BoolVar(&think, "think", false, "")
	cmd.Flags().Int64Var(&maxDuration, "max-duration", 0, "")
	cmd.Flags().StringVar(&modelOverride, "model", "", "")
	return cmd
}

func TestApplyProfile_AppliesValues(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  batch:
    think: "true"
    max-duration: "600"
`)
	defer chdir(t, dir)()

	// reset globals
	think, maxDuration, modelOverride, profileName = false, 0, "", "batch"
	defer func() { profileName = "" }()

	cmd := newProfileTestCmd()
	if err := applyProfile(cmd); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}
	if !think {
		t.Error("expected think=true from profile")
	}
	if maxDuration != 600 {
		t.Errorf("expected max-duration=600, got %d", maxDuration)
	}
}

func TestApplyProfile_CLIWins(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, `
profiles:
  batch:
    max-duration: "600"
`)
	defer chdir(t, dir)()

	think, maxDuration, modelOverride, profileName = false, 0, "", "batch"
	defer func() { profileName = "" }()

	cmd := newProfileTestCmd()
	// Simulate an explicit CLI flag.
	if err := cmd.Flags().Set("max-duration", "30"); err != nil {
		t.Fatal(err)
	}
	if err := applyProfile(cmd); err != nil {
		t.Fatalf("applyProfile: %v", err)
	}
	if maxDuration != 30 {
		t.Errorf("explicit CLI flag should win, got max-duration=%d", maxDuration)
	}
}

func TestApplyProfile_UnknownProfile(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, "profiles:\n  batch:\n    think: \"true\"\n")
	defer chdir(t, dir)()

	profileName = "nope"
	defer func() { profileName = "" }()

	cmd := newProfileTestCmd()
	err := applyProfile(cmd)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	if !strings.Contains(err.Error(), "batch") {
		t.Errorf("error should list available profiles: %v", err)
	}
}

func TestApplyProfile_UnknownFlag(t *testing.T) {
	dir := t.TempDir()
	writeHufuYAML(t, dir, "profiles:\n  bad:\n    no-such-flag: \"x\"\n")
	defer chdir(t, dir)()

	profileName = "bad"
	defer func() { profileName = "" }()

	cmd := newProfileTestCmd()
	err := applyProfile(cmd)
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("expected unknown-flag error, got %v", err)
	}
}

func TestApplyProfile_NoProfileIsNoop(t *testing.T) {
	profileName = ""
	cmd := newProfileTestCmd()
	if err := applyProfile(cmd); err != nil {
		t.Errorf("empty profile should be a no-op, got %v", err)
	}
}

func TestResolveInitialPrompt(t *testing.T) {
	// Passing a non-empty initialPrompt should bypass stdin and interactive fallbacks.
	got, err := resolveInitialPrompt("test prompt", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got, "test prompt") {
		t.Errorf("got %q, expected prefix 'test prompt'", got)
	}
}

// --- helpers ---

func writeHufuYAML(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "hufu.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// chdir changes into dir and returns a func that restores the original wd.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Chdir(orig) }
}
