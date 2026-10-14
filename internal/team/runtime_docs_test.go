package team

import (
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// TestSessionCheckpointAfterDatabaseReplacementReachesTheCurrentFile pins
// that a checkpoint written while a run holds the shared connection lands in
// the database the path names after a replacement, never in the stale one,
// and that the held event store refuses rather than writing to it.
func TestSessionCheckpointAfterDatabaseReplacementReachesTheCurrentFile(t *testing.T) {
	workspace := t.TempDir()
	events, err := NewEventStore(workspace, "run-replaced", "session-replaced")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = events.Close() }()
	if err := SaveSession(workspace, &SessionData{Rounds: 1}); err != nil {
		t.Fatal(err)
	}
	replacement := t.TempDir()
	if err := SaveSession(replacement, &SessionData{Rounds: 2}); err != nil {
		t.Fatal(err)
	}
	replaceRuntimeDatabase(t, workspace, replacement)

	if err := SaveSession(workspace, &SessionData{Rounds: 3}); err != nil {
		t.Fatalf("checkpoint after replacement: %v", err)
	}
	if err := events.Append(RunEvent{Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"stale"}`)}); !errors.Is(err, runtimestore.ErrReplaced) {
		t.Fatalf("append through the replaced connection = %v, want ErrReplaced", err)
	}
	// Read back through a new connection, as the next process would.
	if err := events.Close(); err != nil {
		t.Fatal(err)
	}
	if loaded := LoadSession(workspace); loaded == nil || loaded.Rounds != 3 {
		t.Fatalf("session at the path = %+v, want the checkpoint written after replacement", loaded)
	}
}
