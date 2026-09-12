package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEvalListDefaultsToRepositoryEvalRoot(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	t.Chdir(repoRoot)

	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	if err := runEvalList(cmd, nil); err != nil {
		t.Fatalf("runEvalList: %v", err)
	}
	lines := strings.Fields(output.String())
	if len(lines) != 11 {
		t.Fatalf("listed %d cases, want 11: %s", len(lines), output.String())
	}
	if !strings.Contains(output.String(), "core-lifecycle/single-task-unverified") {
		t.Fatalf("default eval listing omitted core lifecycle case: %s", output.String())
	}
}
