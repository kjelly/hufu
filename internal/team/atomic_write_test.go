package team

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
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

func TestSweepStaleAtomicTempFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "logs", "event_store.jsonl.tmp.09138b0b")
	fresh := filepath.Join(dir, "session_tree.json.tmp.f10eac63")
	notAnOrphan := filepath.Join(dir, "session_tree.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stale, fresh, notAnOrphan} {
		if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-2 * staleAtomicTempFileAge)
	if err := os.Chtimes(stale, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	SweepStaleAtomicTempFiles(dir)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("expected stale temp file to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("expected recently-written temp file to survive (still in flight): %v", err)
	}
	if _, err := os.Stat(notAnOrphan); err != nil {
		t.Errorf("expected non-temp file to be untouched: %v", err)
	}
}

func TestLoadSessionCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	// A truncated checkpoint is rejected rather than half-decoded.
	putRuntimeDocument(t, dir, runtimestore.DocumentSession, []byte(`{"created_at":`))

	session := LoadSession(dir)
	if session != nil {
		t.Errorf("expected nil session for corrupt file, got %+v", session)
	}
}
