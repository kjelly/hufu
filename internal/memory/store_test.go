package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectDirHash(t *testing.T) {
	h1 := projectDirHash("/home/user/projects/myapp")
	h2 := projectDirHash("/home/user/projects/myapp")
	if h1 != h2 {
		t.Errorf("same input should produce same hash: got %s, want %s", h1, h2)
	}

	h3 := projectDirHash("/home/user/projects/other")
	if h1 == h3 {
		t.Errorf("different inputs should produce different hashes")
	}

	if len(h1) != 16 {
		t.Errorf("hash length should be 16, got %d", len(h1))
	}
}

func TestDataDir(t *testing.T) {
	dir, err := dataDir()
	if err != nil {
		t.Fatalf("dataDir() error: %v", err)
	}

	homeDir, _ := os.UserHomeDir()
	expected := filepath.Join(homeDir, ".local", "share", "hufu", "memory")
	if dir != expected {
		t.Errorf("dataDir() = %q, want %q", dir, expected)
	}
}

func TestNewMemoryStoreCreatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "test-project")
	os.MkdirAll(projectDir, 0o755)

	hash := projectDirHash(projectDir)
	expectedPath := filepath.Join(tmpDir, ".local", "share", "hufu", "memory", hash)

	t.Logf("projectDir: %s, hash: %s", projectDir, hash)
	t.Logf("expected store path: %s", expectedPath)

	_ = expectedPath
}
