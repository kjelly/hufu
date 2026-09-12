package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSessionReadOnlyDoesNotCreateMissingWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	session, exists, err := LoadSessionReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if session != nil || exists {
		t.Fatalf("missing session = %#v exists=%t", session, exists)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("read-only session load created workspace: %v", err)
	}
}

func TestLoadSessionReadOnlyReportsMalformedProjection(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "session.json"), []byte(`{"tasks":`), 0o600); err != nil {
		t.Fatal(err)
	}
	session, exists, err := LoadSessionReadOnly(workspace)
	if err == nil || !exists || session != nil {
		t.Fatalf("malformed session = %#v exists=%t err=%v", session, exists, err)
	}
}
