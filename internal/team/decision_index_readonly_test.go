package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDecisionIndexEntriesReadOnlyDoesNotCreateMissingWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	entries, err := LoadDecisionIndexEntriesReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %#v, want none", entries)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("read-only decision load created workspace: %v", err)
	}
}
