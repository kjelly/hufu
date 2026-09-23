package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWriteAndCreate(t *testing.T) {
	tests := []struct {
		name        string
		existing    string
		create      bool
		wantErr     bool
		wantContent string
	}{
		{name: "write new file", create: false, wantContent: "next"},
		{name: "write replaces existing", existing: "old", create: false, wantContent: "next"},
		{name: "create new file", create: true, wantContent: "next"},
		{name: "create refuses existing", existing: "old", create: true, wantErr: true, wantContent: "old"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nested", "file.txt")
			if tt.existing != "" {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tt.existing), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if tt.create {
				err = AtomicCreateFile(path, []byte("next"), 0o600)
			} else {
				err = AtomicWriteFile(path, []byte("next"), 0o600)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != tt.wantContent {
				t.Fatalf("content = %q, want %q", got, tt.wantContent)
			}
			entries, readDirErr := os.ReadDir(filepath.Dir(path))
			if readDirErr != nil {
				t.Fatal(readDirErr)
			}
			for _, entry := range entries {
				if AtomicTempFilePattern.MatchString(entry.Name()) {
					t.Fatalf("temp file %q was left behind", entry.Name())
				}
			}
		})
	}
}

func TestSyncDirMissingDirectory(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("SyncDir on a missing directory succeeded")
	}
}

func TestTryLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, err := TryLockExclusive(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if _, err = TryLockExclusive(path); !errors.Is(err, ErrWouldBlock) {
		t.Fatalf("second lock err = %v, want ErrWouldBlock", err)
	}
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("lock file mode = %o, want 600", perm)
		}
	}
	if err = first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	again, err := TryLockExclusive(path)
	if err != nil {
		t.Fatalf("relock after close: %v", err)
	}
	if err = again.Close(); err != nil {
		t.Fatal(err)
	}
}
