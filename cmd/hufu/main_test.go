package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/config"
)

func TestProviderURLToOllamaAPI(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"http://localhost:11434/v1", "http://localhost:11434/api"},
		{"http://localhost:11434/v1/", "http://localhost:11434/api"},
		{"http://localhost:11434/", "http://localhost:11434/api"},
		{"http://localhost:11434", "http://localhost:11434/api"},
		{"http://192.168.1.100:11434/v1", "http://192.168.1.100:11434/api"},
	}
	for _, tt := range tests {
		result := config.ProviderURLToOllamaAPI(tt.input)
		if result != tt.expected {
			t.Errorf("config.ProviderURLToOllamaAPI(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestCompleteAtNames(t *testing.T) {
	tmpDir := t.TempDir()

	devDir := filepath.Join(tmpDir, "dev-team")
	if err := os.MkdirAll(devDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "developer.md"), []byte("---"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "reviewer.md"), []byte("---"), 0644); err != nil {
		t.Fatal(err)
	}

	resDir := filepath.Join(tmpDir, "research-team")
	if err := os.MkdirAll(resDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resDir, "analyst.md"), []byte("---"), 0644); err != nil {
		t.Fatal(err)
	}

	oldSearchPath := opts.agentTeamSearchPath
	opts.agentTeamSearchPath = tmpDir
	defer func() {
		opts.agentTeamSearchPath = oldSearchPath
	}()

	t.Run("empty or no prefix suggests all teams with @", func(t *testing.T) {
		got := completeAtNames("")
		want := []string{"@dev-team", "@research-team"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("prefix @ matches teams", func(t *testing.T) {
		got := completeAtNames("@de")
		want := []string{"@dev-team", "@developer"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("prefix @ matches agents", func(t *testing.T) {
		got := completeAtNames("@an")
		want := []string{"@analyst"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestRootCommandSilencesUsageOnError(t *testing.T) {
	cmd := newRootCommand()
	if !cmd.SilenceUsage {
		t.Fatal("rootCommand.SilenceUsage must be true to avoid printing usage on error exit")
	}
}

func TestCommandErrorDoesNotPrintUsage(t *testing.T) {
	cmd := newRootCommand()
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--invalid-test-flag"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for invalid flag")
	}
	output := buf.String()
	if strings.Contains(output, "Usage:") {
		t.Fatalf("expected output not to contain Usage: on error, got:\n%s", output)
	}
}

