package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func runRetireCLI(args ...string) (string, error) {
	contextRetireReason = ""
	return helperRunConsolidateCLI(args...)
}

func seedRetireWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	scope := contextstore.Scope{ProjectID: "proj1", TeamID: "demo"}
	persistent := map[string]string{"visibility": "shared", "memory_lifetime": "persistent"}
	for _, item := range []contextstore.ContextItem{
		{ID: "stale", Kind: contextstore.ContextPattern, Content: "Run the retired-marker build script before tests.", Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Metadata: persistent},
		{ID: "keep", Kind: contextstore.ContextPattern, Content: "Run the kept-marker lint before commits.", Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Metadata: persistent},
		{ID: "cand", Kind: contextstore.ContextPattern, Content: "Candidate guidance awaiting review.", Scope: scope, Lifecycle: contextstore.LifecycleCandidate, Metadata: persistent},
	} {
		if err := repo.Append(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RebuildProjection(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestContextRetireCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    func(workspace string) []string
		wantErr string
		wantOut string
	}{
		{name: "reason is required", args: func(ws string) []string {
			return []string{"context", "retire", "stale", "--workspace", ws, "--project", "proj1"}
		}, wantErr: "--reason is required"},
		{name: "scope must match", args: func(ws string) []string {
			return []string{"context", "retire", "stale", "--workspace", ws, "--project", "other", "--reason", "stale"}
		}, wantErr: "outside the requested project/team scope"},
		{name: "unknown id", args: func(ws string) []string {
			return []string{"context", "retire", "missing", "--workspace", ws, "--project", "proj1", "--reason", "stale"}
		}, wantErr: "context item not found"},
		{name: "candidate is refused", args: func(ws string) []string {
			return []string{"context", "retire", "cand", "--workspace", ws, "--project", "proj1", "--reason", "stale"}
		}, wantErr: "not current confirmed knowledge"},
		{name: "confirmed record is retired", args: func(ws string) []string {
			return []string{"context", "retire", "stale", "--workspace", ws, "--project", "proj1", "--team", "demo", "--reason", "script was removed"}
		}, wantOut: "context retire: 1 item(s) retired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := seedRetireWorkspace(t)
			out, err := runRetireCLI(tt.args(workspace)...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("retire error = %v (out %q), want %q", err, out, tt.wantErr)
				}
				return
			}
			if err != nil || !strings.Contains(out, tt.wantOut) {
				t.Fatalf("retire = %q err=%v, want %q", out, err, tt.wantOut)
			}
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = repo.Close() }()
			stale, err := repo.Get(context.Background(), "stale")
			if err != nil || stale.ExpiresAt == nil || stale.Metadata["retired_reason"] != "script was removed" {
				t.Fatalf("retired record = %+v err=%v", stale, err)
			}
			if keep, err := repo.Get(context.Background(), "keep"); err != nil || keep.ExpiresAt != nil {
				t.Fatalf("unrelated record changed: %+v err=%v", keep, err)
			}
			projection, err := os.ReadFile(filepath.Join(workspace, "context-ltm.md"))
			if err != nil || strings.Contains(string(projection), "retired-marker") || !strings.Contains(string(projection), "kept-marker") {
				t.Fatalf("projection after retire = %q err=%v", projection, err)
			}
		})
	}
}
