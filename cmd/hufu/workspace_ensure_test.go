package main

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestWorkspaceEnsurePreservesExistingAndRetiredWorkspaces(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.deps.registryOptions = nil
	ensure := func() map[string]string {
		t.Helper()
		raw := fixture.run(t, "ensure", "--team", "research-fixture", "--output", "json")
		var envelope struct {
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	first := ensure()
	if first["workspace_id"] == "" || first["context_scope_id"] != fixture.projectRoot {
		t.Fatalf("invalid workspace resolution: %v", first)
	}
	marker := filepath.Join(first["control_root"], "source-evidence")
	if err := os.WriteFile(marker, []byte("preserve this evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if next := ensure(); next["workspace_id"] != first["workspace_id"] {
		t.Fatal("ensure replaced an existing workspace")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	fixture.run(t, "delete", "--team", "research-fixture", "--yes", "--output", "json")
	second := ensure()
	if second["workspace_id"] == first["workspace_id"] || second["control_root"] != first["control_root"] {
		t.Fatalf("did not create a fresh destination: first=%v second=%v", first, second)
	}
	registry, err := workspacepkg.OpenReadOnly(fixture.stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = registry.Close() }()
	trash, err := registry.ListTrashWorkspaces(t.Context())
	if err != nil || len(trash) != 1 || trash[0].WorkspaceID != first["workspace_id"] {
		t.Fatalf("retired source changed: trash=%v err=%v", trash, err)
	}
	content, err := os.ReadFile(filepath.Join(trash[0].TrashPath, "source-evidence"))
	if err != nil || string(content) != "preserve this evidence" {
		t.Fatalf("source evidence changed: content=%q err=%v", content, err)
	}
}

func TestWorkspaceEnsureRejectsInvalidRequestsBeforeCreation(t *testing.T) {
	for _, args := range [][]string{
		{"ensure"},
		{"ensure", "--team", "../outside"},
		{"ensure", "--team", "research-fixture", "--output", "invalid"},
	} {
		fixture := newWorkspaceCLIFixture(t)
		if _, err := executeWorkspaceCommand(t, fixture.deps, args...); err == nil {
			t.Fatalf("invalid request accepted: %v", args)
		}
		if _, err := os.Stat(fixture.stateRoot); !os.IsNotExist(err) {
			t.Fatalf("invalid request created state: args=%v err=%v", args, err)
		}
	}
}
