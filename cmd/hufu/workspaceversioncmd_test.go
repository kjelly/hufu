package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCLIWorkspaceVersionMaintenance(t *testing.T) {
	f := newVersionedCLIFixture(t)
	f.write(t, "A.txt", "main")
	if _, err := runCLI(t, "session", "fork", "--name", "exp", "--workspace", f.control); err != nil {
		t.Fatalf("fork: %v", err)
	}

	out, err := runCLI(t, "workspace", "version", "status", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var status team.WorkspaceVersionStatus
	if err = json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("status output %q: %v", out, err)
	}
	if status.ModeFloor != "required" || status.ActiveBranchID != "exp" || status.LiveDrift == nil || *status.LiveDrift {
		t.Fatalf("status = %#v", status)
	}

	out, err = runCLI(t, "workspace", "version", "doctor", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("doctor: %v (%s)", err, out)
	}
	if strings.Contains(out, `"severity":"error"`) {
		t.Fatalf("doctor found errors: %s", out)
	}

	out, err = runCLI(t, "workspace", "version", "gc", "--workspace", f.control, "--json")
	if err != nil || !strings.Contains(out, `"applied":false`) {
		t.Fatalf("gc dry run = %s, %v", out, err)
	}
	if _, err = runCLI(t, "workspace", "version", "gc", "--apply", "--workspace", f.control); err != nil {
		t.Fatalf("gc --apply: %v", err)
	}

	// Switching the config to off is refused until the floor is lowered.
	home := os.Getenv("HOME")
	if err = os.WriteFile(filepath.Join(home, ".config", "hufu", "hufu.yaml"), []byte("workspace-versioning:\n  mode: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = runCLI(t, "session", "list", "--workspace", f.control); err == nil || !strings.Contains(err.Error(), "floor required") {
		t.Fatalf("list below the floor: err = %v", err)
	}
	if _, err = runCLI(t, "workspace", "version", "downgrade", "off", "--workspace", f.control); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if _, err = runCLI(t, "session", "list", "--workspace", f.control); err != nil {
		t.Fatalf("list after downgrade: %v", err)
	}
	// With versioning off, checkout is metadata-only again: files stay.
	f.write(t, "A.txt", "edited while off")
	if _, err = runCLI(t, "session", "checkout", "main", "--workspace", f.control); err != nil {
		t.Fatalf("checkout while off: %v", err)
	}
	if f.read(t, "A.txt") != "edited while off" {
		t.Fatal("an off-mode checkout changed files")
	}
}
