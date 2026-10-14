package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestWorkspaceConvertStorageCommand(t *testing.T) {
	fixture := newWorkspaceCLIFixture(t)
	fixture.run(t, "register")
	registry, err := workspacepkg.OpenReadWrite(fixture.stateRoot,
		workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x51, 0x52)))),
		workspacepkg.WithClock(func() time.Time { return fixture.now }),
	)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), fixture.projectID, "default")
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()

	// Lay out the files an older Hufu wrote.
	source := t.TempDir()
	events, err := team.NewEventStore(source, "run-cli", "session-cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := events.Append(team.RunEvent{Type: "run_started", Actor: "user"}); err != nil {
		t.Fatal(err)
	}
	_ = events.Close()
	var log bytes.Buffer
	if err := team.ExportEventLog(context.Background(), source, &log); err != nil {
		t.Fatal(err)
	}
	ws := workspace.ControlRoot
	if err := os.MkdirAll(filepath.Join(ws, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "logs", "event_store.jsonl"), log.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "session.json"), []byte(`{"rounds":2}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, err := executeWorkspaceCommand(t, fixture.deps, "convert-storage", "--dry-run")
	if err != nil || !strings.Contains(stdout, "Would convert") || !strings.Contains(stdout, "events: 1") || runtimestore.Exists(ws) {
		t.Fatalf("dry run = %q, %v (runtime.sqlite exists: %v)", stdout, err, runtimestore.Exists(ws))
	}
	if stdout, err = executeWorkspaceCommand(t, fixture.deps, "convert-storage"); err == nil || stdout != "" {
		t.Fatalf("unconfirmed conversion = %q, %v", stdout, err)
	}
	locks, err := workspacepkg.AcquireWorkspaceLocks(fixture.stateRoot, []string{workspace.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executeWorkspaceCommand(t, fixture.deps, "convert-storage", "--yes"); err == nil || !strings.Contains(err.Error(), "busy") || runtimestore.Exists(ws) {
		t.Fatalf("conversion of a workspace in use = %v", err)
	}
	if err := locks.Close(); err != nil {
		t.Fatal(err)
	}
	stdout, err = executeWorkspaceCommand(t, fixture.deps, "convert-storage", "--yes", "--output", "json")
	if err != nil || !strings.Contains(stdout, `"events": 1`) || !runtimestore.Exists(ws) {
		t.Fatalf("conversion = %q, %v", stdout, err)
	}
	if session := team.LoadSession(ws); session == nil || session.Rounds != 2 {
		t.Fatalf("converted session = %+v", session)
	}
}
