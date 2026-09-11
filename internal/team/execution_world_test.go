package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Phase 3 tests (spec.md §36 PR-06): the generic ExecutionWorld contract and
// its local-sandbox implementation. Still no Codex — every test drives
// LocalExecutionWorld directly.

// TestExecutionWorldSideEffectMapping proves §10.5's Hufu-side-effect
// projection: none is read-only, workspace_write/external_write/
// infra_mutation all project to workspace-write (the wider effect is simply
// unavailable in v1, not silently granted), and credential_mutation has no
// projection — Prepare must reject it outright.
func TestExecutionWorldSideEffectMapping(t *testing.T) {
	cases := []struct {
		name         string
		effect       SideEffectClass
		wantWritable bool
		wantReadOnly bool
		wantRejected bool
	}{
		{name: "none", effect: SideEffectNone, wantReadOnly: true},
		{name: "empty defaults like none", effect: "", wantReadOnly: true},
		{name: "workspace_write", effect: SideEffectWorkspaceWrite, wantWritable: true},
		{name: "external_write maps to workspace-write only", effect: SideEffectExternalWrite, wantWritable: true},
		{name: "infra_mutation maps to workspace-write only", effect: SideEffectInfraMutation, wantWritable: true},
		{name: "credential_mutation is rejected", effect: SideEffectCredential, wantRejected: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			world := NewLocalExecutionWorld()
			prepared, err := world.Prepare(context.Background(), ExecutionWorldSpec{Root: root, SideEffect: tc.effect})
			if tc.wantRejected {
				if err == nil {
					t.Fatal("expected Prepare to reject credential_mutation admission")
				}
				return
			}
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			t.Cleanup(func() { _ = world.Release(context.Background(), prepared) })
			if tc.wantWritable && len(prepared.WritableRoots) == 0 {
				t.Fatalf("WritableRoots = %#v, want the root to be writable", prepared.WritableRoots)
			}
			if tc.wantReadOnly && (len(prepared.WritableRoots) != 0 || len(prepared.ReadOnlyRoots) == 0) {
				t.Fatalf("writable=%#v readOnly=%#v, want read-only only", prepared.WritableRoots, prepared.ReadOnlyRoots)
			}
		})
	}
}

// TestExecutionWorldDoesNotInheritSecrets proves the process environment is
// built strictly from EnvironmentAllowlist, never from os.Environ(), so a
// secret present in Hufu's own process environment never reaches the
// prepared world just because it happens to be set (§10.7).
func TestExecutionWorldDoesNotInheritSecrets(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("OPENAI_API_KEY", "sk-should-never-appear")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "also-should-never-appear")

	root := t.TempDir()
	world := NewLocalExecutionWorld()
	prepared, err := world.Prepare(context.Background(), ExecutionWorldSpec{
		Root: root, SideEffect: SideEffectWorkspaceWrite,
		EnvironmentAllowlist: []string{"PATH", "CODEX_HOME"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = world.Release(context.Background(), prepared) })
	foundPath := false
	for _, kv := range prepared.Environment {
		if strings.HasPrefix(kv, "OPENAI_API_KEY=") || strings.HasPrefix(kv, "AWS_SECRET_ACCESS_KEY=") {
			t.Fatalf("prepared environment %v leaked a non-allowlisted secret variable", prepared.Environment)
		}
		if kv == "PATH=/usr/bin:/bin" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("prepared environment %v missing the allowlisted PATH", prepared.Environment)
	}
}

func TestExecutionWorldUsesProvidedEnvironmentSnapshot(t *testing.T) {
	firstHome := t.TempDir()
	secondHome := t.TempDir()
	t.Setenv("HOME", secondHome)
	childEnvironment := []string{"HOME=" + firstHome}

	world := NewLocalExecutionWorld()
	prepared, err := world.Prepare(context.Background(), ExecutionWorldSpec{
		Root:                 t.TempDir(),
		SideEffect:           SideEffectWorkspaceWrite,
		EnvironmentAllowlist: []string{"HOME"},
		Environment:          childEnvironment,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = world.Release(context.Background(), prepared) })

	if len(prepared.Environment) != 1 || prepared.Environment[0] != childEnvironment[0] {
		t.Fatalf("prepared environment = %v, want the provided child snapshot %v", prepared.Environment, childEnvironment)
	}
	childEnvironment[0] = "HOME=" + secondHome
	if prepared.Environment[0] != "HOME="+firstHome {
		t.Fatalf("prepared environment changed after caller mutation: %v", prepared.Environment)
	}
}

