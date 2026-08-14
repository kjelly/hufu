package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFileSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.json")
	data := []byte(`{"hello":"world"}`)

	if err := AtomicWriteFile(target, data, 0o644); err != nil {
		t.Fatalf("AtomicWriteFile failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("got %s, want %s", got, data)
	}
}

func TestAtomicCreateFileNeverOverwrites(t *testing.T) {
	target := filepath.Join(t.TempDir(), "new.txt")
	if err := AtomicCreateFile(target, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCreateFile(target, []byte("second"), 0o644); err == nil {
		t.Fatal("expected atomic create to reject existing target")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("target overwritten: %q", got)
	}
}

func TestLoadSessionCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	// Create corrupt session file
	target := filepath.Join(dir, sessionFile)
	if err := os.WriteFile(target, []byte(`{"created_at":`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create leftover temp file
	tmpFile := target + ".tmp.12345"
	_ = os.WriteFile(tmpFile, []byte(`temp data`), 0o644)

	session := LoadSession(dir)
	if session != nil {
		t.Errorf("expected nil session for corrupt file, got %+v", session)
	}
}
