package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTeamPackCommandWritesPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: cli-package\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("---\nrole: worker\n---\nWork.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "cli-package.hufu")
	command := newTeamPackCommand()
	stdout := new(bytes.Buffer)
	command.SetOut(stdout)
	command.SetArgs([]string{dir, "--version", "test-1", "--output", output})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); !strings.Contains(got, "packed team cli-package version test-1") || !strings.Contains(got, "sha256:") {
		t.Fatalf("output = %q", got)
	}
}