// TestExecutionWorldRejectsCredentialMutation proves admission for a
// credential_mutation task fails before any workspace side effect (§10.5's
// last row: "reject provider admission").
func TestExecutionWorldRejectsCredentialMutation(t *testing.T) {
	world := NewLocalExecutionWorld()
	_, err := world.Prepare(context.Background(), ExecutionWorldSpec{Root: t.TempDir(), SideEffect: SideEffectCredential})
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("Prepare error = %v, want a credential_mutation rejection", err)
	}
}

// TestExecutionWorldRejectsOutsideWritableRoot proves
// ValidateExecutionWorldDelta rejects any observed change outside the
// prepared WritableRoots, and accepts one inside them (§10.4: "detect
// changes outside allowed writable roots after execution").
func TestExecutionWorldRejectsOutsideWritableRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "allowed"), 0o755); err != nil {
		t.Fatal(err)
	}
	world := NewLocalExecutionWorld()
	prepared, err := world.Prepare(context.Background(), ExecutionWorldSpec{
		Root: root, SideEffect: SideEffectWorkspaceWrite,
		WritableRoots: []string{filepath.Join(root, "allowed")},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = world.Release(context.Background(), prepared) })

	if err := ValidateExecutionWorldDelta(prepared, WorkspaceDelta{
		Modified: []WorkspaceFileState{{Path: "allowed/inside.txt"}},
	}); err != nil {
		t.Fatalf("in-bounds change unexpectedly rejected: %v", err)
	}

	if err := ValidateExecutionWorldDelta(prepared, WorkspaceDelta{
		Added: []WorkspaceFileState{{Path: "outside.txt"}},
	}); err == nil {
		t.Fatal("expected a change outside the writable root to be rejected")
	}

	if err := ValidateExecutionWorldDelta(prepared, WorkspaceDelta{
		Deleted: []string{"elsewhere.txt"},
	}); err == nil {
		t.Fatal("expected a deletion outside the writable root to be rejected")
	}
}

func TestExecutionWorldSerializesSharedWorkspace(t *testing.T) {
	root := t.TempDir()
	firstWorld := NewLocalExecutionWorld()
	first, err := firstWorld.Prepare(t.Context(), ExecutionWorldSpec{Root: root, SideEffect: SideEffectWorkspaceWrite})
	if err != nil {
		t.Fatalf("first Prepare: %v", err)
	}
	t.Cleanup(func() { _ = firstWorld.Release(context.Background(), first) })

	secondWorld := NewLocalExecutionWorld()
	secondReady := make(chan *PreparedExecutionWorld, 1)
	secondErr := make(chan error, 1)
	go func() {
		prepared, prepareErr := secondWorld.Prepare(context.Background(), ExecutionWorldSpec{Root: root, SideEffect: SideEffectWorkspaceWrite})
		if prepareErr != nil {
			secondErr <- prepareErr
			return
		}
		secondReady <- prepared
	}()

	select {
	case prepared := <-secondReady:
		_ = secondWorld.Release(context.Background(), prepared)
		t.Fatal("second shared-workspace Prepare completed while the first lease was held")
	case err := <-secondErr:
		t.Fatalf("second shared-workspace Prepare failed before the first lease was released: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	if err := firstWorld.Release(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	select {
	case prepared := <-secondReady:
		if err := secondWorld.Release(context.Background(), prepared); err != nil {
			t.Fatal(err)
		}
	case err := <-secondErr:
		t.Fatalf("second shared-workspace Prepare after release: %v", err)
	case <-time.After(time.Second):
		t.Fatal("second shared-workspace Prepare did not proceed after lease release")
	}
}
