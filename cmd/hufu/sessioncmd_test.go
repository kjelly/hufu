package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCLISession_Subcommands(t *testing.T) {
	tempDir := t.TempDir()

	// Initialize session tree and event store in temp workspace
	st := team.NewSessionTree()
	b1, err := st.CreateBranch("test-feature", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}
	if err := team.SaveSessionTree(tempDir, st); err != nil {
		t.Fatalf("SaveSessionTree failed: %v", err)
	}

	es, err := team.NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	_ = es.Append(team.RunEvent{ID: "evt-1", BranchID: "main", Type: "run_started"})
	_ = es.Append(team.RunEvent{ID: "evt-2", BranchID: b1.ID, Type: "task_started"})
	_ = es.Close()

	root := newRootCommand()

	// 1. Test session list
	outBuf := new(bytes.Buffer)
	root.SetOut(outBuf)
	root.SetArgs([]string{"session", "list", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session list failed: %v", err)
	}

	// 2. Test session tree
	root = newRootCommand()
	root.SetArgs([]string{"session", "tree", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session tree failed: %v", err)
	}

	// 3. Test session label
	root = newRootCommand()
	root.SetArgs([]string{"session", "label", "main", "v1.0", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session label failed: %v", err)
	}

	// 4. Test session fork
	root = newRootCommand()
	root.SetArgs([]string{"session", "fork", "main", "--name", "forked-via-cli", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session fork failed: %v", err)
	}

	// 5. Test session checkout
	root = newRootCommand()
	root.SetArgs([]string{"session", "checkout", "forked-via-cli", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session checkout failed: %v", err)
	}

	// 6. Test session diff
	root = newRootCommand()
	root.SetArgs([]string{"session", "diff", "main", "forked-via-cli", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("session diff failed: %v", err)
	}

	// Verify session tree saved correctly
	reloaded, err := team.LoadSessionTree(tempDir)
	if err != nil {
		t.Fatalf("LoadSessionTree failed: %v", err)
	}
	if reloaded.ActiveBranch != "forked-via-cli" {
		t.Errorf("expected active branch to be forked-via-cli, got %q", reloaded.ActiveBranch)
	}
	if reloaded.Labels["v1.0"] != "main" {
		t.Errorf("expected label v1.0 -> main, got %q", reloaded.Labels["v1.0"])
	}
}

func containsString(s, substr string) bool {
	return strings.Contains(s, substr) || filepath.Base(s) == substr
}

// TestCLISession_CheckoutRebuildsSession verifies that checkout rewrites
// session.json from the branch's event lineage so the next run resumes the
// checked-out branch's tasks.
func TestCLISession_CheckoutRebuildsSession(t *testing.T) {
	tempDir := t.TempDir()

	// Create main branch with 1 user message + 2 completed tasks via event store.
	es, err := team.NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	msgPayload, _ := json.Marshal(map[string]string{"role": "user", "content": "hello main"})
	_ = es.Append(team.RunEvent{ID: "evt-1", BranchID: "main", Type: "user_message_added", Payload: msgPayload})
	_ = es.Append(team.RunEvent{ID: "evt-2", BranchID: "main", Type: "task_completed", TaskID: "1", Payload: mustMarshal(map[string]string{"id": "1", "desc": "task one", "status": "done"})})
	_ = es.Append(team.RunEvent{ID: "evt-3", BranchID: "main", Type: "task_completed", TaskID: "2", Payload: mustMarshal(map[string]string{"id": "2", "desc": "task two", "status": "done"})})
	_ = es.Close()

	// Write session.json to simulate a live main run with tasks 1+2.
	liveSession := &team.SessionData{}
	liveSession.AddEntry("user", "hello main")
	liveSession.Tasks = []*team.TodoItem{{ID: "1", Desc: "task one", Status: team.TaskDone}, {ID: "2", Desc: "task two", Status: team.TaskDone}}
	if err := team.SaveSession(tempDir, liveSession); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	root := newRootCommand()

	// Fork exp via CLI (persists the branch to session_tree.json).
	root.SetOut(new(bytes.Buffer))
	root.SetArgs([]string{"session", "fork", "main", "--name", "exp", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("fork exp failed: %v", err)
	}

	// Simulate an exp run: append an exp-only task to the event store and
	// write session.json with exp's tasks.
	es, _ = team.OpenEventStore(tempDir)
	if es != nil {
		_ = es.Append(team.RunEvent{ID: "evt-3", BranchID: "exp", Type: "task_completed", TaskID: "3", Payload: mustMarshal(map[string]string{"id": "3", "desc": "exp task three", "status": "done"})})
		_ = es.Close()
	}
	expSession := &team.SessionData{}
	expSession.AddEntry("user", "hello exp")
	expSession.Tasks = []*team.TodoItem{{ID: "3", Desc: "exp task three", Status: team.TaskDone}}
	if err := team.SaveSession(tempDir, expSession); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Checkout main: session.json must be rebuilt to main lineage (tasks 1+2 only).
	root = newRootCommand()
	root.SetOut(new(bytes.Buffer))
	root.SetArgs([]string{"session", "checkout", "main", "--workspace", tempDir})
	if err := root.Execute(); err != nil {
		t.Fatalf("checkout main failed: %v", err)
	}

	sd := team.LoadSession(tempDir)
	if len(sd.Tasks) != 2 {
		t.Errorf("expected 2 tasks after checkout main, got %d: %+v", len(sd.Tasks), sd.Tasks)
	}
	if len(sd.Entries) != 1 || sd.Entries[0].Content != "hello main" {
		t.Errorf("expected main entries after checkout, got %+v", sd.Entries)
	}
}

func mustMarshal(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}
