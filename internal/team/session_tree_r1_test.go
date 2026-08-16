package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// Events appended through an EventStore bound to a branch must be stamped with
// that branch's ID so CLI-created branches collect their own events (R1).
func TestEventStore_BranchIDTagging(t *testing.T) {
	tempDir := t.TempDir()
	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer func() { _ = es.Close() }()

	es.SetBranchID("exp")

	_ = es.Append(RunEvent{Type: "run_started"})
	_ = es.Append(RunEvent{Type: "task_started", BranchID: "explicit-branch"})

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].BranchID != "exp" {
		t.Errorf("expected store-bound branch exp, got %q", events[0].BranchID)
	}
	if events[1].BranchID != "explicit-branch" {
		t.Errorf("explicit BranchID must not be overridden, got %q", events[1].BranchID)
	}
}

// initEventStore must bind the store to the active branch recorded in
// workspace/session_tree.json so a run after `hufu session checkout <branch>`
// writes its events into that branch's lineage (R1).
func TestCoordinatorInitEventStore_TagsActiveBranch(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()
	exp, err := st.CreateBranch("exp", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}
	st.ActiveBranch = exp.ID
	if err := SaveSessionTree(tempDir, st); err != nil {
		t.Fatalf("SaveSessionTree failed: %v", err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: tempDir,
			Config:    agent.TeamConfig{Name: "team-x"},
		},
		executionRunID: "run-x",
	}
	c.initEventStore()
	if c.eventStore == nil {
		t.Fatalf("expected event store to be initialized")
	}
	defer func() { _ = c.eventStore.Close() }()

	c.emitEvent("run_started", "coordinator", "", nil)

	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].BranchID != exp.ID {
		t.Errorf("expected event tagged with active branch %q, got %q", exp.ID, events[0].BranchID)
	}
}

func TestCoordinatorInitEventStore_FreshSessionUsesIndependentRootBranch(t *testing.T) {
	workspace := t.TempDir()
	st := NewSessionTree()
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}

	store, err := NewEventStore(workspace, "run-old", "session-old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type:    string(EventTaskCompleted),
		Actor:   "worker",
		TaskID:  "old-task",
		Payload: []byte(`{"id":"old-task","status":"done","agent":"worker","desc":"old task"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team-x"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.SetFreshSession(true)
	c.initEventStore()
	if c.eventStore == nil {
		t.Fatal("expected event store to be initialized")
	}
	defer c.eventStore.Close()

	if len(c.sessionData.Tasks) != 0 {
		t.Fatalf("fresh session replayed prior tasks: %#v", c.sessionData.Tasks)
	}
	updatedTree, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if updatedTree.ActiveBranch == "main" {
		t.Fatal("fresh session retained main event-store lineage")
	}
	branch := updatedTree.Branches[updatedTree.ActiveBranch]
	if branch == nil || branch.ParentID != "" || branch.ForkEventID != "" {
		t.Fatalf("fresh branch = %#v, want independent root", branch)
	}

	c.emitEvent("run_started", "coordinator", "", map[string]bool{"fresh": true})
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].BranchID; got != branch.ID {
		t.Fatalf("fresh event branch = %q, want %q", got, branch.ID)
	}
}
