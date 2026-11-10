package media

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadFileRejectsFIFOWithoutWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(t.Context(), path, Limits{}); err == nil {
		t.Fatal("FIFO accepted")
	}
}
