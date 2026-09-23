package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestWorkspaceVersioningConfig(t *testing.T) {
	var cfg Config
	data := []byte(`
workspace-versioning:
  mode: required
  capture:
    max-files: 1000
    max-file-bytes: 2048
  retention:
    keep-recent-snapshots: 7
    orphan-grace: 2h
`)
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	wv := cfg.WorkspaceVersioning
	if wv.Mode != "required" || wv.Capture.MaxFiles != 1000 || wv.Capture.MaxFileBytes != 2048 || wv.Retention.KeepRecent() != 7 {
		t.Fatalf("parsed = %+v", wv)
	}
	if grace, err := wv.Retention.Grace(); err != nil || grace != 2*time.Hour {
		t.Fatalf("grace = %v, %v", grace, err)
	}
	var defaults WorkspaceVersioningRetain
	if defaults.KeepRecent() != 50 {
		t.Fatalf("default keep = %d", defaults.KeepRecent())
	}
	if grace, err := defaults.Grace(); err != nil || grace != 24*time.Hour {
		t.Fatalf("default grace = %v, %v", grace, err)
	}
	if _, err := (WorkspaceVersioningRetain{OrphanGrace: "soon"}).Grace(); err == nil {
		t.Fatal("invalid grace accepted")
	}
}

func TestWorkspaceVersioningConfigMergesAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home.yaml")
	local := filepath.Join(dir, "local.yaml")
	if err := os.WriteFile(home, []byte("workspace-versioning:\n  mode: observe\n  capture:\n    max-files: 10\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("workspace-versioning:\n  mode: required\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(home)
	cfg.mergeFromFile(local)
	if cfg.WorkspaceVersioning.Mode != "required" || cfg.WorkspaceVersioning.Capture.MaxFiles != 10 {
		t.Fatalf("merged = %+v", cfg.WorkspaceVersioning)
	}
}
