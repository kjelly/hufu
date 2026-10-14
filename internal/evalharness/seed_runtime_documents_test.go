package evalharness

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestSeedWorkspaceFilesRoutesRuntimeState(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		body     string
		document string // non-empty: seeded as this runtime document
		wantErr  bool
	}{
		{name: "session checkpoint", key: "session.json", body: `{"rounds":1}`, document: runtimestore.DocumentSession},
		{name: "terminal sessions", key: "logs/terminal_sessions.json", body: `[]`, document: runtimestore.DocumentTerminalSessions},
		{name: "malformed runtime state", key: "session.json", body: `{`, wantErr: true},
		{name: "retired state file", key: "session_tree.json", body: `{}`, wantErr: true},
		{name: "ordinary file", key: "inputs/manifest.json", body: `{"ok":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			err := seedWorkspaceFiles(workspace, map[string]string{tt.key: tt.body})
			if (err != nil) != tt.wantErr {
				t.Fatalf("seedWorkspaceFiles error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			_, statErr := os.Stat(filepath.Join(workspace, tt.key))
			if tt.document == "" {
				if statErr != nil {
					t.Fatalf("ordinary file was not written: %v", statErr)
				}
				return
			}
			if statErr == nil {
				t.Fatalf("runtime state %s was written as a file", tt.key)
			}
			store, err := runtimestore.OpenReadOnly(context.Background(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			body, exists, err := store.GetDocument(context.Background(), tt.document)
			if err != nil || !exists || string(body) != tt.body {
				t.Fatalf("document %s = %q, %v, %v; want %q", tt.document, body, exists, err, tt.body)
			}
		})
	}
}
